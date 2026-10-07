package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"family-photo-cloud/internal/upload"
)

// startTusUpload creates a session plus its TUS resource and returns the
// session, its upload capability and the TUS resource path.
func (fixture *gatewayFixture) startTusUpload(token, clientID string, content []byte) (createResponse, string) {
	fixture.t.Helper()
	hash := sha256.Sum256(content)
	session := fixture.createSession(token, clientID, clientID+".jpg", content, hash)
	metadata := "session_id " + base64.StdEncoding.EncodeToString([]byte(session.ID))
	created := fixture.request(http.MethodPost, tusBasePath, session.UploadToken, nil, map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   fmt.Sprint(len(content)),
		"Upload-Metadata": metadata,
	})
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(created.Body)
		fixture.t.Fatalf("create status = %d: %s", created.StatusCode, body)
	}
	location, err := url.Parse(created.Header.Get("Location"))
	if err != nil || location.Path == "" {
		fixture.t.Fatalf("invalid TUS Location %q: %v", created.Header.Get("Location"), err)
	}
	return session, location.Path
}

func (fixture *gatewayFixture) patch(path, uploadToken string, offset int, body []byte) *http.Response {
	fixture.t.Helper()
	return fixture.request(http.MethodPatch, path, uploadToken, bytes.NewReader(body), map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": fmt.Sprint(offset),
		"Content-Type":  "application/offset+octet-stream",
	})
}

// tusd's filestore writes the .info sidecar once at creation and never rewrites
// its Offset; the durable offset is the payload size. Expiry/restart must use
// the same rule or every partially uploaded resource looks inconsistent.
func TestRestartResetsPartiallyUploadedTusResource(t *testing.T) {
	fixture := newGatewayFixture(t)
	token := fixture.token(userA)
	content := []byte("ten-bytes!")
	session, path := fixture.startTusUpload(token, "partial-restart", content)

	response := fixture.patch(path, session.UploadToken, 0, content[:4])
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("partial PATCH status = %d", response.StatusCode)
	}

	staging := filepath.Join(fixture.mediaRoot, ".staging", "tus")
	sidecar, err := os.ReadFile(filepath.Join(staging, session.ID+".info"))
	if err != nil {
		t.Fatal(err)
	}
	var info struct{ Offset, Size int64 }
	if err := json.Unmarshal(sidecar, &info); err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(content)) || info.Offset != 0 {
		t.Fatalf("tusd sidecar format changed: %s", sidecar)
	}

	processor, err := upload.NewProcessor(fixture.repository, fixture.mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := processor.InspectTusResource(session.ID, int64(len(content)))
	if err != nil || inspection.State != upload.TusIncomplete || inspection.Offset != 4 {
		t.Fatalf("inspection = %#v err=%v, want incomplete at offset 4", inspection, err)
	}

	restart := fixture.request(http.MethodPost, "/v1/upload-sessions/"+session.ID+"/restart", token, nil, nil)
	body, _ := io.ReadAll(restart.Body)
	restart.Body.Close()
	if restart.StatusCode != http.StatusOK {
		t.Fatalf("restart status = %d: %s", restart.StatusCode, body)
	}
	for _, name := range []string{session.ID, session.ID + ".info"} {
		if _, err := os.Stat(filepath.Join(staging, name)); !os.IsNotExist(err) {
			t.Fatalf("restart left staging file %s: %v", name, err)
		}
	}
	reset, err := fixture.repository.SessionByID(t.Context(), session.ID)
	if err != nil || reset.State != upload.StateCreated || reset.ReceivedSize != 0 {
		t.Fatalf("session after restart = %#v err=%v", reset, err)
	}
}
