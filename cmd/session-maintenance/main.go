package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "session-maintenance:", err)
		os.Exit(1)
	}
}

func run() error {
	retentionDays := 180
	if raw := os.Getenv("SESSION_HISTORY_RETENTION_DAYS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 30 {
			return fmt.Errorf("SESSION_HISTORY_RETENTION_DAYS must be an integer >= 30")
		}
		retentionDays = parsed
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

	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	generations, err := tx.Exec(ctx, `
        DELETE FROM user_sessions
        WHERE (revoked_at IS NOT NULL OR expires_at <= now())
          AND GREATEST(COALESCE(revoked_at, expires_at), expires_at) < $1`, cutoff)
	if err != nil {
		return fmt.Errorf("prune refresh generations: %w", err)
	}
	devices, err := tx.Exec(ctx, `
        DELETE FROM device_sessions AS device
        WHERE (device.revoked_at IS NOT NULL OR device.expires_at <= now())
          AND GREATEST(COALESCE(device.revoked_at, device.expires_at), device.expires_at) < $1
          AND NOT EXISTS (
              SELECT 1 FROM user_sessions AS generation
              WHERE generation.device_session_id = device.id
          )`, cutoff)
	if err != nil {
		return fmt.Errorf("prune device sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("pruned %d refresh generations and %d device sessions older than %d days\n", generations.RowsAffected(), devices.RowsAffected(), retentionDays)
	return nil
}
