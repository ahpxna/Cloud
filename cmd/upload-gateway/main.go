package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"family-photo-cloud/internal/account"
	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/gateway"
	"family-photo-cloud/internal/upload"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("upload gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	key, err := decodeSecretEnv("ACCESS_TOKEN_HMAC_KEY_BASE64")
	if err != nil {
		return err
	}
	loginThrottleKey, err := decodeSecretEnv("LOGIN_THROTTLE_HMAC_KEY_BASE64")
	if err != nil {
		return err
	}
	mfaEncryptionKey, err := decodeSecretEnvExact("MFA_ENCRYPTION_KEY_BASE64", 32)
	if err != nil {
		return err
	}
	refreshRetryEncryptionKey, err := decodeSecretEnvExact("REFRESH_RETRY_ENCRYPTION_KEY_BASE64", 32)
	if err != nil {
		return err
	}
	tokens, err := auth.NewAccessTokenManager(key, auth.DefaultIssuer, auth.DefaultAudience)
	if err != nil {
		return err
	}

	startupContext, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()
	databaseURL := os.Getenv("DATABASE_URL")
	pool, err := pgxpool.New(startupContext, databaseURL)
	if err != nil {
		return fmt.Errorf("configure PostgreSQL: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(startupContext); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	gatewayLease, err := acquireSingleGatewayLease(startupContext, pool)
	if err != nil {
		return err
	}
	defer gatewayLease.Release()

	maxUploadBytes, err := envInt64("TUS_MAX_UPLOAD_BYTES", 20<<30)
	if err != nil {
		return err
	}
	chunkBytes, err := envInt64("TUS_MVP_CHUNK_BYTES", 32<<20)
	if err != nil {
		return err
	}
	verificationJobs, err := envInt("VERIFICATION_WORKERS", 2)
	if err != nil {
		return err
	}
	maxConcurrentPatches, err := envInt("MAX_CONCURRENT_PATCHES", 6)
	if err != nil {
		return err
	}
	maxPatchesPerUser, err := envInt("MAX_PATCHES_PER_USER", 2)
	if err != nil {
		return err
	}
	minimumFreeBytes, err := envInt64("PHOTO_MIN_FREE_BYTES", 200<<30)
	if err != nil {
		return err
	}
	reconcileInterval, err := envDurationStrict("RECONCILE_INTERVAL", time.Minute)
	if err != nil {
		return err
	}
	verificationLease, err := envDurationStrict("VERIFICATION_LEASE", 10*time.Minute)
	if err != nil {
		return err
	}
	maxActiveUploadSessions, err := envInt("MAX_ACTIVE_UPLOAD_SESSIONS", 200)
	if err != nil {
		return err
	}
	uploadCreateWindow, err := envDurationStrict("UPLOAD_SESSION_CREATE_WINDOW", time.Minute)
	if err != nil {
		return err
	}
	maxUploadCreatesPerWindow, err := envInt("MAX_UPLOAD_SESSION_CREATES_PER_WINDOW", 30)
	if err != nil {
		return err
	}
	globalLoginRate, err := envFloat("GLOBAL_LOGIN_RATE_PER_SECOND", 1)
	if err != nil {
		return err
	}
	globalLoginBurst, err := envInt("GLOBAL_LOGIN_BURST", 20)
	if err != nil {
		return err
	}
	repository := upload.NewPostgresRepository(pool)
	application, err := gateway.New(gateway.Config{
		Repository:                repository,
		Accounts:                  account.NewPostgresRepository(pool),
		Tokens:                    tokens,
		MediaRoot:                 envString("PHOTO_MEDIA_ROOT", "/srv/media"),
		MaxUploadBytes:            maxUploadBytes,
		ChunkBytes:                chunkBytes,
		VerificationJobs:          verificationJobs,
		MaxConcurrentPatches:      maxConcurrentPatches,
		MaxPatchesPerUser:         maxPatchesPerUser,
		MinimumFreeBytes:          minimumFreeBytes,
		ReconcileInterval:         reconcileInterval,
		VerificationLease:         verificationLease,
		MaxActiveUploadSessions:   maxActiveUploadSessions,
		UploadSessionCreateWindow: uploadCreateWindow,
		MaxUploadCreatesPerWindow: maxUploadCreatesPerWindow,
		LoginThrottleHMACKey:      loginThrottleKey,
		GlobalLoginRatePerSecond:  globalLoginRate,
		GlobalLoginBurst:          globalLoginBurst,
		MFAEncryptionKey:          mfaEncryptionKey,
		RefreshRetryEncryptionKey: refreshRetryEncryptionKey,
		Logger:                    logger,
	})
	if err != nil {
		return err
	}
	defer application.Close()

	readTimeout, err := envDurationStrict("HTTP_READ_TIMEOUT", 20*time.Minute)
	if err != nil {
		return err
	}
	writeTimeout, err := envDurationStrict("HTTP_WRITE_TIMEOUT", 30*time.Minute)
	if err != nil {
		return err
	}
	idleTimeout, err := envDurationStrict("HTTP_IDLE_TIMEOUT", 2*time.Minute)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              envString("HTTP_ADDR", "127.0.0.1:8080"),
		Handler:           application,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    64 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("upload gateway listening", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	signals, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	select {
	case <-signals.Done():
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

const singleGatewayAdvisoryLock int64 = 7042026082401

func acquireSingleGatewayLease(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve gateway database connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, singleGatewayAdvisoryLock).Scan(&acquired); err != nil {
		conn.Release()
		return nil, fmt.Errorf("acquire single-gateway writer lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, errors.New("another upload gateway already holds the writable-media lease")
	}
	return conn, nil
}

func decodeSecretEnv(name string) ([]byte, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be standard base64", name)
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("decoded %s must contain at least 32 bytes", name)
	}
	return key, nil
}

func decodeSecretEnvExact(name string, size int) ([]byte, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be standard base64", name)
	}
	if len(key) != size {
		return nil, fmt.Errorf("decoded %s must contain exactly %d bytes", name, size)
	}
	return key, nil
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt64(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value, err := envInt64(name, int64(fallback))
	return int(value), err
}

func envFloat(name string, fallback float64) (float64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", name)
	}
	return parsed, nil
}

func envDurationStrict(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return parsed, nil
}
