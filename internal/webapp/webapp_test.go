package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func TestWebAppServesFilesWithStrictCSP(t *testing.T) {
	handler := Handler()
	for path, wantType := range map[string]string{
		"/app/":                     "text/html",
		"/app/app.js":               "javascript",
		"/app/app.css":              "text/css",
		"/app/manifest.webmanifest": "application/manifest+json",
		"/app/icon-192.png":         "image/png",
	} {
		response := serve(handler, http.MethodGet, path)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
		if got := response.Header().Get("Content-Type"); !strings.Contains(got, wantType) {
			t.Fatalf("%s content type = %q", path, got)
		}
		csp := response.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s CSP = %q", path, csp)
		}
	}
	if response := serve(handler, http.MethodPost, "/app/"); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", response.Code)
	}
	if response := serve(handler, http.MethodGet, "/app/missing.js"); response.Code != http.StatusNotFound {
		t.Fatalf("missing file status = %d", response.Code)
	}
	if response := serve(handler, http.MethodGet, "/app"); response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != "/app/" {
		t.Fatalf("/app redirect = %d %q", response.Code, response.Header().Get("Location"))
	}
}

func TestIndexHasNoInlineScriptsOrStyles(t *testing.T) {
	body := serve(Handler(), http.MethodGet, "/app/").Body.String()
	for _, forbidden := range []string{"<script>", "<style", " style=", "onclick=", "onload=", "javascript:"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("index.html contains %q, which the CSP would block", forbidden)
		}
	}
}

func TestRootRedirectsOnlyRoot(t *testing.T) {
	handler := RedirectRoot()
	if response := serve(handler, http.MethodGet, "/"); response.Code != http.StatusFound || response.Header().Get("Location") != BasePath {
		t.Fatalf("root redirect = %d %q", response.Code, response.Header().Get("Location"))
	}
	if response := serve(handler, http.MethodGet, "/wp-admin"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", response.Code)
	}
}

func TestStaticFilesRevalidateWithETag(t *testing.T) {
	handler := Handler()
	first := serve(handler, http.MethodGet, "/app/app.js")
	etag := first.Header().Get("ETag")
	if etag == "" || first.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers = %v", first.Header())
	}
	request := httptest.NewRequest(http.MethodGet, "/app/app.js", nil)
	request.Header.Set("If-None-Match", etag)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d", recorder.Code)
	}
	if serve(handler, http.MethodGet, "/app/").Header().Get("ETag") == "" {
		t.Fatal("index has no ETag")
	}
}
