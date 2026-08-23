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

	"family-photo-cloud/internal/database"
	"family-photo-cloud/internal/upload"

	"github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
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
	postgresDatabase := embeddedpostgres.NewDatabase(configuration)
	if err := postgresDatabase.Start(); err != nil {
		t.Fatalf("start embedded PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = postgresDatabase.Stop() })

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

	migrations, err := database.LoadMigrations(migrationDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	migrationConnection, err := pgx.Connect(ctx, configuration.GetConnectionURL())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Apply(ctx, migrationConnection, migrations, 0); err != nil {
		migrationConnection.Close(ctx)
		t.Fatalf("apply tracked migrations: %v", err)
	}
	if err := migrationConnection.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var migrationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrationCount); err != nil || migrationCount != len(migrations) {
		t.Fatalf("migration ledger count=%d err=%v want=%d", migrationCount, err, len(migrations))
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
	duplicate, duplicateCreated, err := repository.CreateSession(ctx, upload.CreateSessionInput{
		OwnerID: ownerID, ClientAssetID: "deduplicated-owner-quota", OriginalFilename: "same.heic",
		MediaType: "image/heic", ExpectedSize: int64(len(content)), ClientSHA256: hash,
		ExpiresAt: time.Now().Add(time.Hour), AvailableBytes: 1 << 40, MinimumFreeBytes: 1,
	})
	if err != nil || !duplicateCreated || duplicate.State != upload.StateCreated {
		t.Fatalf("duplicate content should not consume quota twice: session=%#v created=%v err=%v", duplicate, duplicateCreated, err)
	}
	if err := repository.MarkExpired(ctx, duplicate.ID); err != nil {
		t.Fatal(err)
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

	var reservationOwnerID string
	if err := pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, state)
        VALUES ('reservation@example.com', 'test-only-password-hash', 'active')
        RETURNING id::text`).Scan(&reservationOwnerID); err != nil {
		t.Fatal(err)
	}
	pendingHash := sha256.Sum256([]byte("reservation-bytes"))
	pending, _, err := repository.CreateSession(ctx, upload.CreateSessionInput{
		OwnerID: reservationOwnerID, ClientAssetID: "half-uploaded", OriginalFilename: "large.mov",
		MediaType: "video/quicktime", ExpectedSize: 100, ClientSHA256: pendingHash,
		ExpiresAt: time.Now().Add(time.Hour), AvailableBytes: 1 << 40, MinimumFreeBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ClaimTusCreation(ctx, pending.ID, reservationOwnerID, 100); err != nil {
		t.Fatal(err)
	}
	if err := repository.RecordProgress(ctx, pending.ID, 50); err != nil {
		t.Fatal(err)
	}
	_, created, err = repository.CreateSession(ctx, upload.CreateSessionInput{
		OwnerID: reservationOwnerID, ClientAssetID: "remaining-reservation", OriginalFilename: "second.mov",
		MediaType: "video/quicktime", ExpectedSize: 100, ClientSHA256: sha256.Sum256([]byte("second-reservation")),
		ExpiresAt: time.Now().Add(time.Hour), AvailableBytes: 160, MinimumFreeBytes: 10,
	})
	if err != nil || !created {
		t.Fatalf("reservation must use remaining persisted bytes: created=%v err=%v", created, err)
	}

	if err := repository.MarkReceived(ctx, pending.ID, 100); err != nil {
		t.Fatal(err)
	}
	claimed, err := repository.ClaimVerification(ctx, "integration-worker", time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != pending.ID {
		t.Fatalf("first verification claim=%#v err=%v", claimed, err)
	}
	claimed, err = repository.ClaimVerification(ctx, "second-worker", time.Minute, 10)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("active verification lease must prevent duplicate claim: %#v err=%v", claimed, err)
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

func migrationDirectory(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations")
}
