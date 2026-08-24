// Command metrics-exporter exposes low-cardinality operational metrics from
// PostgreSQL and the media filesystem. It is intended for a private/loopback
// listener only; it never exposes filenames, tokens, EXIF, or owner IDs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type exporter struct {
	pool      *pgxpool.Pool
	mediaRoot string
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("configure PostgreSQL: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	exp := &exporter{pool: pool, mediaRoot: envString("PHOTO_MEDIA_ROOT", "/srv/media")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.HandleFunc("GET /readyz", exp.ready)
	mux.HandleFunc("GET /metrics", exp.metrics)
	server := &http.Server{
		Addr:              envString("METRICS_ADDR", "127.0.0.1:9090"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	signals, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-signals.Done():
		shutdown, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		return server.Shutdown(shutdown)
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (e *exporter) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := e.pool.Ping(ctx); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if _, err := availableBytes(e.mediaRoot); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{\"status\":\"ready\"}\n"))
}

func (e *exporter) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metrics, err := e.snapshot(ctx)
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte(metrics))
}

func (e *exporter) snapshot(ctx context.Context) (string, error) {
	var out strings.Builder
	out.WriteString("# HELP photo_cloud_upload_sessions Upload sessions by durable state.\n")
	out.WriteString("# TYPE photo_cloud_upload_sessions gauge\n")
	rows, err := e.pool.Query(ctx, `SELECT state, count(*) FROM upload_sessions GROUP BY state ORDER BY state`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return "", err
		}
		fmt.Fprintf(&out, "photo_cloud_upload_sessions{state=%s} %d\n", prometheusQuote(state), count)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()

	scalarQueries := []struct {
		name, help, query string
	}{
		{"photo_cloud_assets_total", "Visible immutable assets.", `SELECT count(*) FROM assets WHERE deleted_at IS NULL`},
		{"photo_cloud_verification_backlog", "Uploads waiting in a verifier-owned state.", `SELECT count(*) FROM upload_sessions WHERE state IN ('received','verifying','verified','committing','quarantining')`},
		{"photo_cloud_signed_manifests_total", "Recorded signed integrity manifests.", `SELECT count(*) FROM signed_manifests`},
		{"photo_cloud_integrity_checks_total", "Append-only integrity check evidence rows.", `SELECT count(*) FROM asset_integrity_checks`},
		{"photo_cloud_upload_events_last_sequence", "Largest append-only upload event sequence observed.", `SELECT COALESCE(max(sequence_id),0) FROM upload_events`},
	}
	for _, metric := range scalarQueries {
		var value int64
		if err := e.pool.QueryRow(ctx, metric.query).Scan(&value); err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", metric.name, metric.help, metric.name, metric.name, value)
	}

	out.WriteString("# HELP photo_cloud_integrity_latest_assets Current integrity state by latest check for each asset.\n")
	out.WriteString("# TYPE photo_cloud_integrity_latest_assets gauge\n")
	rows, err = e.pool.Query(ctx, `
        WITH latest AS (
            SELECT DISTINCT ON (asset_id) asset_id, result
            FROM asset_integrity_checks
            ORDER BY asset_id, checked_at DESC, id DESC
        )
        SELECT result, count(*) FROM latest GROUP BY result ORDER BY result`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var result string
		var count int64
		if err := rows.Scan(&result, &count); err != nil {
			rows.Close()
			return "", err
		}
		fmt.Fprintf(&out, "photo_cloud_integrity_latest_assets{result=%s} %d\n", prometheusQuote(result), count)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()

	free, err := availableBytes(e.mediaRoot)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&out, "# HELP photo_cloud_storage_available_bytes Bytes available to the unprivileged media process.\n# TYPE photo_cloud_storage_available_bytes gauge\nphoto_cloud_storage_available_bytes %d\n", free)

	var oldestSeconds float64
	if err := e.pool.QueryRow(ctx, `
        SELECT COALESCE(EXTRACT(EPOCH FROM (now() - min(updated_at))), 0)::double precision
        FROM upload_sessions
        WHERE state IN ('received','verifying','verified','committing','quarantining')`).Scan(&oldestSeconds); err != nil {
		return "", err
	}
	fmt.Fprintf(&out, "# HELP photo_cloud_verification_oldest_age_seconds Age of the oldest verification/commit backlog item.\n# TYPE photo_cloud_verification_oldest_age_seconds gauge\nphoto_cloud_verification_oldest_age_seconds %s\n", strconv.FormatFloat(oldestSeconds, 'f', 3, 64))
	return out.String(), nil
}

func prometheusQuote(value string) string {
	return strconv.Quote(value)
}

func availableBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
