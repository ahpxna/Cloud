package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	DefaultIssuer   = "family-photo-cloud"
	DefaultAudience = "family-photo-cloud-ios"
	UploadAudience  = "family-photo-cloud-tus"
)

type Principal struct {
	UserID    string
	SessionID string
	UploadID  string
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok && principal.UserID != "" && (principal.SessionID != "" || principal.UploadID != "")
}

type AccessClaims struct {
	UserID    string `json:"uid"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

type UploadClaims struct {
	UserID   string `json:"uid"`
	UploadID string `json:"upid"`
	jwt.RegisteredClaims
}

type AccessTokenManager struct {
	key      []byte
	issuer   string
	audience string
	leeway   time.Duration
}

func NewAccessTokenManager(key []byte, issuer, audience string) (*AccessTokenManager, error) {
	if len(key) < 32 {
		return nil, errors.New("access-token HMAC key must contain at least 32 bytes")
	}
	if issuer == "" || audience == "" {
		return nil, errors.New("token issuer and audience are required")
	}
	return &AccessTokenManager{
		key:      append([]byte(nil), key...),
		issuer:   issuer,
		audience: audience,
		leeway:   30 * time.Second,
	}, nil
}

func (m *AccessTokenManager) Issue(principal Principal, now time.Time, ttl time.Duration) (string, error) {
	if principal.UserID == "" || principal.SessionID == "" {
		return "", errors.New("user and session IDs are required")
	}
	if ttl <= 0 || ttl > 30*time.Minute {
		return "", errors.New("access-token TTL must be between zero and 30 minutes")
	}

	claims := AccessClaims{
		UserID:    principal.UserID,
		SessionID: principal.SessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   principal.UserID,
			Audience:  jwt.ClaimStrings{m.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-m.leeway)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}

	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.key)
}

func (m *AccessTokenManager) Verify(raw string) (Principal, error) {
	claims := new(AccessClaims)
	token, err := jwt.ParseWithClaims(
		raw,
		claims,
		func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("unexpected signing method %q", token.Method.Alg())
			}
			return m.key, nil
		},
		jwt.WithAudience(m.audience),
		jwt.WithIssuer(m.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(m.leeway),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil || !token.Valid {
		return Principal{}, errors.New("invalid access token")
	}
	if claims.UserID == "" || claims.SessionID == "" || claims.Subject != claims.UserID {
		return Principal{}, errors.New("invalid access-token identity")
	}
	return Principal{UserID: claims.UserID, SessionID: claims.SessionID}, nil
}

func (m *AccessTokenManager) IssueUpload(ownerID, uploadID string, now time.Time, ttl time.Duration) (string, error) {
	if ownerID == "" || uploadID == "" {
		return "", errors.New("owner and upload IDs are required")
	}
	if ttl <= 0 || ttl > 8*24*time.Hour {
		return "", errors.New("upload-token TTL must be between zero and eight days")
	}
	claims := UploadClaims{
		UserID:   ownerID,
		UploadID: uploadID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   ownerID,
			Audience:  jwt.ClaimStrings{UploadAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-m.leeway)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.key)
}

func (m *AccessTokenManager) VerifyUpload(raw string) (Principal, error) {
	claims := new(UploadClaims)
	token, err := jwt.ParseWithClaims(
		raw,
		claims,
		func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("unexpected signing method %q", token.Method.Alg())
			}
			return m.key, nil
		},
		jwt.WithAudience(UploadAudience),
		jwt.WithIssuer(m.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(m.leeway),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil || !token.Valid {
		return Principal{}, errors.New("invalid upload token")
	}
	if claims.UserID == "" || claims.UploadID == "" || claims.Subject != claims.UserID {
		return Principal{}, errors.New("invalid upload-token identity")
	}
	return Principal{UserID: claims.UserID, UploadID: claims.UploadID}, nil
}
