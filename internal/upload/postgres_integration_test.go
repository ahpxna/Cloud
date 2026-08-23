//go:build integration

package upload_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"family-photo-cloud/internal/upload"

	"github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresVerifiedCommitAndLibraryRead(t *testing.T) {
	port := availablePort(t)
	root := t.TempDir()
	configuration := embeddedpostgres.DefaultConfig().
		Port(port).
		Database("photo_cloud").
		Username("photo_cloud").
		Password("test-only-password").
		RuntimePath(filepath.Join(root, "runtime")).
		DataPath(filepath.Join(root, "data")).
		CachePath(filepath.Join(root, "cache")).
		Logger(io.Discard)
	database := embeddedpostgres.NewDatabase(configuration)
	if err := database.Start(); err != nil {
		t.Fatalf("start embedded PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = database.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, configuration.GetConnectionURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	for _, migrationPath := range migrationPaths(t) {
		migration, err := os.ReadFile(migrationPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply migration %s: %v", migrationPath, err)
		}
	}

	var ownerID string
	if err := pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, state)
        VALUES ('family@example.com', 'test-only-password-hash', 'active')
        RETURNING id::text`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}

	content := []byte("verified original bytes stored by PostgreSQL integration test")
	hash := sha256.Sum256(content)
	repository := upload.NewPostgresRepository(pool)
	session, created, err := repository.CreateSession(ctx, upload.CreateSessionInput{
		OwnerID:          ownerID,
		ClientAssetID:    "photo-local-id-1",
		OriginalFilename: "IMG_0001.HEIC",
		MediaType:        "image/heic",
		ExpectedSize:     int64(len(content)),
		ClientSHA256:     hash,
		ExpiresAt:        time.Now().Add(time.Hour),
		AvailableBytes:   1 << 40,
		MinimumFreeBytes: 1,
	})
	if err != nil || !created {
		t.Fatalf("create upload session: created=%v err=%v", created, err)
	}
	if err := repository.ClaimTusCreation(ctx, session.ID, ownerID, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	processor, err := upload.NewProcessor(repository, filepath.Join(root, "media"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), session.ID), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkReceived(ctx, session.ID, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if err := processor.Process(ctx, session.ID); err != nil {
		t.Fatal(err)
	}

	committed, err := repository.SessionByID(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.State != upload.StateAvailable || committed.ServerSHA256 == nil || *committed.ServerSHA256 != hash {
		t.Fatalf("unexpected committed session: %#v", committed)
	}
	assets, err := repository.ListAssets(ctx, ownerID, nil, 10)
	if err != nil || len(assets) != 1 {
		t.Fatalf("list assets: assets=%#v err=%v", assets, err)
	}
	if assets[0].ContentSHA256 != hash || assets[0].ByteSize != int64(len(content)) {
		t.Fatalf("asset integrity metadata mismatch: %#v", assets[0])
	}

	if _, err := pool.Exec(ctx, `UPDATE upload_events SET event_type = 'failed'`); err == nil {
		t.Fatal("append-only upload_events trigger allowed mutation")
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET quota_bytes = $2 WHERE id = $1::uuid`, ownerID, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	_, _, err = repository.CreateSession(ctx, upload.CreateSessionInput{
		OwnerID: ownerID, ClientAssetID: "over-owner-quota", OriginalFilename: "next.jpg",
		MediaType: "image/jpeg", ExpectedSize: 1, ClientSHA256: sha256.Sum256([]byte("x")),
		ExpiresAt: time.Now().Add(time.Hour), AvailableBytes: 1 << 40, MinimumFreeBytes: 1,
	})
	if !errors.Is(err, upload.ErrInsufficientStorage) {
		t.Fatalf("owner quota error = %v, want insufficient storage", err)
	}

	var secondOwnerID string
	if err := pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, state)
        VALUES ('second@example.com', 'test-only-password-hash', 'active')
        RETURNING id::text`).Scan(&secondOwnerID); err != nil {
		t.Fatal(err)
	}
	_, _, err = repository.CreateSession(ctx, upload.CreateSessionInput{
		OwnerID: secondOwnerID, ClientAssetID: "below-free-floor", OriginalFilename: "next.jpg",
		MediaType: "image/jpeg", ExpectedSize: 2, ClientSHA256: sha256.Sum256([]byte("xy")),
		ExpiresAt: time.Now().Add(time.Hour), AvailableBytes: 10, MinimumFreeBytes: 10,
	})
	if !errors.Is(err, upload.ErrInsufficientStorage) {
		t.Fatalf("free space reserve error = %v, want insufficient storage", err)
	}
}

func availablePort(t *testing.T) uint32 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return uint32(listener.Addr().(*net.TCPAddr).Port)
}

func migrationPaths(t *testing.T) []string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	directory := filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations")
	paths, err := filepath.Glob(filepath.Join(directory, "*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("find migrations: %v", err)
	}
	return paths
}
