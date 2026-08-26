package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"family-photo-cloud/internal/auth"
)

type memoryAccountRepository struct {
	mu       sync.Mutex
	user     User
	sessions map[[32]byte]string
	nextID   int
}

func (repository *memoryAccountRepository) SessionActive(_ context.Context, _ string, sessionID string) (bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for _, id := range repository.sessions {
		if id == sessionID {
			return true, nil
		}
	}
	return false, nil
}

func (repository *memoryAccountRepository) ListDeviceSessions(_ context.Context, _ string) ([]DeviceSession, error) {
	return nil, nil
}

func (repository *memoryAccountRepository) RevokeDeviceSession(_ context.Context, _ string, _ string) error {
	return ErrInvalidCredentials
}

func (repository *memoryAccountRepository) ActiveUserByEmail(_ context.Context, email string) (User, error) {
	if strings.EqualFold(email, repository.user.Email) {
		return repository.user, nil
	}
	return User{}, ErrInvalidCredentials
}

func (repository *memoryAccountRepository) CreateRefreshSession(
	_ context.Context, _ string, _ string, hash [32]byte, _ time.Time, _ *int64,
) (string, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.nextID++
	id := "90000000-0000-4000-8000-00000000000" + string(rune('0'+repository.nextID))
	repository.sessions[hash] = id
	return id, nil
}

func (repository *memoryAccountRepository) RotateRefreshSession(
	_ context.Context, oldHash, newHash, _ [32]byte, _ time.Time, _ []byte, _ []byte, _ time.Time,
) (RefreshRotation, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	id, ok := repository.sessions[oldHash]
	if !ok {
		return RefreshRotation{}, ErrInvalidCredentials
	}
	delete(repository.sessions, oldHash)
	repository.sessions[newHash] = id
	return RefreshRotation{User: repository.user, SessionID: id}, nil
}

func (repository *memoryAccountRepository) RevokeRefreshSession(_ context.Context, hash [32]byte) error {
	repository.mu.Lock()
	delete(repository.sessions, hash)
	repository.mu.Unlock()
	return nil
}

func TestLoginRefreshRotationAndLogout(t *testing.T) {
	passwordHash, err := HashPassword("a strong family password")
	if err != nil {
		t.Fatal(err)
	}
	repository := &memoryAccountRepository{
		user: User{
			ID:           "10000000-0000-4000-8000-000000000001",
			Email:        "parent@example.com",
			PasswordHash: passwordHash,
			Role:         "member",
		},
		sessions: make(map[[32]byte]string),
	}
	tokens, err := auth.NewAccessTokenManager([]byte(strings.Repeat("k", 32)), auth.DefaultIssuer, auth.DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(repository, tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(api)
	defer server.Close()

	login := postAccountJSON(t, server, "/v1/auth/login", `{
        "email":"parent@example.com",
        "password":"a strong family password",
        "device_name":"Mom's iPhone"
    }`)
	if login.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(login.Body)
		t.Fatalf("login status = %d: %s", login.StatusCode, body)
	}
	first := decodeTokenResponse(t, login)
	principal, err := tokens.Verify(first.AccessToken)
	if err != nil || principal.UserID != repository.user.ID {
		t.Fatalf("access token principal = %#v, err=%v", principal, err)
	}
	firstHash := sha256.Sum256([]byte(first.RefreshToken))
	if _, ok := repository.sessions[firstHash]; !ok {
		t.Fatal("refresh token was not stored by digest")
	}

	refreshBody, _ := json.Marshal(refreshRequest{RefreshToken: first.RefreshToken, RotationRequestID: "11111111-2222-4333-8444-555555555555"})
	refresh := postAccountJSON(t, server, "/v1/auth/refresh", string(refreshBody))
	if refresh.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(refresh.Body)
		t.Fatalf("refresh status = %d: %s", refresh.StatusCode, body)
	}
	second := decodeTokenResponse(t, refresh)
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}
	if _, ok := repository.sessions[firstHash]; ok {
		t.Fatal("old refresh token remained active")
	}

	replay := postAccountJSON(t, server, "/v1/auth/refresh", string(refreshBody))
	replay.Body.Close()
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh replay status = %d, want 401", replay.StatusCode)
	}

	logoutBody, _ := json.Marshal(refreshRequest{RefreshToken: second.RefreshToken})
	logout := postAccountJSON(t, server, "/v1/auth/logout", string(logoutBody))
	logout.Body.Close()
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", logout.StatusCode)
	}
}

func TestRevokedSessionRejectsPreviouslyIssuedAccessTokenImmediately(t *testing.T) {
	passwordHash, err := HashPassword("a strong family password")
	if err != nil {
		t.Fatal(err)
	}
	repository := &memoryAccountRepository{user: User{ID: "10000000-0000-4000-8000-000000000001", Email: "parent@example.com", PasswordHash: passwordHash}, sessions: make(map[[32]byte]string)}
	tokens, err := auth.NewAccessTokenManager([]byte(strings.Repeat("k", 32)), auth.DefaultIssuer, auth.DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(repository, tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(api)
	defer server.Close()
	login := postAccountJSON(t, server, "/v1/auth/login", `{"email":"parent@example.com","password":"a strong family password","device_name":"iPhone"}`)
	tokensIssued := decodeTokenResponse(t, login)
	principal, err := tokens.Verify(tokensIssued.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/auth/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+tokensIssued.AccessToken)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("active access token status=%d", response.StatusCode)
	}
	repository.mu.Lock()
	for hash, id := range repository.sessions {
		if id == principal.SessionID {
			delete(repository.sessions, hash)
		}
	}
	repository.mu.Unlock()
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked access token status=%d want 401", response.StatusCode)
	}
}

func TestLoginDoesNotRevealUnknownAccount(t *testing.T) {
	passwordHash, err := HashPassword("a strong family password")
	if err != nil {
		t.Fatal(err)
	}
	repository := &memoryAccountRepository{
		user:     User{ID: "user", Email: "known@example.com", PasswordHash: passwordHash},
		sessions: make(map[[32]byte]string),
	}
	tokens, _ := auth.NewAccessTokenManager([]byte(strings.Repeat("k", 32)), auth.DefaultIssuer, auth.DefaultAudience)
	server := httptest.NewServer(NewAPI(repository, tokens, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()

	for _, body := range []string{
		`{"email":"missing@example.com","password":"a strong family password","device_name":"iPhone"}`,
		`{"email":"known@example.com","password":"wrong password value","device_name":"iPhone"}`,
	} {
		response := postAccountJSON(t, server, "/v1/auth/login", body)
		var problem map[string]any
		if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized || problem["code"] != "invalid_credentials" {
			t.Fatalf("unexpected login rejection: status=%d problem=%v", response.StatusCode, problem)
		}
	}
}

func TestLoginLimiterFailsClosedWhenCardinalityIsSaturated(t *testing.T) {
	now := time.Date(2026, time.August, 23, 0, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter(2, time.Minute, 2)
	if !limiter.Allow("first@example.com", now) || !limiter.Allow("second@example.com", now) {
		t.Fatal("expected initial limiter entries to be accepted")
	}
	if limiter.Allow("third@example.com", now.Add(time.Second)) {
		t.Fatal("a saturated fallback limiter must not evict a protected identity")
	}
	if len(limiter.entries) != 2 {
		t.Fatalf("limiter cardinality = %d, want 2", len(limiter.entries))
	}
	if !limiter.Allow("third@example.com", now.Add(time.Minute)) {
		t.Fatal("expired attempts were not reclaimed")
	}
}

func TestLoginLimiterLocksAccountWithinWindow(t *testing.T) {
	now := time.Date(2026, time.August, 23, 0, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter(2, time.Minute, 10)
	if !limiter.Allow("parent@example.com", now) || !limiter.Allow("parent@example.com", now) {
		t.Fatal("expected attempts within limit")
	}
	if limiter.Allow("parent@example.com", now) {
		t.Fatal("expected account limiter to reject third attempt")
	}
}

func TestAuthenticateAccessFailureWritesExactlyOneProblem(t *testing.T) {
	repository := &memoryAccountRepository{sessions: make(map[[32]byte]string)}
	tokens, err := auth.NewAccessTokenManager([]byte(strings.Repeat("k", 32)), auth.DefaultIssuer, auth.DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(repository, tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/v1/auth/sessions", nil)
	writer := &countingResponseWriter{header: make(http.Header)}

	api.listSessions(writer, request)

	if writer.status != http.StatusUnauthorized {
		t.Fatalf("status=%d want %d", writer.status, http.StatusUnauthorized)
	}
	if writer.writeHeaderCalls != 1 {
		t.Fatalf("WriteHeader calls=%d want 1", writer.writeHeaderCalls)
	}
}

type countingResponseWriter struct {
	header           http.Header
	status           int
	writeHeaderCalls int
	body             bytes.Buffer
}

func (writer *countingResponseWriter) Header() http.Header { return writer.header }

func (writer *countingResponseWriter) WriteHeader(status int) {
	writer.writeHeaderCalls++
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *countingResponseWriter) Write(payload []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.body.Write(payload)
}

func postAccountJSON(t *testing.T, server *httptest.Server, path, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeTokenResponse(t *testing.T, response *http.Response) tokenResponse {
	t.Helper()
	defer response.Body.Close()
	var result tokenResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.AccessToken == "" || result.RefreshToken == "" || result.ExpiresIn != 900 {
		t.Fatalf("invalid token response: %#v", result)
	}
	return result
}
