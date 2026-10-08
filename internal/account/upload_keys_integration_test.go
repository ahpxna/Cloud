//go:build integration

package account

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPostgresUploadKeyLifecycle(t *testing.T) {
	pool, ctx := accountIntegrationPool(t)
	repository := NewPostgresRepository(pool)
	admin := NewAdminRepository(pool)
	passwordHash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	userID, err := admin.CreateUser(ctx, "mom@example.com", passwordHash, "member")
	if err != nil {
		t.Fatal(err)
	}

	raw, hash, err := newUploadKey()
	if err != nil {
		t.Fatal(err)
	}
	if parsed, ok := UploadKeyHash(raw); !ok || parsed != hash {
		t.Fatal("generated key does not parse back to its hash")
	}
	key, err := repository.CreateUploadKey(ctx, userID, "iPhone của mẹ", hash)
	if err != nil {
		t.Fatal(err)
	}
	owner, keyID, err := repository.UploadKeyOwner(ctx, hash, time.Now().UTC())
	if err != nil || owner != userID || keyID != key.ID {
		t.Fatalf("owner lookup = %q %q err=%v", owner, keyID, err)
	}
	keys, err := repository.ListUploadKeys(ctx, userID)
	if err != nil || len(keys) != 1 || keys[0].Name != "iPhone của mẹ" || keys[0].LastUsedAt == nil {
		t.Fatalf("listed keys = %#v err=%v", keys, err)
	}

	for index := 1; index < maxActiveUploadKeys; index++ {
		_, extraHash, err := newUploadKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.CreateUploadKey(ctx, userID, fmt.Sprintf("extra %d", index), extraHash); err != nil {
			t.Fatal(err)
		}
	}
	_, overflow, err := newUploadKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateUploadKey(ctx, userID, "one too many", overflow); !errors.Is(err, ErrUploadKeyLimit) {
		t.Fatalf("eleventh key error = %v", err)
	}

	if err := repository.RevokeUploadKey(ctx, userID, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.UploadKeyOwner(ctx, hash, time.Now().UTC()); !errors.Is(err, ErrUploadKeyInvalid) {
		t.Fatalf("revoked key lookup error = %v", err)
	}
	if err := repository.RevokeUploadKey(ctx, userID, key.ID); !errors.Is(err, ErrUploadKeyInvalid) {
		t.Fatalf("second revoke error = %v", err)
	}

	// Revoking frees a slot: nine active keys remain, so a tenth is allowed.
	_, replacement, err := newUploadKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateUploadKey(ctx, userID, "replacement", replacement); err != nil {
		t.Fatal(err)
	}

	// Operator revocation (lost phone, password reset, disable) kills every key.
	if _, err := admin.RevokeSessions(ctx, "mom@example.com"); err != nil {
		t.Fatal(err)
	}
	if keys, err := repository.ListUploadKeys(ctx, userID); err != nil || len(keys) != 0 {
		t.Fatalf("keys after revoke-sessions = %#v err=%v", keys, err)
	}
	if _, _, err := repository.UploadKeyOwner(ctx, replacement, time.Now().UTC()); !errors.Is(err, ErrUploadKeyInvalid) {
		t.Fatalf("key after revoke-sessions error = %v", err)
	}
	_, live, err := newUploadKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateUploadKey(ctx, userID, "new phone", live); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Disable(ctx, "mom@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.UploadKeyOwner(ctx, live, time.Now().UTC()); !errors.Is(err, ErrUploadKeyInvalid) {
		t.Fatalf("key of a disabled account error = %v", err)
	}
}
