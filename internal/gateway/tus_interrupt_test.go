//go:build !race

package gateway

// These tests exercise tusd's lock-release protocol, which interrupts an
// in-flight PATCH. tusd v2.10.x assigns httpContext.body without
// synchronisation against the goroutine that closes it on release
// (pkg/handler/context.go:65 vs unrouted_handler.go writeChunk), so the race
// detector occasionally reports a data race inside tusd itself. CI runs this
// file without -race (see validate.yml) until upstream fixes it.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A mobile client that changes networks leaves a PATCH whose body never
// finishes. A new request for the same upload must interrupt it through tusd's
// lock-release protocol instead of waiting for the HTTP read timeout.
func TestStalledPatchDoesNotBlockResumeOrRestart(t *testing.T) {
	fixture := newGatewayFixture(t)
	token := fixture.token(userA)
	content := []byte("ten-bytes!")
	session, path := fixture.startTusUpload(token, "stalled-patch", content)

	stalledDone := fixture.stallPatch(path, session.UploadToken, 6, content[:2])
	waitForReceived(t, fixture, session.ID, 2)

	resumed := make(chan *http.Response, 1)
	go func() { resumed <- fixture.patch(path, session.UploadToken, 2, content[2:6]) }()
	select {
	case response := <-resumed:
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("resumed PATCH status = %d", response.StatusCode)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("resumed PATCH was blocked by a stalled PATCH for the same upload")
	}
	select {
	case status := <-stalledDone:
		if strings.Contains(status, " 204 ") {
			t.Fatalf("stalled PATCH unexpectedly succeeded: %q", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled PATCH was not interrupted")
	}
}

func TestRestartInterruptsStalledPatch(t *testing.T) {
	fixture := newGatewayFixture(t)
	token := fixture.token(userA)
	content := []byte("ten-bytes!")
	session, path := fixture.startTusUpload(token, "stalled-restart", content)

	fixture.stallPatch(path, session.UploadToken, 6, content[:2])
	waitForReceived(t, fixture, session.ID, 2)

	restarted := make(chan *http.Response, 1)
	go func() {
		restarted <- fixture.request(http.MethodPost, "/v1/upload-sessions/"+session.ID+"/restart", token, nil, nil)
	}()
	select {
	case response := <-restarted:
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("restart status = %d: %s", response.StatusCode, body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("restart was blocked by a stalled PATCH")
	}
}

// stallPatch writes a PATCH whose declared body is longer than what is sent,
// like a phone that lost its network mid-chunk. The connection stays open until
// the test ends. The returned channel receives the final status line, if any.
func (fixture *gatewayFixture) stallPatch(path, uploadToken string, declared int, sent []byte) <-chan string {
	fixture.t.Helper()
	connection, err := net.Dial("tcp", fixture.httpServer.Listener.Addr().String())
	if err != nil {
		fixture.t.Fatal(err)
	}
	fixture.t.Cleanup(func() { connection.Close() })
	request := fmt.Sprintf("PATCH %s HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer %s\r\n"+
		"Tus-Resumable: 1.0.0\r\nUpload-Offset: 0\r\nContent-Type: application/offset+octet-stream\r\n"+
		"Content-Length: %d\r\n\r\n", path, uploadToken, declared)
	if _, err := connection.Write(append([]byte(request), sent...)); err != nil {
		fixture.t.Fatal(err)
	}
	status := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(connection).ReadString('\n')
		status <- strings.TrimSpace(line)
	}()
	return status
}

func waitForReceived(t *testing.T, fixture *gatewayFixture, id string, size int64) {
	t.Helper()
	staging := filepath.Join(fixture.mediaRoot, ".staging", "tus", id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if stat, err := os.Stat(staging); err == nil && stat.Size() >= size {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("staging payload never reached %d bytes", size)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
