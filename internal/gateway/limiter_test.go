package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPatchLimiterEnforcesGlobalAndPerUserLimits(t *testing.T) {
	t.Parallel()
	limiter := newPatchLimiter(3, 2)
	for slot := 1; slot <= 2; slot++ {
		if !limiter.Acquire("user-a") {
			t.Fatalf("user-a should receive slot %d", slot)
		}
	}
	if limiter.Acquire("user-a") {
		t.Fatal("user-a exceeded per-user limit")
	}
	if !limiter.Acquire("user-b") {
		t.Fatal("user-b should receive remaining global slot")
	}
	if limiter.Acquire("user-c") {
		t.Fatal("global limit was exceeded")
	}
	limiter.Release("user-a")
	if !limiter.Acquire("user-c") {
		t.Fatal("released slot was not reusable")
	}
	limiter.Release("user-a")
	limiter.Release("user-b")
	limiter.Release("user-c")
}

func TestPlainHTTPFromFrontProxyRedirectsToHTTPS(t *testing.T) {
	handler := redirectPlainHTTP("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	serve := func(method, target, proto string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, nil)
		request.Host = "family-photos.example.ts.net:80"
		if proto != "" {
			request.Header.Set("X-Forwarded-Proto", proto)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	for target, want := range map[string]string{
		"/__plain-http/app/?a=1": "https://family-photos.example.ts.net/app/?a=1",
		"/__plain-http":          "https://family-photos.example.ts.net/",
		"/__plain-http/":         "https://family-photos.example.ts.net/",
	} {
		if response := serve(http.MethodGet, target, ""); response.Code != http.StatusPermanentRedirect ||
			response.Header().Get("Location") != want {
			t.Fatalf("%s redirect = %d %q, want %q", target, response.Code, response.Header().Get("Location"), want)
		}
	}
	if response := serve(http.MethodGet, "/app/?a=1", "http"); response.Code != http.StatusPermanentRedirect ||
		response.Header().Get("Location") != "https://family-photos.example.ts.net/app/?a=1" {
		t.Fatalf("X-Forwarded-Proto redirect = %d %q", response.Code, response.Header().Get("Location"))
	}
	if response := serve(http.MethodPost, "/__plain-http/v1/direct-uploads", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("plain-HTTP upload status = %d, want 400", response.Code)
	}
	if response := serve(http.MethodPost, "/v1/auth/login", "http"); response.Code != http.StatusBadRequest {
		t.Fatalf("plain-HTTP POST status = %d, want 400", response.Code)
	}
	for _, proto := range []string{"", "https"} {
		if response := serve(http.MethodGet, "/app/", proto); response.Code != http.StatusTeapot {
			t.Fatalf("proto %q was not passed through: %d", proto, response.Code)
		}
	}
}

func TestPlainHTTPRedirectUsesCanonicalHost(t *testing.T) {
	handler := redirectPlainHTTP("family-photos.example.ts.net", http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodGet, "/__plain-http/app/", nil)
	request.Host = "family-photos"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPermanentRedirect || recorder.Header().Get("Location") != "https://family-photos.example.ts.net/app/" {
		t.Fatalf("short-name redirect = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}
