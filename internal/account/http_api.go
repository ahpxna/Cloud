package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"family-photo-cloud/internal/auth"
)

const (
	accessTokenTTL  = 15 * time.Minute
	refreshTokenTTL = 30 * 24 * time.Hour
)

type API struct {
	repository Repository
	tokens     *auth.AccessTokenManager
	logger     *slog.Logger
	now        func() time.Time
	limiter    *loginLimiter
	dummyHash  string
}

func NewAPI(repository Repository, tokens *auth.AccessTokenManager, logger *slog.Logger) *API {
	if logger == nil {
		logger = slog.Default()
	}
	dummyHash, err := HashPassword("dummy password used only for timing equalization")
	if err != nil {
		panic("construct fixed-valid dummy password: " + err.Error())
	}
	return &API{
		repository: repository,
		tokens:     tokens,
		logger:     logger,
		now:        time.Now,
		limiter:    newLoginLimiter(5, 10*time.Minute),
		dummyHash:  dummyHash,
	}
}

type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
	UserID           string `json:"user_id"`
}

func (api *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/auth/login" && r.Method == http.MethodPost:
		api.login(w, r)
	case r.URL.Path == "/v1/auth/refresh" && r.Method == http.MethodPost:
		api.refresh(w, r)
	case r.URL.Path == "/v1/auth/logout" && r.Method == http.MethodPost:
		api.logout(w, r)
	default:
		w.Header().Set("Allow", "POST")
		accountProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (api *API) login(w http.ResponseWriter, r *http.Request) {
	var request loginRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	email, validEmail := normalizeEmail(request.Email)
	if !validEmail || len(request.Password) > 1024 || request.DeviceName == "" || len(request.DeviceName) > 100 {
		accountProblem(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	}
	if !api.limiter.Allow(email, api.now()) {
		w.Header().Set("Retry-After", "600")
		accountProblem(w, http.StatusTooManyRequests, "login_rate_limited", "try again later")
		return
	}

	user, err := api.repository.ActiveUserByEmail(r.Context(), email)
	if err != nil {
		if !errors.Is(err, ErrInvalidCredentials) {
			api.logger.Error("lookup account", "error", err)
			accountProblem(w, http.StatusInternalServerError, "authentication_unavailable", "authentication is temporarily unavailable")
			return
		}
		_, _ = VerifyPassword(api.dummyHash, request.Password)
		accountProblem(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	}
	valid, err := VerifyPassword(user.PasswordHash, request.Password)
	if err != nil {
		api.logger.Error("invalid password record", "user_id", user.ID, "error", err)
		accountProblem(w, http.StatusInternalServerError, "authentication_unavailable", "authentication is temporarily unavailable")
		return
	}
	if !valid {
		accountProblem(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	}
	api.limiter.Reset(email)
	api.createTokenPair(w, r.Context(), user, request.DeviceName)
}

func (api *API) refresh(w http.ResponseWriter, r *http.Request) {
	var request refreshRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	oldHash, ok := refreshHash(request.RefreshToken)
	if !ok {
		accountProblem(w, http.StatusUnauthorized, "invalid_refresh_token", "refresh token is invalid or expired")
		return
	}
	newRaw, newHash, err := newRefreshToken()
	if err != nil {
		accountProblem(w, http.StatusInternalServerError, "token_issue_failed", "could not issue token")
		return
	}
	now := api.now().UTC()
	user, sessionID, err := api.repository.RotateRefreshSession(r.Context(), oldHash, newHash, now.Add(refreshTokenTTL))
	if err != nil {
		accountProblem(w, http.StatusUnauthorized, "invalid_refresh_token", "refresh token is invalid or expired")
		return
	}
	accessToken, err := api.tokens.Issue(auth.Principal{UserID: user.ID, SessionID: sessionID}, now, accessTokenTTL)
	if err != nil {
		accountProblem(w, http.StatusInternalServerError, "token_issue_failed", "could not issue token")
		return
	}
	accountJSON(w, http.StatusOK, tokenResponse{
		AccessToken:      accessToken,
		TokenType:        "Bearer",
		ExpiresIn:        int64(accessTokenTTL.Seconds()),
		RefreshToken:     newRaw,
		RefreshExpiresIn: int64(refreshTokenTTL.Seconds()),
		UserID:           user.ID,
	})
}

func (api *API) logout(w http.ResponseWriter, r *http.Request) {
	var request refreshRequest
	if !decodeAccountJSON(w, r, &request) {
		return
	}
	hash, ok := refreshHash(request.RefreshToken)
	if ok {
		if err := api.repository.RevokeRefreshSession(r.Context(), hash); err != nil {
			accountProblem(w, http.StatusInternalServerError, "logout_failed", "could not revoke session")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) createTokenPair(w http.ResponseWriter, ctx context.Context, user User, deviceName string) {
	rawRefresh, refreshDigest, err := newRefreshToken()
	if err != nil {
		accountProblem(w, http.StatusInternalServerError, "token_issue_failed", "could not issue token")
		return
	}
	now := api.now().UTC()
	sessionID, err := api.repository.CreateRefreshSession(ctx, user.ID, deviceName, refreshDigest, now.Add(refreshTokenTTL))
	if err != nil {
		api.logger.Error("create refresh session", "user_id", user.ID, "error", err)
		accountProblem(w, http.StatusInternalServerError, "token_issue_failed", "could not issue token")
		return
	}
	accessToken, err := api.tokens.Issue(auth.Principal{UserID: user.ID, SessionID: sessionID}, now, accessTokenTTL)
	if err != nil {
		accountProblem(w, http.StatusInternalServerError, "token_issue_failed", "could not issue token")
		return
	}
	accountJSON(w, http.StatusOK, tokenResponse{
		AccessToken:      accessToken,
		TokenType:        "Bearer",
		ExpiresIn:        int64(accessTokenTTL.Seconds()),
		RefreshToken:     rawRefresh,
		RefreshExpiresIn: int64(refreshTokenTTL.Seconds()),
		UserID:           user.ID,
	})
}

func newRefreshToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw[:])
	return encoded, sha256.Sum256([]byte(encoded)), nil
}

func refreshHash(raw string) ([32]byte, bool) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) != 32 || len(raw) > 128 {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(raw)), true
}

func normalizeEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) < 3 || len(email) > 254 {
		return "", false
	}
	address, err := mail.ParseAddress(email)
	return email, err == nil && address.Address == email
}

func decodeAccountJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		accountProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		accountProblem(w, http.StatusBadRequest, "invalid_json", "request body is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		accountProblem(w, http.StatusBadRequest, "invalid_json", "request must contain one JSON object")
		return false
	}
	return true
}

func accountJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func accountProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code, "detail": detail})
}

type loginAttempt struct {
	count       int
	windowStart time.Time
}

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]loginAttempt
	limit   int
	window  time.Duration
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	return &loginLimiter{entries: make(map[string]loginAttempt), limit: limit, window: window}
}

func (limiter *loginLimiter) Allow(key string, now time.Time) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	attempt := limiter.entries[key]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) >= limiter.window {
		limiter.entries[key] = loginAttempt{count: 1, windowStart: now}
		return true
	}
	if attempt.count >= limiter.limit {
		return false
	}
	attempt.count++
	limiter.entries[key] = attempt
	return true
}

func (limiter *loginLimiter) Reset(key string) {
	limiter.mu.Lock()
	delete(limiter.entries, key)
	limiter.mu.Unlock()
}
