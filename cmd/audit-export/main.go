// Command audit-export writes the append-only upload event stream as JSONL plus
// a SHA-256 sidecar. It is intentionally read-only and suitable for copying to
// protected off-host evidence storage.
package main

import (
	"bufio"
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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type event struct {
	SequenceID      int64           `json:"sequence_id"`
	UploadSessionID string          `json:"upload_session_id"`
	OwnerID         string          `json:"owner_id"`
	EventType       string          `json:"event_type"`
	OffsetFrom      *int64          `json:"offset_from,omitempty"`
	OffsetTo        *int64          `json:"offset_to,omitempty"`
	Attempt         *int            `json:"attempt,omitempty"`
	ErrorCode       *string         `json:"error_code,omitempty"`
	RequestID       *string         `json:"request_id,omitempty"`
	Details         json.RawMessage `json:"details"`
	OccurredAt      time.Time       `json:"occurred_at"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "audit-export:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("audit-export", flag.ContinueOnError)
	output := flags.String("output", "", "new JSONL output path")
	after := flags.Int64("after-sequence", 0, "export sequence IDs strictly greater than this value")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *output == "" || *after < 0 {
		return errors.New("usage: audit-export -output /protected/path/events.jsonl [-after-sequence N]")
	}

	directory := filepath.Dir(*output)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	sidecar := *output + ".sha256"
	if _, err := os.Stat(*output); err == nil {
		if _, sideErr := os.Stat(sidecar); sideErr == nil {
			return fmt.Errorf("refusing to replace existing export %s", *output)
		} else if !errors.Is(sideErr, os.ErrNotExist) {
			return sideErr
		}
		// Crash recovery: the JSONL hard-link may have reached disk before its
		// checksum sidecar. Repair only the missing sidecar from immutable bytes;
		// this path deliberately does not require PostgreSQL to be online.
		digest, err := fileSHA256(*output)
		if err != nil {
			return err
		}
		if err := publishSidecarNoReplace(directory, sidecar, digest, filepath.Base(*output)); err != nil {
			return err
		}
		if err := syncDirectory(directory); err != nil {
			return err
		}
		fmt.Printf("repaired missing checksum sidecar for %s\n", *output)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(sidecar); err == nil {
		return fmt.Errorf("refusing export because checksum sidecar already exists without JSONL: %s", sidecar)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("configure PostgreSQL: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}

	temp, err := os.CreateTemp(directory, ".audit-export-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}

	hasher := sha256.New()
	writer := bufio.NewWriter(io.MultiWriter(temp, hasher))
	rows, err := pool.Query(ctx, `
        SELECT sequence_id, upload_session_id::text, owner_id::text, event_type,
               offset_from, offset_to, attempt, error_code, request_id,
               details, occurred_at
        FROM upload_events
        WHERE sequence_id > $1
        ORDER BY sequence_id`, *after)
	if err != nil {
		temp.Close()
		return err
	}
	count := 0
	last := *after
	for rows.Next() {
		var item event
		if err := rows.Scan(&item.SequenceID, &item.UploadSessionID, &item.OwnerID, &item.EventType,
			&item.OffsetFrom, &item.OffsetTo, &item.Attempt, &item.ErrorCode,
			&item.RequestID, &item.Details, &item.OccurredAt); err != nil {
			rows.Close()
			temp.Close()
			return err
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			rows.Close()
			temp.Close()
			return err
		}
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			rows.Close()
			temp.Close()
			return err
		}
		count++
		last = item.SequenceID
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		temp.Close()
		return err
	}
	rows.Close()
	if err := writer.Flush(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}

	if err := os.Link(tempPath, *output); err != nil {
		return fmt.Errorf("publish audit JSONL without replacement: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("persist audit JSONL directory entry: %w", err)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if err := publishSidecarNoReplace(directory, sidecar, digest, filepath.Base(*output)); err != nil {
		return err
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("persist audit checksum directory entry: %w", err)
	}
	fmt.Printf("exported %d events through sequence %d to %s\n", count, last, *output)
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func publishSidecarNoReplace(directory, sidecar, digest, basename string) error {
	temp, err := os.CreateTemp(directory, ".audit-sha256-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := fmt.Fprintf(temp, "%s  %s\n", digest, basename); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(tempPath, sidecar); err != nil {
		return fmt.Errorf("publish checksum sidecar without replacement: %w", err)
	}
	return nil
}

func syncDirectory(directory string) error {
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return err
	}
	return dir.Close()
}
