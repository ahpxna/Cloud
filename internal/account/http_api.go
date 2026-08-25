package account

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	repository     Repository
	tokens         *auth.AccessTokenManager
	logger         *slog.Logger
	now            func() time.Time
	limiter        *loginLimiter
	durableLimiter LoginThrottleRepository
	throttleKey    []byte
	globalLimiter  *tokenBucket
	passwordGate   chan struct{}
	dummyHash      string
	mfaRepo        MFARepository
	mfaCipher      *mfaCipher
}

type SecurityConfig struct {
	LoginThrottleHMACKey []byte
	GlobalLoginRate      float64
	GlobalLoginBurst     int
	MFAEncryptionKey     []byte
}

func NewAPI(repository Repository, tokens *auth.AccessTokenManager, logger *slog.Logger) *API {
	api, err := newAPI(repository, tokens, logger, SecurityConfig{})
	if err != nil {
		panic(err)
	}
	return api
}

// NewSecureAPI is the production constructor. Unlike NewAPI (kept for small
// unit fakes), it refuses to start unless the repository can persist login
// throttles and an independent HMAC key is configured.
func NewSecureAPI(repository Repository, tokens *auth.AccessTokenManager, logger *slog.Logger, config SecurityConfig) (*API, error) {
	if len(config.LoginThrottleHMACKey) < 32 {
		return nil, errors.New("login throttle HMAC key must contain at least 32 bytes")
	}
	if _, ok := repository.(LoginThrottleRepository); !ok {
		return nil, errors.New("production account repository must persist login throttles")
	}
	mfaRepo, ok := repository.(MFARepository)
	if !ok {
		return nil, errors.New("production account repository must persist MFA state")
	}
	mfaCipher, err := newMFACipher(config.MFAEncryptionKey)
	if err != nil {
		return nil, err
	}
	api, err := newAPI(repository, tokens, logger, config)
	if err != nil {
		return nil, err
	}
	api.mfaRepo = mfaRepo
	api.mfaCipher = mfaCipher
	return api, nil
}

func newAPI(repository Repository, tokens *auth.AccessTokenManager, logger *slog.Logger, config SecurityConfig) (*API, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dummyHash, err := HashPassword("dummy password used only for timing equalization")
	if err != nil {
		return nil, fmt.Errorf("construct fixed-valid dummy password: %w", err)
	}
	if config.GlobalLoginRate <= 0 {
		config.GlobalLoginRate = 1
	}
	if config.GlobalLoginBurst <= 0 {
		config.GlobalLoginBurst = 20
	}
	api := &API{
		repository:    repository,
		tokens:        tokens,
		logger:        logger,
		now:           time.Now,
		limiter:       newLoginLimiter(5, 10*time.Minute, 10_000),
		throttleKey:   append([]byte(nil), config.LoginThrottleHMACKey...),
		globalLimiter: newTokenBucket(config.GlobalLoginRate, config.GlobalLoginBurst),
		passwordGate:  make(chan struct{}, 4),
		dummyHash:     dummyHash,
	}
	api.durableLimiter, _ = repository.(LoginThrottleRepository)
	return api, nil
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
	case r.URL.Path == "/v1/auth/mfa/enroll" && r.Method == http.MethodPost:
		api.mfaEnroll(w, r)
	case r.URL.Path == "/v1/auth/mfa/confirm" && r.Method == http.MethodPost:
		api.mfaConfirm(w, r)
	case r.URL.Path == "/v1/auth/mfa/verify" && r.Method == http.MethodPost:
		api.mfaVerify(w, r)
	case r.URL.Path == "/v1/auth/mfa/recovery" && r.Method == http.MethodPost:
		api.mfaRecovery(w, r)
	case r.URL.Path == "/v1/auth/mfa/disable" && r.Method == http.MethodPost:
		api.mfaDisable(w, r)
	case r.URL.Path == "/v1/auth/sessions" && r.Method == http.MethodGet:
		api.listSessions(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/auth/sessions/") && r.Method == http.MethodDelete:
		api.revokeSession(w, r, strings.TrimPrefix(r.URL.Path, "/v1/auth/sessions/"))
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		accountProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (api *API) listSessions(w http.ResponseWriter, r *http.Request) {
	principal, ok := api.accessPrincipal(r)
	if !ok {
		accountProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}
	sessions, err := api.repository.ListDeviceSessions(r.Context(), principal.UserID)
	if err != nil {
		api.logger.Error("list device sessions", "user_id", principal.UserID, "error", err)
		accountProblem(w, http.StatusInternalServerError, "session_list_failed", "could not list signed-in devices")
		return
	}
	accountJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (api *API) revokeSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if sessionID == "" || strings.Contains(sessionID, "/") {
		accountProblem(w, http.StatusNotFound, "not_found", "device session not found")
		return
	}
	principal, ok := api.accessPrincipal(r)
	if !ok {
		accountProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}
	if err := api.repository.RevokeDeviceSession(r.Context(), principal.UserID, sessionID); err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			accountProblem(w, http.StatusNotFound, "not_found", "device session not found")
			return
		}
		api.logger.Error("revoke device session", "user_id", principal.UserID, "error", err)
		accountProblem(w, http.StatusInternalServerError, "session_revoke_failed", "could not revoke device")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) accessPrincipal(r *http.Request) (auth.Principal, bool) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return auth.Principal{}, false
	}
	principal, err := api.tokens.Verify(parts[1])
	if err != nil {
		return auth.Principal{}, false
	}
	active, err := api.repository.SessionActive(r.Context(), principal.UserID, principal.SessionID)
	return principal, err == nil && active
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
	now := api.now().UTC()
	if !api.globalLimiter.Allow(now) {
		w.Header().Set("Retry-After", "1")
		accountProblem(w, http.StatusTooManyRequests, "login_rate_limited", "try again later")
		return
	}
	identityHash := api.loginIdentityHash(email)
	if api.durableLimiter != nil && len(api.throttleKey) >= 32 {
		allowed, retryAfter, err := api.durableLimiter.RecordLoginAttempt(r.Context(), identityHash, now, 10*time.Minute, 5)
		if err != nil {
			api.logger.Error("record login throttle", "error", err)
			accountProblem(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
			return
		}
		if !allowed {
			seconds := int64(retryAfter.Round(time.Second).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", fmt.Sprint(seconds))
			accountProblem(w, http.StatusTooManyRequests, "login_rate_limited", "try again later")
			return
		}
	} else if !api.limiter.Allow(email, now) {
		w.Header().Set("Retry-After", "600")
		accountProblem(w, http.StatusTooManyRequests, "login_rate_limited", "try again later")
		return
	}
	select {
	case api.passwordGate <- struct{}{}:
		defer func() { <-api.passwordGate }()
	default:
		w.Header().Set("Retry-After", "1")
		accountProblem(w, http.StatusTooManyRequests, "login_busy", "try again shortly")
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
	if api.durableLimiter != nil && len(api.throttleKey) >= 32 {
		if err := api.durableLimiter.ClearLoginAttempts(r.Context(), identityHash); err != nil {
			api.logger.Error("clear login throttle", "user_id", user.ID, "error", err)
			accountProblem(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
			return
		}
	} else {
		api.limiter.Reset(email)
	}
	if api.mfaRepo != nil {
		record, err := api.mfaRepo.TOTPForUser(r.Context(), user.ID)
		if err == nil && record.ConfirmedAt != nil {
			api.issueMFAChallenge(w, r.Context(), user, request.DeviceName)
			return
		}
		if err != nil && !errors.Is(err, ErrMFANotConfigured) {
			api.logger.Error("load MFA state", "user_id", user.ID, "error", err)
			accountProblem(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
			return
		}
	}
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
		if errors.Is(err, ErrRefreshReplay) {
			api.logger.Warn("refresh token replay; revoked token family")
		}
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

func (api *API) loginIdentityHash(email string) [32]byte {
	mac := hmac.New(sha256.New, api.throttleKey)
	_, _ = mac.Write([]byte(email))
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

type tokenBucket struct {
	mu       sync.Mutex
	rate     float64
	burst    float64
	tokens   float64
	lastFill time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst)}
}

func (bucket *tokenBucket) Allow(now time.Time) bool {
	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	if bucket.lastFill.IsZero() {
		bucket.lastFill = now
	} else if now.After(bucket.lastFill) {
		bucket.tokens += now.Sub(bucket.lastFill).Seconds() * bucket.rate
		if bucket.tokens > bucket.burst {
			bucket.tokens = bucket.burst
		}
		bucket.lastFill = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

type loginAttempt struct {
	count       int
	windowStart time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	entries  map[string]loginAttempt
	limit    int
	window   time.Duration
	capacity int
}

func newLoginLimiter(limit int, window time.Duration, capacity int) *loginLimiter {
	return &loginLimiter{entries: make(map[string]loginAttempt), limit: limit, window: window, capacity: capacity}
}

func (limiter *loginLimiter) Allow(key string, now time.Time) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	attempt, known := limiter.entries[key]
	if known && now.Sub(attempt.windowStart) >= limiter.window {
		delete(limiter.entries, key)
		known = false
	}
	if !known && len(limiter.entries) >= limiter.capacity {
		for existingKey, existing := range limiter.entries {
			if now.Sub(existing.windowStart) >= limiter.window {
				delete(limiter.entries, existingKey)
			}
		}
		if len(limiter.entries) >= limiter.capacity {
			return false
		}
	}
	if !known {
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
