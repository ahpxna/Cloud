package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/upload"
)

const (
	userA = "10000000-0000-4000-8000-000000000001"
	userB = "20000000-0000-4000-8000-000000000002"
)

type gatewayFixture struct {
	t          *testing.T
	server     *Server
	httpServer *httptest.Server
	repository *upload.MemoryRepository
	tokens     *auth.AccessTokenManager
}

func newGatewayFixture(t *testing.T) *gatewayFixture {
	t.Helper()
	repository := upload.NewMemoryRepository()
	tokens, err := auth.NewAccessTokenManager([]byte(strings.Repeat("k", 32)), auth.DefaultIssuer, auth.DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Repository:       repository,
		Tokens:           tokens,
		MediaRoot:        t.TempDir(),
		MaxUploadBytes:   1 << 20,
		ChunkBytes:       6,
		VerificationJobs: 1,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		httpServer.Close()
		server.Close()
	})
	return &gatewayFixture{t: t, server: server, httpServer: httpServer, repository: repository, tokens: tokens}
}

func (fixture *gatewayFixture) token(user string) string {
	fixture.t.Helper()
	token, err := fixture.tokens.Issue(auth.Principal{
		UserID: user, SessionID: "90000000-0000-4000-8000-000000000009",
	}, time.Now(), 15*time.Minute)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return token
}

func (fixture *gatewayFixture) request(method, path, token string, body io.Reader, headers map[string]string) *http.Response {
	fixture.t.Helper()
	request, err := http.NewRequest(method, fixture.httpServer.URL+path, body)
	if err != nil {
		fixture.t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := fixture.httpServer.Client().Do(request)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return response
}

func TestTusUploadResumesAndRejectsCrossUserAccess(t *testing.T) {
	fixture := newGatewayFixture(t)
	content := []byte("ten-bytes!")
	hash := sha256.Sum256(content)
	tokenA := fixture.token(userA)
	tokenB := fixture.token(userB)

	session := fixture.createSession(tokenA, "asset-a", "IMG_0001.JPG", content, hash)
	if session.UploadToken == "" {
		t.Fatal("session response omitted upload capability")
	}
	uploadToken := session.UploadToken
	metadata := "session_id " + base64.StdEncoding.EncodeToString([]byte(session.ID))
	createResponse := fixture.request(http.MethodPost, tusBasePath, uploadToken, nil, map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   fmt.Sprint(len(content)),
		"Upload-Metadata": metadata,
	})
	defer createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createResponse.Body)
		t.Fatalf("create status = %d: %s", createResponse.StatusCode, body)
	}
	location, err := url.Parse(createResponse.Header.Get("Location"))
	if err != nil || location.Path == "" {
		t.Fatalf("invalid TUS Location: %q (%v)", createResponse.Header.Get("Location"), err)
	}

	crossUserHead := fixture.request(http.MethodHead, location.Path, tokenB, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	crossUserHead.Body.Close()
	if crossUserHead.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user HEAD status = %d, want 404", crossUserHead.StatusCode)
	}

	first := content[:4]
	patchResponse := fixture.request(http.MethodPatch, location.Path, uploadToken, bytes.NewReader(first), map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": "0",
		"Content-Type":  "application/offset+octet-stream",
	})
	patchResponse.Body.Close()
	if patchResponse.StatusCode != http.StatusNoContent || patchResponse.Header.Get("Upload-Offset") != "4" {
		t.Fatalf("first PATCH status/offset = %d/%q", patchResponse.StatusCode, patchResponse.Header.Get("Upload-Offset"))
	}

	headResponse := fixture.request(http.MethodHead, location.Path, uploadToken, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	headResponse.Body.Close()
	if headResponse.StatusCode != http.StatusOK || headResponse.Header.Get("Upload-Offset") != "4" {
		t.Fatalf("resume HEAD status/offset = %d/%q", headResponse.StatusCode, headResponse.Header.Get("Upload-Offset"))
	}

	oversizedPatch := fixture.request(http.MethodPatch, location.Path, uploadToken, bytes.NewReader([]byte("1234567")), map[string]string{
		"Tus-Resumable": "1.0.0", "Upload-Offset": "4", "Content-Type": "application/offset+octet-stream",
	})
	oversizedPatch.Body.Close()
	if oversizedPatch.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized PATCH status = %d, want 413", oversizedPatch.StatusCode)
	}

	unauthorizedPatch := fixture.request(http.MethodPatch, location.Path, "", bytes.NewReader(content[4:]), map[string]string{
		"Tus-Resumable": "1.0.0", "Upload-Offset": "4", "Content-Type": "application/offset+octet-stream",
	})
	unauthorizedPatch.Body.Close()
	if unauthorizedPatch.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized PATCH status = %d", unauthorizedPatch.StatusCode)
	}

	patchResponse = fixture.request(http.MethodPatch, location.Path, uploadToken, bytes.NewReader(content[4:]), map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": "4",
		"Content-Type":  "application/offset+octet-stream",
	})
	patchResponse.Body.Close()
	if patchResponse.StatusCode != http.StatusNoContent || patchResponse.Header.Get("Upload-Offset") != fmt.Sprint(len(content)) {
		t.Fatalf("resumed PATCH status/offset = %d/%q", patchResponse.StatusCode, patchResponse.Header.Get("Upload-Offset"))
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := fixture.repository.SessionByID(context.Background(), session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == upload.StateAvailable {
			if got.ReceivedSize != int64(len(content)) || got.ServerSHA256 == nil || *got.ServerSHA256 != hash {
				t.Fatalf("invalid available metadata: %#v", got)
			}
			break
		}
		if got.State == upload.StateFailed || got.State == upload.StateQuarantined {
			t.Fatalf("upload entered %s", got.State)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for available; state=%s", got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	assetList := fixture.request(http.MethodGet, "/v1/assets?limit=1", tokenA, nil, nil)
	defer assetList.Body.Close()
	if assetList.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(assetList.Body)
		t.Fatalf("asset list status = %d: %s", assetList.StatusCode, body)
	}
	var library struct {
		Assets []struct {
			ID          string `json:"id"`
			OriginalURL string `json:"original_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(assetList.Body).Decode(&library); err != nil {
		t.Fatal(err)
	}
	if len(library.Assets) != 1 || library.Assets[0].ID == "" {
		t.Fatalf("unexpected asset list: %#v", library)
	}

	rangeResponse := fixture.request(http.MethodGet, library.Assets[0].OriginalURL, tokenA, nil, map[string]string{"Range": "bytes=0-3"})
	rangeBytes, _ := io.ReadAll(rangeResponse.Body)
	rangeResponse.Body.Close()
	if rangeResponse.StatusCode != http.StatusPartialContent || string(rangeBytes) != string(content[:4]) {
		t.Fatalf("range response = %d/%q", rangeResponse.StatusCode, rangeBytes)
	}

	crossUserOriginal := fixture.request(http.MethodGet, library.Assets[0].OriginalURL, tokenB, nil, nil)
	crossUserOriginal.Body.Close()
	if crossUserOriginal.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user original status = %d, want 404", crossUserOriginal.StatusCode)
	}
}

func TestSessionCreationIsIdempotentAndOwnerScoped(t *testing.T) {
	fixture := newGatewayFixture(t)
	content := []byte("photo")
	hash := sha256.Sum256(content)
	tokenA := fixture.token(userA)

	first := fixture.createSession(tokenA, "same-local-id", "one.jpg", content, hash)
	second := fixture.createSession(tokenA, "same-local-id", "one.jpg", content, hash)
	if first.ID != second.ID || second.Created {
		t.Fatalf("idempotency mismatch: first=%#v second=%#v", first, second)
	}
	refreshedCapability := fixture.getSession(tokenA, first.ID)
	if refreshedCapability.UploadToken == "" {
		t.Fatal("session lookup omitted resumable upload capability")
	}
	capabilityOnJSON := fixture.request(http.MethodGet, "/v1/upload-sessions/"+first.ID, first.UploadToken, nil, nil)
	capabilityOnJSON.Body.Close()
	if capabilityOnJSON.StatusCode != http.StatusUnauthorized {
		t.Fatalf("upload capability reached general API: status=%d", capabilityOnJSON.StatusCode)
	}

	other := fixture.createSession(tokenA, "other-local-id", "two.jpg", content, hash)
	wrongMetadata := "session_id " + base64.StdEncoding.EncodeToString([]byte(other.ID))
	wrongCapability := fixture.request(http.MethodPost, tusBasePath, first.UploadToken, nil, map[string]string{
		"Tus-Resumable": "1.0.0", "Upload-Length": fmt.Sprint(len(content)), "Upload-Metadata": wrongMetadata,
	})
	wrongCapability.Body.Close()
	if wrongCapability.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-session capability status = %d, want 403", wrongCapability.StatusCode)
	}

	changedBody := fmt.Sprintf(`{"client_asset_id":"same-local-id","original_filename":"different.jpg","media_type":"image/jpeg","expected_size":%d,"client_sha256":"%s"}`,
		len(content), hex.EncodeToString(hash[:]))
	response := fixture.request(http.MethodPost, "/v1/upload-sessions", tokenA, strings.NewReader(changedBody), map[string]string{"Content-Type": "application/json"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("changed idempotency payload status = %d, want 409", response.StatusCode)
	}
}

type createResponse struct {
	ID          string       `json:"id"`
	State       upload.State `json:"state"`
	Created     bool         `json:"created"`
	UploadToken string       `json:"upload_token"`
}

func (fixture *gatewayFixture) createSession(token, clientID, filename string, content []byte, hash [32]byte) createResponse {
	fixture.t.Helper()
	body := fmt.Sprintf(`{"client_asset_id":%q,"original_filename":%q,"media_type":"image/jpeg","expected_size":%d,"client_sha256":"%s"}`,
		clientID, filename, len(content), hex.EncodeToString(hash[:]))
	response := fixture.request(http.MethodPost, "/v1/upload-sessions", token, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		fixture.t.Fatalf("session status = %d: %s", response.StatusCode, responseBody)
	}
	var result createResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		fixture.t.Fatal(err)
	}
	return result
}

func (fixture *gatewayFixture) getSession(token, id string) createResponse {
	fixture.t.Helper()
	response := fixture.request(http.MethodGet, "/v1/upload-sessions/"+id, token, nil, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		fixture.t.Fatalf("session lookup status = %d: %s", response.StatusCode, responseBody)
	}
	var result createResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		fixture.t.Fatal(err)
	}
	return result
}
