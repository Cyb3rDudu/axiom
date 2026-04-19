package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestCORSOriginsWildcardTrigger(t *testing.T) {
	t.Parallel()
	got := corsOrigins(envMap(map[string]string{"ALLOW_CORS_WILDCARD": "true"}))
	if len(got) != 1 || got[0] != "*" {
		t.Errorf("wildcard via ALLOW_CORS_WILDCARD: got %v, want [*]", got)
	}
	got = corsOrigins(envMap(map[string]string{"CORS_ALLOWED_ORIGINS": "*"}))
	if len(got) != 1 || got[0] != "*" {
		t.Errorf("wildcard via CORS_ALLOWED_ORIGINS=*: got %v, want [*]", got)
	}
}

func TestCORSOriginsIncludesDefaultsAndCustom(t *testing.T) {
	t.Parallel()
	got := corsOrigins(envMap(map[string]string{
		"CORS_ALLOWED_ORIGINS": "https://app.example.com, https://admin.example.com",
	}))
	hasApp := false
	hasAdmin := false
	hasDefault := false
	for _, o := range got {
		switch o {
		case "https://app.example.com":
			hasApp = true
		case "https://admin.example.com":
			hasAdmin = true
		case "http://localhost:3000":
			hasDefault = true
		}
	}
	if !hasApp || !hasAdmin || !hasDefault {
		t.Errorf("missing expected origins: %+v", got)
	}
}

func TestCORSOriginsDeduplicates(t *testing.T) {
	t.Parallel()
	got := corsOrigins(envMap(map[string]string{
		"CORS_ALLOWED_ORIGINS": "http://localhost:3000,http://localhost:3000",
	}))
	count := 0
	for _, o := range got {
		if o == "http://localhost:3000" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("duplicate origin should be collapsed, saw %d copies", count)
	}
}

func TestCORSMiddlewareReflectsAllowedOrigin(t *testing.T) {
	t.Parallel()
	mw := CORS(envMap(map[string]string{"CORS_ALLOWED_ORIGINS": "https://app.example.com"}))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("allow-origin: got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("allow-credentials: got %q", got)
	}
}

func TestCORSMiddlewareDropsDisallowedOrigin(t *testing.T) {
	t.Parallel()
	mw := CORS(envMap(map[string]string{}))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin leaked: %q", got)
	}
}

func TestCORSMiddlewareWildcardReflectsOrigin(t *testing.T) {
	t.Parallel()
	mw := CORS(envMap(map[string]string{"ALLOW_CORS_WILDCARD": "true"}))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Origin", "https://anything.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Wildcard + credentials means mirror the origin, not emit "*".
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://anything.example.com" {
		t.Errorf("wildcard should mirror origin, got %q", got)
	}
}

func TestCORSMiddlewarePreflight(t *testing.T) {
	t.Parallel()
	mw := CORS(envMap(map[string]string{"CORS_ALLOWED_ORIGINS": "https://app.example.com"}))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream handler should not run for preflight")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/me", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status: got %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Max-Age") != "86400" {
		t.Errorf("max-age: got %q", rec.Header().Get("Access-Control-Max-Age"))
	}
	if rec.Header().Get("Access-Control-Allow-Methods") != "*" {
		t.Errorf("methods: got %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}
}

func TestCORSMiddlewareIgnoresOPTIONSWithoutRequestMethod(t *testing.T) {
	// A bare OPTIONS with no Access-Control-Request-Method is not a
	// preflight; the middleware should pass it through.
	t.Parallel()
	called := false
	mw := CORS(envMap(map[string]string{}))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodOptions, "/api/me", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !called {
		t.Error("downstream should receive non-preflight OPTIONS")
	}
}

func TestCORSFallsBackToOSGetenv(t *testing.T) {
	t.Setenv("ALLOW_CORS_WILDCARD", "true")
	mw := CORS(nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://x.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://x.example" {
		t.Errorf("fallback to os.Getenv should mirror origin, got %q", got)
	}
}
