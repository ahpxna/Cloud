package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrometheusQuoteEscapesLabels(t *testing.T) {
	got := prometheusQuote("a\"b\\c\n")
	want := `"a\"b\\c\n"`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLastBackupSuccessReportsZeroForMissingOrMalformedStatus(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "last-success")
	if got := lastBackupSuccess(path); got != 0 {
		t.Fatalf("missing status = %d, want 0", got)
	}
	for _, contents := range []string{"not-a-number\n", "-5\n", strings.Repeat("9", 100)} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := lastBackupSuccess(path); got != 0 {
			t.Fatalf("status %q = %d, want 0", contents, got)
		}
	}
	if err := os.WriteFile(path, []byte("1791331200\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastBackupSuccess(path); got != 1791331200 {
		t.Fatalf("valid status = %d", got)
	}
}

func TestGatewayReadyRequiresOKWithoutFollowingRedirects(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if status == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	exp := &exporter{gatewayReadyURL: server.URL + "/readyz", httpClient: &http.Client{
		Timeout:       time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	for _, testCase := range []struct {
		status int
		want   bool
	}{{http.StatusOK, true}, {http.StatusServiceUnavailable, false}, {http.StatusFound, false}} {
		status = testCase.status
		if got := exp.gatewayReady(context.Background()); got != testCase.want {
			t.Fatalf("status %d ready=%v, want %v", testCase.status, got, testCase.want)
		}
	}
	exp.gatewayReadyURL = "http://127.0.0.1:1/readyz"
	if exp.gatewayReady(context.Background()) {
		t.Fatal("unreachable gateway reported ready")
	}
}
