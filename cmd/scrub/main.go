// Command scrub re-reads committed originals, recalculates SHA-256, and records
// append-only integrity-check evidence. It never repairs or deletes data.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type assetRow struct {
	ID         string
	StorageKey string
	ByteSize   int64
	Expected   [32]byte
}

type result struct {
	AssetID        string `json:"asset_id"`
	StorageKey     string `json:"storage_key"`
	Result         string `json:"result"`
	ErrorCode      string `json:"error_code,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256"`
	ObservedSHA256 string `json:"observed_sha256,omitempty"`
}

type report struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Checked    int       `json:"checked"`
	Matched    int       `json:"matched"`
	Mismatched int       `json:"mismatched"`
	Missing    int       `json:"missing"`
	IOErrors   int       `json:"io_errors"`
	Results    []result  `json:"results,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scrub:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("scrub", flag.ContinueOnError)
	mediaRoot := flags.String("media-root", envString("PHOTO_MEDIA_ROOT", "/srv/media"), "media filesystem root")
	output := flags.String("output", "", "optional JSON report path")
	limit := flags.Int("limit", 0, "maximum assets to check; 0 means all")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *limit < 0 {
		return errors.New("limit cannot be negative")
	}
	root, err := filepath.Abs(*mediaRoot)
	if err != nil {
		return fmt.Errorf("resolve media root: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("configure PostgreSQL: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}

	started := time.Now().UTC()
	rows, err := pool.Query(ctx, `
        SELECT id::text, storage_key, byte_size, content_sha256
        FROM assets
        WHERE deleted_at IS NULL
        ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read asset inventory: %w", err)
	}
	defer rows.Close()

	rep := report{StartedAt: started}
	for rows.Next() {
		if *limit > 0 && rep.Checked >= *limit {
			break
		}
		var asset assetRow
		var expected []byte
		if err := rows.Scan(&asset.ID, &asset.StorageKey, &asset.ByteSize, &expected); err != nil {
			return fmt.Errorf("scan asset: %w", err)
		}
		if len(expected) != sha256.Size {
			return fmt.Errorf("asset %s has invalid stored SHA-256 length", asset.ID)
		}
		copy(asset.Expected[:], expected)
		item := checkAsset(ctx, root, asset)
		if err := recordCheck(ctx, pool, asset, item); err != nil {
			return fmt.Errorf("record integrity check for %s: %w", asset.ID, err)
		}
		rep.Checked++
		switch item.Result {
		case "match":
			rep.Matched++
		case "mismatch":
			rep.Mismatched++
		case "missing":
			rep.Missing++
		case "io_error":
			rep.IOErrors++
		}
		if item.Result != "match" {
			rep.Results = append(rep.Results, item)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rep.FinishedAt = time.Now().UTC()
	if *output != "" {
		if err := writeReport(*output, rep); err != nil {
			return err
		}
	}
	fmt.Printf("scrubbed %d assets: %d match, %d mismatch, %d missing, %d io_error\n",
		rep.Checked, rep.Matched, rep.Mismatched, rep.Missing, rep.IOErrors)
	if rep.Mismatched+rep.Missing+rep.IOErrors > 0 {
		return errors.New("integrity failures detected")
	}
	return nil
}

func checkAsset(ctx context.Context, root string, asset assetRow) result {
	out := result{
		AssetID: asset.ID, StorageKey: asset.StorageKey,
		ExpectedSHA256: hex.EncodeToString(asset.Expected[:]),
	}
	path, err := safePath(root, asset.StorageKey)
	if err != nil {
		out.Result, out.ErrorCode = "io_error", "invalid_storage_key"
		return out
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		out.Result, out.ErrorCode = "missing", "asset_missing"
		return out
	}
	if err != nil {
		out.Result, out.ErrorCode = "io_error", "asset_open_failed"
		return out
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		out.Result, out.ErrorCode = "io_error", "asset_stat_failed"
		return out
	}
	if info.Size() != asset.ByteSize {
		out.Result, out.ErrorCode = "mismatch", "asset_size_mismatch"
		return out
	}
	digest, err := hashFile(ctx, file)
	if err != nil {
		out.Result, out.ErrorCode = "io_error", "asset_hash_failed"
		return out
	}
	out.ObservedSHA256 = hex.EncodeToString(digest[:])
	if digest != asset.Expected {
		out.Result, out.ErrorCode = "mismatch", "asset_sha256_mismatch"
		return out
	}
	out.Result = "match"
	return out
}

func hashFile(ctx context.Context, reader io.Reader) ([32]byte, error) {
	hasher := sha256.New()
	buffer := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			if _, writeErr := hasher.Write(buffer[:n]); writeErr != nil {
				return [32]byte{}, writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return [32]byte{}, err
		}
	}
	var digest [32]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

func recordCheck(ctx context.Context, pool *pgxpool.Pool, asset assetRow, item result) error {
	var observed []byte
	if item.ObservedSHA256 != "" {
		decoded, err := hex.DecodeString(item.ObservedSHA256)
		if err != nil {
			return err
		}
		observed = decoded
	}
	_, err := pool.Exec(ctx, `
        INSERT INTO asset_integrity_checks (
            asset_id, expected_sha256, observed_sha256, result, error_code
        ) VALUES ($1::uuid, $2, $3, $4, NULLIF($5, ''))`,
		asset.ID, asset.Expected[:], observed, item.Result, item.ErrorCode)
	return err
}

func safePath(root, storageKey string) (string, error) {
	if storageKey == "" || filepath.IsAbs(storageKey) || strings.ContainsRune(storageKey, '\x00') {
		return "", errors.New("invalid storage key")
	}
	candidate := filepath.Join(root, filepath.FromSlash(storageKey))
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("storage key leaves media root")
	}
	return candidate, nil
}

func writeReport(path string, rep report) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(directory, ".scrub-*.json")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(directory)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
