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

	directory := filepath.Dir(*output)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
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
	var last int64 = *after
	for rows.Next() {
		var item event
		if err := rows.Scan(
			&item.SequenceID, &item.UploadSessionID, &item.OwnerID, &item.EventType,
			&item.OffsetFrom, &item.OffsetTo, &item.Attempt, &item.ErrorCode,
			&item.RequestID, &item.Details, &item.OccurredAt,
		); err != nil {
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
	if _, err := os.Stat(*output); err == nil {
		return fmt.Errorf("refusing to replace existing export %s", *output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(tempPath, *output); err != nil {
		return err
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	sidecar := *output + ".sha256"
	sidecarContent := []byte(fmt.Sprintf("%s  %s\n", digest, filepath.Base(*output)))
	if err := os.WriteFile(sidecar, sidecarContent, 0o600); err != nil {
		_ = os.Remove(*output)
		return err
	}
	dir, err := os.Open(directory)
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	fmt.Printf("exported %d events through sequence %d to %s\n", count, last, *output)
	return nil
}
