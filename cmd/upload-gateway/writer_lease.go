package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"
)

const singleGatewayAdvisoryLock int64 = 7042026082401

type writerLease struct {
	db     *pgxpool.Conn
	file   *os.File
	cancel context.CancelFunc
	lost   chan error
	once   sync.Once
}

func acquireWriterLease(ctx context.Context, pool *pgxpool.Pool, mediaRoot string) (*writerLease, error) {
	if err := os.MkdirAll(mediaRoot, 0o750); err != nil {
		return nil, fmt.Errorf("prepare media root for writer fence: %w", err)
	}
	lockPath := filepath.Join(mediaRoot, ".photo-cloud-writer.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open media writer fence: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.New("another upload gateway already holds the writable-media filesystem lease")
		}
		return nil, fmt.Errorf("acquire media writer fence: %w", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("reserve gateway database connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, singleGatewayAdvisoryLock).Scan(&acquired); err != nil {
		conn.Release()
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("acquire single-gateway database lock: %w", err)
	}
	if !acquired {
		conn.Release()
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		file.Close()
		return nil, errors.New("another upload gateway already holds the writable-media database lease")
	}

	monitorCtx, cancel := context.WithCancel(context.Background())
	lease := &writerLease{db: conn, file: file, cancel: cancel, lost: make(chan error, 1)}
	go lease.monitor(monitorCtx)
	return lease, nil
}

func (lease *writerLease) monitor(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			var one int
			err := lease.db.QueryRow(pingCtx, `SELECT 1`).Scan(&one)
			cancel()
			if err != nil || one != 1 {
				if err == nil {
					err = errors.New("gateway database lease heartbeat returned unexpected result")
				}
				select {
				case lease.lost <- fmt.Errorf("single-writer database lease lost: %w", err):
				default:
				}
				return
			}
		}
	}
}

func (lease *writerLease) Lost() <-chan error { return lease.lost }

func (lease *writerLease) Close() {
	lease.once.Do(func() {
		lease.cancel()
		if lease.db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, _ = lease.db.Exec(ctx, `SELECT pg_advisory_unlock($1)`, singleGatewayAdvisoryLock)
			cancel()
			lease.db.Release()
		}
		if lease.file != nil {
			_ = unix.Flock(int(lease.file.Fd()), unix.LOCK_UN)
			_ = lease.file.Close()
		}
	})
}
