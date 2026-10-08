package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAccessTokenRoundTrip(t *testing.T) {
	t.Parallel()
	manager, err := NewAccessTokenManager([]byte(strings.Repeat("k", 32)), DefaultIssuer, DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	want := Principal{UserID: "user-a", SessionID: "session-a"}
	raw, err := manager.Issue(want, now, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := manager.Verify(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("principal mismatch: got %#v want %#v", got, want)
	}
}

func TestAccessTokenRejectsWrongAlgorithmAndAudience(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("k", 32))
	manager, err := NewAccessTokenManager(key, DefaultIssuer, DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claims := AccessClaims{
		UserID: "user-a", SessionID: "session-a",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: DefaultIssuer, Subject: "user-a",
			Audience:  jwt.ClaimStrings{"wrong-client"},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS384, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(raw); err == nil {
		t.Fatal("expected token rejection")
	}
}

func TestAccessTokenRequiresStrongKey(t *testing.T) {
	t.Parallel()
	if _, err := NewAccessTokenManager([]byte("short"), DefaultIssuer, DefaultAudience); err == nil {
		t.Fatal("expected short key to be rejected")
	}
}

func TestUploadCapabilityIsScopedAndNotAcceptedAsAccessToken(t *testing.T) {
	t.Parallel()
	manager, err := NewAccessTokenManager([]byte(strings.Repeat("k", 32)), DefaultIssuer, DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := manager.IssueUpload("user-a", "session-a", "upload-a", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := manager.VerifyUpload(raw)
	if err != nil || principal.UserID != "user-a" || principal.UploadID != "upload-a" || principal.SessionID != "session-a" {
		t.Fatalf("upload principal = %#v, err=%v", principal, err)
	}
	if _, err := manager.Verify(raw); err == nil {
		t.Fatal("upload capability was accepted as a general access token")
	}
}

func TestViewTicketIsScopedToOneAssetAndCannotActAsAccessToken(t *testing.T) {
	manager, err := NewAccessTokenManager([]byte(strings.Repeat("v", 32)), DefaultIssuer, DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{UserID: "user-1", SessionID: "session-1"}
	ticket, err := manager.IssueView(principal, "asset-1", time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := manager.VerifyView(ticket, "asset-1")
	if err != nil || got != principal {
		t.Fatalf("view ticket = %#v err=%v", got, err)
	}
	if _, err := manager.VerifyView(ticket, "asset-2"); err == nil {
		t.Fatal("view ticket accepted for a different asset")
	}
	if _, err := manager.Verify(ticket); err == nil {
		t.Fatal("view ticket accepted as an access token")
	}
	if _, err := manager.VerifyUpload(ticket); err == nil {
		t.Fatal("view ticket accepted as an upload capability")
	}
	access, err := manager.Issue(principal, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyView(access, "asset-1"); err == nil {
		t.Fatal("access token accepted as a view ticket")
	}
	expired, err := manager.IssueView(principal, "asset-1", time.Now().Add(-time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyView(expired, "asset-1"); err == nil {
		t.Fatal("expired view ticket accepted")
	}
}
