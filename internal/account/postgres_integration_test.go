//go:build integration

package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"family-photo-cloud/internal/database"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresMFAAndDurableThrottleLifecycle(t *testing.T) {
	pool, ctx := accountIntegrationPool(t)
	repository := NewPostgresRepository(pool)

	var userID string
	if err := pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, state)
        VALUES ('mfa-integration@example.com', 'test-only-password-hash', 'active')
        RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	identity := sha256.Sum256([]byte("login-identity"))
	for attempt := 0; attempt < 5; attempt++ {
		allowed, _, err := repository.RecordLoginAttempt(ctx, identity, now.Add(time.Duration(attempt)*time.Second), 10*time.Minute, 5)
		if err != nil || !allowed {
			t.Fatalf("login attempt %d: allowed=%v err=%v", attempt+1, allowed, err)
		}
	}
	allowed, retryAfter, err := repository.RecordLoginAttempt(ctx, identity, now.Add(6*time.Second), 10*time.Minute, 5)
	if err != nil || allowed || retryAfter <= 0 {
		t.Fatalf("sixth login attempt: allowed=%v retry=%s err=%v", allowed, retryAfter, err)
	}
	if err := repository.ClearLoginAttempts(ctx, identity); err != nil {
		t.Fatal(err)
	}

	preMFARefresh := sha256.Sum256([]byte("pre-mfa-refresh-token"))
	preMFASessionID, err := repository.CreateRefreshSession(ctx, userID, "Pre-MFA iPhone", preMFARefresh, now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SavePendingTOTP(ctx, userID, []byte("encrypted-test-secret"), make([]byte, 12)); err != nil {
		t.Fatal(err)
	}
	initialRecovery := [][32]byte{
		sha256.Sum256([]byte("initial-recovery-1")),
		sha256.Sum256([]byte("initial-recovery-2")),
	}
	if err := repository.ConfirmTOTP(ctx, userID, 100, initialRecovery); err != nil {
		t.Fatal(err)
	}
	active, err := repository.SessionActive(ctx, userID, preMFASessionID)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("confirming MFA left a pre-MFA refresh session active")
	}

	allowed, _, err = repository.RecordMFAActionAttempt(ctx, userID, "enroll", now, mfaActionWindow, mfaActionAttempts)
	if err != nil || !allowed {
		t.Fatalf("MFA enroll throttle: allowed=%v err=%v", allowed, err)
	}
	if err := repository.ClearMFAActionAttempts(ctx, userID, "enroll"); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < mfaActionAttempts; attempt++ {
		allowed, _, err := repository.RecordMFAActionAttempt(
			ctx, userID, "recovery", now.Add(time.Duration(attempt)*time.Second), mfaActionWindow, mfaActionAttempts,
		)
		if err != nil || !allowed {
			t.Fatalf("MFA recovery attempt %d: allowed=%v err=%v", attempt+1, allowed, err)
		}
	}
	allowed, retryAfter, err = repository.RecordMFAActionAttempt(ctx, userID, "recovery", now.Add(6*time.Second), mfaActionWindow, mfaActionAttempts)
	if err != nil || allowed || retryAfter <= 0 {
		t.Fatalf("sixth MFA recovery attempt: allowed=%v retry=%s err=%v", allowed, retryAfter, err)
	}
	if err := repository.ClearMFAActionAttempts(ctx, userID, "recovery"); err != nil {
		t.Fatal(err)
	}
	allowed, _, err = repository.RecordMFAActionAttempt(ctx, userID, "recovery", now.Add(7*time.Second), mfaActionWindow, mfaActionAttempts)
	if err != nil || !allowed {
		t.Fatalf("cleared MFA action throttle: allowed=%v err=%v", allowed, err)
	}
	if err := repository.ClearMFAActionAttempts(ctx, userID, "recovery"); err != nil {
		t.Fatal(err)
	}

	// Deliberately make the replacement violate UNIQUE(user_id, code_hash). The
	// TOTP counter update and code replacement must roll back together.
	duplicate := sha256.Sum256([]byte("duplicate-recovery"))
	if err := repository.RotateRecoveryCodes(ctx, userID, 101, [][32]byte{duplicate, duplicate}); err == nil {
		t.Fatal("duplicate recovery replacement unexpectedly succeeded")
	}
	var lastCounter int64
	if err := pool.QueryRow(ctx, `SELECT last_used_counter FROM user_mfa_totp WHERE user_id = $1::uuid`, userID).Scan(&lastCounter); err != nil {
		t.Fatal(err)
	}
	if lastCounter != 100 {
		t.Fatalf("failed recovery rotation consumed TOTP counter: got %d want 100", lastCounter)
	}
	var initialCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_mfa_recovery_codes WHERE user_id = $1::uuid AND used_at IS NULL`, userID).Scan(&initialCount); err != nil {
		t.Fatal(err)
	}
	if initialCount != len(initialRecovery) {
		t.Fatalf("failed recovery rotation changed codes: got %d want %d", initialCount, len(initialRecovery))
	}

	rotated := [][32]byte{
		sha256.Sum256([]byte("rotated-recovery-1")),
		sha256.Sum256([]byte("rotated-recovery-2")),
	}
	if err := repository.RotateRecoveryCodes(ctx, userID, 101, rotated); err != nil {
		t.Fatal(err)
	}
	if err := repository.RotateRecoveryCodes(ctx, userID, 101, rotated); !errors.Is(err, ErrMFAReplay) {
		t.Fatalf("replayed TOTP rotation error=%v want %v", err, ErrMFAReplay)
	}

	challengeHash := sha256.Sum256([]byte("recovery-challenge-one"))
	if err := repository.CreateMFAChallenge(ctx, userID, "Integration iPhone", challengeHash, now, now.Add(mfaChallengeTTL), mfaChallengeAttempts); err != nil {
		t.Fatal(err)
	}
	user, device, err := repository.CompleteMFARecoveryChallenge(ctx, challengeHash, rotated[0], now.Add(time.Minute))
	if err != nil || user.ID != userID || device != "Integration iPhone" {
		t.Fatalf("recovery challenge user=%#v device=%q err=%v", user, device, err)
	}
	secondChallenge := sha256.Sum256([]byte("recovery-challenge-two"))
	if err := repository.CreateMFAChallenge(ctx, userID, "Integration iPhone", secondChallenge, now.Add(2*time.Minute), now.Add(7*time.Minute), mfaChallengeAttempts); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.CompleteMFARecoveryChallenge(ctx, secondChallenge, rotated[0], now.Add(3*time.Minute)); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("used recovery code error=%v want %v", err, ErrMFAInvalid)
	}

	if err := repository.DisableMFA(ctx, userID, 102); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.TOTPForUser(ctx, userID); !errors.Is(err, ErrMFANotConfigured) {
		t.Fatalf("disabled MFA lookup error=%v want %v", err, ErrMFANotConfigured)
	}
}

func TestPostgresRefreshRotationRetryGraceIsBoundToRequestID(t *testing.T) {
	pool, ctx := accountIntegrationPool(t)
	repository := NewPostgresRepository(pool)

	var userID string
	if err := pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, state)
        VALUES ('refresh-retry@example.com', 'test-only-password-hash', 'active')
        RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	oldHash := sha256.Sum256([]byte("refresh-old"))
	newHash := sha256.Sum256([]byte("refresh-new"))
	requestA := sha256.Sum256([]byte("rotation-request-a"))
	requestB := sha256.Sum256([]byte("rotation-request-b"))
	if _, err := repository.CreateRefreshSession(ctx, userID, "Retry iPhone", oldHash, now.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	ciphertext := []byte("authenticated-ciphertext")
	nonce := []byte("123456789012")
	first, err := repository.RotateRefreshSession(ctx, oldHash, newHash, requestA, now.Add(30*24*time.Hour), ciphertext, nonce, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Retried || first.SessionID == "" {
		t.Fatalf("first rotation = %#v", first)
	}

	retryCandidate := sha256.Sum256([]byte("ignored-retry-candidate"))
	retried, err := repository.RotateRefreshSession(ctx, oldHash, retryCandidate, requestA, now.Add(30*24*time.Hour), []byte("ignored"), nonce, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !retried.Retried || retried.SessionID != first.SessionID {
		t.Fatalf("retry rotation = %#v, first=%#v", retried, first)
	}
	if string(retried.RetryTokenCiphertext) != string(ciphertext) || string(retried.RetryTokenNonce) != string(nonce) {
		t.Fatal("retry did not return the stored successor token ciphertext")
	}
	active, err := repository.SessionActive(ctx, userID, first.SessionID)
	if err != nil || !active {
		t.Fatalf("successor after idempotent retry: active=%v err=%v", active, err)
	}

	// Possession of the old token alone is not a grace-period bypass. A retry
	// with a different request ID is a real replay and revokes the live family.
	_, err = repository.RotateRefreshSession(ctx, oldHash, retryCandidate, requestB, now.Add(30*24*time.Hour), []byte("ignored"), nonce, now.Add(6*time.Second))
	if !errors.Is(err, ErrRefreshReplay) {
		t.Fatalf("mismatched retry request error=%v want %v", err, ErrRefreshReplay)
	}
	active, err = repository.SessionActive(ctx, userID, first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("mismatched retry request did not revoke the live refresh family")
	}
}

func TestPostgresRefreshRotationRetryGraceExpires(t *testing.T) {
	pool, ctx := accountIntegrationPool(t)
	repository := NewPostgresRepository(pool)

	var userID string
	if err := pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, state)
        VALUES ('refresh-expiry@example.com', 'test-only-password-hash', 'active')
        RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	oldHash := sha256.Sum256([]byte("refresh-expiry-old"))
	newHash := sha256.Sum256([]byte("refresh-expiry-new"))
	requestID := sha256.Sum256([]byte("rotation-request-expiry"))
	if _, err := repository.CreateRefreshSession(ctx, userID, "Expiry iPhone", oldHash, now.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	first, err := repository.RotateRefreshSession(ctx, oldHash, newHash, requestID, now.Add(30*24*time.Hour), []byte("authenticated-ciphertext"), []byte("123456789012"), now)
	if err != nil {
		t.Fatal(err)
	}

	retryCandidate := sha256.Sum256([]byte("ignored-after-expiry"))
	_, err = repository.RotateRefreshSession(ctx, oldHash, retryCandidate, requestID, now.Add(30*24*time.Hour), []byte("ignored"), []byte("123456789012"), now.Add(refreshRetryGrace+time.Second))
	if !errors.Is(err, ErrRefreshReplay) {
		t.Fatalf("expired retry grace error=%v want %v", err, ErrRefreshReplay)
	}
	active, err := repository.SessionActive(ctx, userID, first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("expired retry grace did not revoke the live refresh family")
	}
}

func TestPostgresMFAChallengeIssuanceLimit(t *testing.T) {
	pool, ctx := accountIntegrationPool(t)
	repository := NewPostgresRepository(pool)

	var userID string
	if err := pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, state)
        VALUES ('mfa-challenge-limit@example.com', 'test-only-password-hash', 'active')
        RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := repository.SavePendingTOTP(ctx, userID, []byte("encrypted-test-secret"), make([]byte, 12)); err != nil {
		t.Fatal(err)
	}
	if err := repository.ConfirmTOTP(ctx, userID, 100, [][32]byte{sha256.Sum256([]byte("recovery"))}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	for index := 0; index < mfaChallengeIssueLimit; index++ {
		hash := sha256.Sum256([]byte{byte(index), 0x7f})
		createdAt := now.Add(time.Duration(index) * time.Second)
		if err := repository.CreateMFAChallenge(ctx, userID, "Integration iPhone", hash, createdAt, createdAt.Add(mfaChallengeTTL), mfaChallengeAttempts); err != nil {
			t.Fatalf("challenge %d: %v", index+1, err)
		}
	}
	overLimit := sha256.Sum256([]byte("over-limit"))
	if err := repository.CreateMFAChallenge(ctx, userID, "Integration iPhone", overLimit, now.Add(time.Minute), now.Add(6*time.Minute), mfaChallengeAttempts); !errors.Is(err, ErrMFARateLimited) {
		t.Fatalf("challenge over limit error=%v want %v", err, ErrMFARateLimited)
	}
}

func TestPostgresDeviceRevokeRevokesRefreshFamilyAfterRotation(t *testing.T) {
	pool, ctx := accountIntegrationPool(t)
	repository := NewPostgresRepository(pool)

	var userID string
	if err := pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, state)
        VALUES ('device-family-revoke@example.com', 'test-only-password-hash', 'active')
        RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	parentHash := sha256.Sum256([]byte("device-family-parent"))
	parentID, err := repository.CreateRefreshSession(ctx, userID, "Family iPhone", parentHash, now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	childHash := sha256.Sum256([]byte("device-family-child"))
	rotation, err := repository.RotateRefreshSession(ctx, parentHash, childHash, [32]byte{}, now.Add(30*24*time.Hour), nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.RevokeDeviceSession(ctx, userID, parentID); err != nil {
		t.Fatal(err)
	}
	active, err := repository.SessionActive(ctx, userID, rotation.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("revoke of a rotated parent left the child session active")
	}
}

func accountIntegrationPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	port := accountAvailablePort(t)
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
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, configuration.GetConnectionURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	migrations, err := database.LoadMigrations(accountMigrationDirectory(t))
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
	return pool, ctx
}

func accountAvailablePort(t *testing.T) uint32 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return uint32(listener.Addr().(*net.TCPAddr).Port)
}

func accountMigrationDirectory(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations")
}
