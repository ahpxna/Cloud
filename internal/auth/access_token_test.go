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
	raw, err := manager.IssueUpload("user-a", "upload-a", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := manager.VerifyUpload(raw)
	if err != nil || principal.UserID != "user-a" || principal.UploadID != "upload-a" || principal.SessionID != "" {
		t.Fatalf("upload principal = %#v, err=%v", principal, err)
	}
	if _, err := manager.Verify(raw); err == nil {
		t.Fatal("upload capability was accepted as a general access token")
	}
}
