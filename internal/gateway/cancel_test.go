package gateway

// Cancelling a stuck single-request upload goes through tusd's file lock
// release protocol, but not through tusd's PATCH handler, so unlike
// tus_interrupt_test.go these tests also run under -race.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"family-photo-cloud/internal/upload"
)

// stallDirectUpload sends a single-request upload whose body stops after
// sent bytes, like a phone that lost its connection.
func (fixture *gatewayFixture) stallDirectUpload(content []byte, sent int, name string) <-chan string {
	fixture.t.Helper()
	connection, err := net.Dial("tcp", fixture.httpServer.Listener.Addr().String())
	if err != nil {
		fixture.t.Fatal(err)
	}
	fixture.t.Cleanup(func() { connection.Close() })
	hash := sha256.Sum256(content)
	request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer %s\r\n"+
		"X-Content-SHA256: %s\r\nX-File-Name: %s\r\nContent-Length: %d\r\n\r\n",
		directUploadPath, testUploadKey, hex.EncodeToString(hash[:]), name, len(content))
	if _, err := connection.Write(append([]byte(request), content[:sent]...)); err != nil {
		fixture.t.Fatal(err)
	}
	status := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(connection).ReadString('\n')
		status <- strings.TrimSpace(line)
	}()
	return status
}

type listedUpload struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	ErrorCode    string `json:"error_code"`
	ReceivedSize int64  `json:"received_size"`
	PreviewURL   string `json:"preview_url"`
	Cancellable  bool   `json:"cancellable"`
}

func (fixture *gatewayFixture) listUploads(token string) []listedUpload {
	fixture.t.Helper()
	response := fixture.request(http.MethodGet, "/v1/upload-sessions", token, nil, nil)
	defer response.Body.Close()
	var body struct {
		Uploads []listedUpload `json:"uploads"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		fixture.t.Fatal(err)
	}
	return body.Uploads
}

func TestCancelStopsAStuckUploadAndShowsWhatArrived(t *testing.T) {
	fixture := newDirectFixture(t)
	token := fixture.token(userA)
	content := []byte("\xff\xd8\xff\xe0 the first bytes of a photo that never finishes arriving: " + strings.Repeat("pixels ", 40))
	// More than the 64 bytes the handler sniffs before it starts the upload.
	stalled := fixture.stallDirectUpload(content, 100, "IMG_0563.JPG")

	var stuck listedUpload
	deadline := time.Now().Add(5 * time.Second)
	for {
		uploads := fixture.listUploads(token)
		if len(uploads) == 1 && uploads[0].ReceivedSize == 100 {
			stuck = uploads[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck upload never listed with its received bytes: %+v", uploads)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stuck.State != string(upload.StateUploading) || !stuck.Cancellable || stuck.PreviewURL == "" {
		t.Fatalf("listed stuck upload = %+v", stuck)
	}

	// The preview opens with its ticket alone (for an <img>), shows exactly
	// the bytes received, and the ticket opens nothing else.
	preview := fixture.request(http.MethodGet, stuck.PreviewURL, "", nil, nil)
	body, _ := io.ReadAll(preview.Body)
	preview.Body.Close()
	if preview.StatusCode != http.StatusOK || string(body) != string(content[:100]) || preview.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("preview = %d %q %v", preview.StatusCode, body, preview.Header)
	}
	ticket := stuck.PreviewURL[strings.Index(stuck.PreviewURL, "?"):]
	for _, path := range []string{"/v1/upload-sessions/00000000-0000-4000-8000-000000000000/preview" + ticket, "/v1/assets/" + stuck.ID + "/original" + ticket} {
		response := fixture.request(http.MethodGet, path, "", nil, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s with a preview ticket = %d", path, response.StatusCode)
		}
	}

	// Somebody else cannot stop it.
	other := fixture.request(http.MethodDelete, "/v1/upload-sessions/"+stuck.ID, fixture.token(userB), nil, nil)
	other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Fatalf("cancel by another user = %d", other.StatusCode)
	}

	started := time.Now()
	response := fixture.request(http.MethodDelete, "/v1/upload-sessions/"+stuck.ID, token, nil, nil)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel = %d", response.StatusCode)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancel took %v", elapsed)
	}
	select {
	case status := <-stalled:
		if !strings.Contains(status, " 409 ") {
			t.Fatalf("stalled upload status = %q", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled upload was not interrupted")
	}
	if _, err := os.Stat(filepath.Join(fixture.mediaRoot, ".staging", "tus", stuck.ID)); !os.IsNotExist(err) {
		t.Fatalf("partial bytes kept: %v", err)
	}
	cancelled := fixture.listUploads(token)[0]
	if cancelled.State != string(upload.StateExpired) || cancelled.ErrorCode != "cancelled" || cancelled.Cancellable {
		t.Fatalf("after cancel = %+v", cancelled)
	}
	// Cancelling twice is harmless, and sending the file again restarts it.
	again := fixture.request(http.MethodDelete, "/v1/upload-sessions/"+stuck.ID, token, nil, nil)
	again.Body.Close()
	if again.StatusCode != http.StatusNoContent {
		t.Fatalf("second cancel = %d", again.StatusCode)
	}
	status, resent := fixture.directUpload(testUploadKey, content, map[string]string{"X-File-Name": "IMG_0563.JPG"})
	if status != http.StatusAccepted || resent.ID != stuck.ID {
		t.Fatalf("resend = %d %+v", status, resent)
	}
	fixture.waitForState(stuck.ID, upload.StateAvailable)
	finished := fixture.request(http.MethodDelete, "/v1/upload-sessions/"+stuck.ID, token, nil, nil)
	finished.Body.Close()
	if finished.StatusCode != http.StatusConflict {
		t.Fatalf("cancel of a finished upload = %d", finished.StatusCode)
	}
}

// A second request for the same file takes over from a stalled one instead
// of waiting for it to time out.
func TestRetriedDirectUploadTakesOverAStalledOne(t *testing.T) {
	fixture := newDirectFixture(t)
	content := []byte("a video the Shortcut sends again after the first try stalled " + strings.Repeat("frames ", 40))
	stalled := fixture.stallDirectUpload(content, 100, "IMG_0001.HEIC")
	waitForAnyStaging(t, fixture, 100)
	status, response := fixture.directUpload(testUploadKey, content, nil)
	if status != http.StatusAccepted {
		t.Fatalf("retry = %d %+v", status, response)
	}
	select {
	case line := <-stalled:
		if !strings.Contains(line, " 409 ") {
			t.Fatalf("stalled request = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled request was not interrupted")
	}
	fixture.waitForState(response.ID, upload.StateAvailable)
}

func waitForAnyStaging(t *testing.T, fixture *gatewayFixture, size int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(fixture.mediaRoot, ".staging", "tus"))
		for _, entry := range entries {
			if info, err := entry.Info(); err == nil && !strings.Contains(entry.Name(), ".") && info.Size() >= size {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no staging payload appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
