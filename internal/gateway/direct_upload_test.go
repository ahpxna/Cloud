package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"family-photo-cloud/internal/account"
	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/upload"
)

const testUploadKey = "fpcu_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type fakeUploadKeys struct {
	owners map[[32]byte]string
}

func (f fakeUploadKeys) UploadKeyOwner(_ context.Context, keyHash [32]byte, _ time.Time) (string, string, error) {
	owner, ok := f.owners[keyHash]
	if !ok {
		return "", "", account.ErrUploadKeyInvalid
	}
	return owner, "key-id", nil
}

func newDirectFixture(t *testing.T) *gatewayFixture {
	t.Helper()
	keyHash, ok := account.UploadKeyHash(testUploadKey)
	if !ok {
		t.Fatal("test upload key has an invalid format")
	}
	repository := upload.NewMemoryRepository()
	tokens, err := auth.NewAccessTokenManager([]byte(strings.Repeat("k", 32)), auth.DefaultIssuer, auth.DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	mediaRoot := t.TempDir()
	server, err := New(Config{
		Repository:       repository,
		Tokens:           tokens,
		MediaRoot:        mediaRoot,
		MaxUploadBytes:   1 << 20,
		ChunkBytes:       6,
		VerificationJobs: 1,
		UploadKeys:       fakeUploadKeys{owners: map[[32]byte]string{keyHash: userA}},
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
	return &gatewayFixture{t: t, server: server, httpServer: httpServer, repository: repository, tokens: tokens, mediaRoot: mediaRoot}
}

func (fixture *gatewayFixture) directUpload(token string, content []byte, headers map[string]string) (int, directUploadResponse) {
	fixture.t.Helper()
	hash := sha256.Sum256(content)
	all := map[string]string{"X-Content-SHA256": hex.EncodeToString(hash[:]), "X-File-Name": "IMG_0001.HEIC"}
	for name, value := range headers {
		all[name] = value
	}
	response := fixture.request(http.MethodPost, directUploadPath, token, bytes.NewReader(content), all)
	defer response.Body.Close()
	var decoded directUploadResponse
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

func (fixture *gatewayFixture) waitForState(id string, want upload.State) upload.Session {
	fixture.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		session, err := fixture.repository.SessionByID(context.Background(), id)
		if err == nil && session.State == want {
			return session
		}
		if time.Now().After(deadline) {
			fixture.t.Fatalf("session %s did not reach %s: %#v err=%v", id, want, session, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDirectUploadWithShortcutKeyIsVerifiedAndIdempotent(t *testing.T) {
	fixture := newDirectFixture(t)
	content := []byte("a whole photo sent by an iOS Shortcut")

	status, created := fixture.directUpload(testUploadKey, content, nil)
	if status != http.StatusAccepted || created.State != upload.StateReceived || created.ID == "" {
		t.Fatalf("first upload = %d %#v", status, created)
	}
	session := fixture.waitForState(created.ID, upload.StateAvailable)
	if session.OwnerID != userA || session.MediaType != "image/heic" || session.OriginalFilename != "IMG_0001.HEIC" {
		t.Fatalf("unexpected session %#v", session)
	}

	status, again := fixture.directUpload(testUploadKey, content, nil)
	if status != http.StatusOK || !again.Duplicate || again.ID != created.ID {
		t.Fatalf("repeat upload = %d %#v", status, again)
	}

	// The key is upload-only: it cannot read the library or list uploads.
	for _, path := range []string{"/v1/assets", "/v1/upload-sessions"} {
		response := fixture.request(http.MethodGet, path, testUploadKey, nil, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("upload key reached %s: status %d", path, response.StatusCode)
		}
	}

	list := fixture.request(http.MethodGet, "/v1/upload-sessions?limit=10", fixture.token(userA), nil, nil)
	defer list.Body.Close()
	var listed struct {
		Uploads []upload.SessionSummary `json:"uploads"`
	}
	if err := json.NewDecoder(list.Body).Decode(&listed); err != nil || list.StatusCode != http.StatusOK {
		t.Fatalf("list = %d err=%v", list.StatusCode, err)
	}
	if len(listed.Uploads) != 1 || listed.Uploads[0].State != upload.StateAvailable || listed.Uploads[0].ReceivedSize != int64(len(content)) {
		t.Fatalf("listed uploads = %#v", listed.Uploads)
	}
	other := fixture.request(http.MethodGet, "/v1/upload-sessions", fixture.token(userB), nil, nil)
	defer other.Body.Close()
	var otherListed struct {
		Uploads []upload.SessionSummary `json:"uploads"`
	}
	if err := json.NewDecoder(other.Body).Decode(&otherListed); err != nil || len(otherListed.Uploads) != 0 {
		t.Fatalf("another owner saw uploads: %#v err=%v", otherListed.Uploads, err)
	}
}

func TestDirectUploadQuarantinesMismatchedDigest(t *testing.T) {
	fixture := newDirectFixture(t)
	wrong := sha256.Sum256([]byte("not these bytes"))
	status, created := fixture.directUpload(fixture.token(userA), []byte("actual bytes"), map[string]string{
		"X-Content-SHA256": hex.EncodeToString(wrong[:]),
		"X-File-Name":      "clip.mov",
	})
	if status != http.StatusAccepted {
		t.Fatalf("upload status = %d", status)
	}
	session := fixture.waitForState(created.ID, upload.StateQuarantined)
	if session.MediaType != "video/quicktime" {
		t.Fatalf("media type = %q", session.MediaType)
	}
	assets := fixture.request(http.MethodGet, "/v1/assets", fixture.token(userA), nil, nil)
	defer assets.Body.Close()
	body, _ := io.ReadAll(assets.Body)
	if !strings.Contains(string(body), `"assets":[]`) {
		t.Fatalf("quarantined upload became visible: %s", body)
	}
}

func TestDirectUploadRejectsBadRequests(t *testing.T) {
	fixture := newDirectFixture(t)
	content := []byte("photo")
	cases := []struct {
		name    string
		token   string
		headers map[string]string
		want    int
	}{
		{"no credentials", "", nil, http.StatusUnauthorized},
		{"unknown key", "fpcu_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", nil, http.StatusUnauthorized},
		{"upload capability is not accepted", "", map[string]string{"Authorization": "Bearer not-a-token"}, http.StatusUnauthorized},
		{"missing digest", testUploadKey, map[string]string{"X-Content-SHA256": ""}, http.StatusBadRequest},
		{"path in name", testUploadKey, map[string]string{"X-File-Name": "../etc/passwd.jpg"}, http.StatusBadRequest},
		{"not media", testUploadKey, map[string]string{"X-File-Name": "notes.txt", "Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
	}
	for _, testCase := range cases {
		status, _ := fixture.directUpload(testCase.token, content, testCase.headers)
		if status != testCase.want {
			t.Errorf("%s: status %d, want %d", testCase.name, status, testCase.want)
		}
	}
	if status, created := fixture.directUpload(testUploadKey, content, map[string]string{"X-File-Name": "IMG%20%C3%A1nh.JPG"}); status != http.StatusAccepted {
		t.Fatalf("percent-encoded name status = %d", status)
	} else if session, _ := fixture.repository.SessionByID(context.Background(), created.ID); session.OriginalFilename != "IMG ánh.JPG" {
		t.Fatalf("decoded filename = %q", session.OriginalFilename)
	}
}

func TestViewTicketLoadsOnlyItsOwnOriginal(t *testing.T) {
	fixture := newDirectFixture(t)
	first := []byte("first original")
	second := []byte("second original")
	for index, content := range [][]byte{first, second} {
		_, created := fixture.directUpload(testUploadKey, content, map[string]string{"X-File-Name": []string{"a.jpg", "b.jpg"}[index]})
		fixture.waitForState(created.ID, upload.StateAvailable)
	}
	list := fixture.request(http.MethodGet, "/v1/assets?tickets=1", fixture.token(userA), nil, nil)
	defer list.Body.Close()
	var library struct {
		Assets []struct {
			ID      string `json:"id"`
			ViewURL string `json:"view_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(list.Body).Decode(&library); err != nil || len(library.Assets) != 2 {
		t.Fatalf("library = %#v err=%v", library, err)
	}
	for _, asset := range library.Assets {
		if asset.ViewURL == "" {
			t.Fatal("view_url missing")
		}
		response := fixture.request(http.MethodGet, asset.ViewURL, "", nil, nil)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || (string(body) != string(first) && string(body) != string(second)) {
			t.Fatalf("ticket download = %d %q", response.StatusCode, body)
		}
	}
	ticket := library.Assets[0].ViewURL[strings.Index(library.Assets[0].ViewURL, "?"):]
	swapped := fixture.request(http.MethodGet, "/v1/assets/"+library.Assets[1].ID+"/original"+ticket, "", nil, nil)
	swapped.Body.Close()
	if swapped.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ticket opened another asset: %d", swapped.StatusCode)
	}
	listWithTicket := fixture.request(http.MethodGet, "/v1/assets"+ticket, "", nil, nil)
	listWithTicket.Body.Close()
	if listWithTicket.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ticket listed the library: %d", listWithTicket.StatusCode)
	}
}

func TestDirectUploadSniffsExtensionlessShortcutFiles(t *testing.T) {
	fixture := newDirectFixture(t)
	heic := append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c'}, []byte("rest of an HEIC photo")...)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE1}, []byte("rest of a JPEG photo")...)
	mov := append([]byte{0, 0, 0, 0x14, 'f', 't', 'y', 'p', 'q', 't', ' ', ' '}, []byte("rest of a video")...)
	for _, testCase := range []struct {
		content       []byte
		name          string
		wantName      string
		wantMediaType string
	}{
		{heic, "IMG_1234", "IMG_1234.HEIC", "image/heic"},
		{jpeg, "IMG_1235", "IMG_1235.JPG", "image/jpeg"},
		{mov, "IMG_1236", "IMG_1236.MOV", "video/quicktime"},
		{jpeg, "named.jpeg", "named.jpeg", "image/jpeg"},
	} {
		status, created := fixture.directUpload(testUploadKey, testCase.content, map[string]string{
			"X-File-Name": testCase.name, "Content-Type": "application/octet-stream",
		})
		if status != http.StatusAccepted {
			t.Fatalf("%s: status %d", testCase.name, status)
		}
		session := fixture.waitForState(created.ID, upload.StateAvailable)
		if session.OriginalFilename != testCase.wantName || session.MediaType != testCase.wantMediaType {
			t.Fatalf("%s: stored as %q %q", testCase.name, session.OriginalFilename, session.MediaType)
		}
	}
}
