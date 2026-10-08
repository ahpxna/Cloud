//go:build integration

package account

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestPostgresAdminAccountLifecycle(t *testing.T) {
	pool, ctx := accountIntegrationPool(t)
	admin := NewAdminRepository(pool)
	sessions := NewPostgresRepository(pool)

	passwordHash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	userID, err := admin.CreateUser(ctx, " Parent@Example.com ", passwordHash, "member")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	signIn := func(label string) (string, [32]byte) {
		t.Helper()
		token := sha256.Sum256([]byte(label + now.String()))
		sessionID, err := sessions.CreateRefreshSession(ctx, userID, label, token, now.Add(30*24*time.Hour), nil)
		if err != nil {
			t.Fatal(err)
		}
		return sessionID, token
	}
	requireActive := func(sessionID string, want bool) {
		t.Helper()
		active, err := sessions.SessionActive(ctx, userID, sessionID)
		if err != nil || active != want {
			t.Fatalf("session %s active=%v err=%v, want %v", sessionID, active, err, want)
		}
	}

	phone, phoneToken := signIn("Parent iPhone")
	requireActive(phone, true)
	accounts, err := admin.ListUsers(ctx)
	if err != nil || len(accounts) != 1 || accounts[0].Email != "parent@example.com" || accounts[0].ActiveDevices != 1 || accounts[0].MFAEnabled {
		t.Fatalf("list users = %#v err=%v", accounts, err)
	}

	revoked, err := admin.Disable(ctx, "PARENT@example.com")
	if err != nil || revoked != 1 {
		t.Fatalf("disable revoked=%d err=%v", revoked, err)
	}
	requireActive(phone, false)
	if _, err := sessions.ActiveUserByEmail(ctx, "parent@example.com"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled account can still sign in: %v", err)
	}
	if _, err := sessions.RotateRefreshSession(ctx, phoneToken, sha256.Sum256([]byte("next")), [32]byte{}, now.Add(time.Hour), nil, nil, now); err == nil {
		t.Fatal("disabled account refreshed a session")
	}

	if err := admin.Enable(ctx, "parent@example.com"); err != nil {
		t.Fatal(err)
	}
	requireActive(phone, false) // devices revoked by disable stay revoked
	tablet, _ := signIn("Parent iPad")
	requireActive(tablet, true)

	newHash, err := HashPassword("a completely different passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if revoked, err := admin.ResetPassword(ctx, "parent@example.com", newHash); err != nil || revoked != 1 {
		t.Fatalf("reset password revoked=%d err=%v", revoked, err)
	}
	requireActive(tablet, false)
	user, err := sessions.ActiveUserByEmail(ctx, "parent@example.com")
	if err != nil || user.PasswordHash != newHash {
		t.Fatalf("password was not replaced: err=%v", err)
	}

	laptop, _ := signIn("Parent laptop")
	if _, err := pool.Exec(ctx, `
        INSERT INTO user_mfa_totp (user_id, encrypted_secret, nonce, confirmed_at)
        VALUES ($1::uuid, '\x00', '\x000000000000000000000000', now())`, userID); err != nil {
		t.Fatal(err)
	}
	if revoked, err := admin.ResetMFA(ctx, "parent@example.com"); err != nil || revoked != 1 {
		t.Fatalf("reset MFA revoked=%d err=%v", revoked, err)
	}
	requireActive(laptop, false)
	var mfaRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_mfa_totp WHERE user_id = $1::uuid`, userID).Scan(&mfaRows); err != nil || mfaRows != 0 {
		t.Fatalf("MFA rows after reset = %d err=%v", mfaRows, err)
	}

	desktop, _ := signIn("Parent desktop")
	if revoked, err := admin.MarkDeleted(ctx, "parent@example.com"); err != nil || revoked != 1 {
		t.Fatalf("delete revoked=%d err=%v", revoked, err)
	}
	requireActive(desktop, false)
	if err := admin.Enable(ctx, "parent@example.com"); err == nil {
		t.Fatal("a deleting account was re-enabled")
	}
	if _, err := admin.Disable(ctx, "missing@example.com"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("unknown account error = %v", err)
	}
}
