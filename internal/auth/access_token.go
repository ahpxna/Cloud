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
	// ViewAudience scopes a short-lived ticket to one asset's original so a
	// browser <img>/<video> element, which cannot send an Authorization
	// header, can load it.
	ViewAudience = "family-photo-cloud-view"
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
	UserID    string `json:"uid"`
	SessionID string `json:"sid"`
	UploadID  string `json:"upid"`
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

func (m *AccessTokenManager) IssueUpload(ownerID, sessionID, uploadID string, now time.Time, ttl time.Duration) (string, error) {
	if ownerID == "" || sessionID == "" || uploadID == "" {
		return "", errors.New("owner, session, and upload IDs are required")
	}
	if ttl <= 0 || ttl > 8*24*time.Hour {
		return "", errors.New("upload-token TTL must be between zero and eight days")
	}
	claims := UploadClaims{
		UserID: ownerID, SessionID: sessionID, UploadID: uploadID,
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
	if claims.UserID == "" || claims.SessionID == "" || claims.UploadID == "" || claims.Subject != claims.UserID {
		return Principal{}, errors.New("invalid upload-token identity")
	}
	return Principal{UserID: claims.UserID, SessionID: claims.SessionID, UploadID: claims.UploadID}, nil
}

// View ticket kinds: a ticket for one never opens another. The ticket's
// subject ID is an asset ID, except for upload previews (an upload session
// ID) and downloads (a server-side download selection ID).
const (
	ViewOriginal      = "original"
	ViewThumbnail     = "thumbnail"
	ViewUploadPreview = "upload_preview"
	ViewDownload      = "download"
)

type ViewClaims struct {
	UserID    string `json:"uid"`
	SessionID string `json:"sid"`
	AssetID   string `json:"aid"`
	Kind      string `json:"knd"`
	jwt.RegisteredClaims
}

func (m *AccessTokenManager) IssueView(principal Principal, assetID, kind string, now time.Time, ttl time.Duration) (string, error) {
	if principal.UserID == "" || principal.SessionID == "" || assetID == "" {
		return "", errors.New("user, session, and asset IDs are required")
	}
	// Original tickets are short-lived. Thumbnail tickets may live a few hours
	// so they can be issued per hour and cached by the browser.
	maxTTL := 30 * time.Minute
	switch kind {
	case ViewOriginal, ViewUploadPreview, ViewDownload:
	case ViewThumbnail:
		maxTTL = 3 * time.Hour
	default:
		return "", errors.New("unknown view-ticket kind")
	}
	if ttl <= 0 || ttl > maxTTL {
		return "", errors.New("view-ticket TTL exceeds the limit for its kind")
	}
	claims := ViewClaims{
		UserID: principal.UserID, SessionID: principal.SessionID, AssetID: assetID, Kind: kind,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   principal.UserID,
			Audience:  jwt.ClaimStrings{ViewAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-m.leeway)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.key)
}

// VerifyView accepts a view ticket only for the asset and kind it was issued for.
func (m *AccessTokenManager) VerifyView(raw, assetID, kind string) (Principal, error) {
	claims := new(ViewClaims)
	token, err := jwt.ParseWithClaims(
		raw,
		claims,
		func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("unexpected signing method %q", token.Method.Alg())
			}
			return m.key, nil
		},
		jwt.WithAudience(ViewAudience),
		jwt.WithIssuer(m.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(m.leeway),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil || !token.Valid {
		return Principal{}, errors.New("invalid view ticket")
	}
	if claims.UserID == "" || claims.SessionID == "" || claims.Subject != claims.UserID || claims.AssetID == "" ||
		claims.AssetID != assetID || claims.Kind == "" || claims.Kind != kind {
		return Principal{}, errors.New("invalid view-ticket scope")
	}
	return Principal{UserID: claims.UserID, SessionID: claims.SessionID}, nil
}
