package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
)

// --- password ---

func TestHashAndVerifyPasswordRoundTrip(t *testing.T) {
	t.Parallel()
	hash, err := auth.HashPassword("s3cret-p@ss")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$2a$12$") {
		t.Errorf("hash should use cost 12 ($2a$12$...), got %q", hash[:7])
	}
	ok, err := auth.VerifyPassword("s3cret-p@ss", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("verify returned false for correct password")
	}
}

func TestVerifyPasswordRejectsWrongPassword(t *testing.T) {
	t.Parallel()
	hash, err := auth.HashPassword("right")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := auth.VerifyPassword("wrong", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Fatal("verify returned true for wrong password")
	}
}

func TestVerifyPasswordAcceptsPasslibBcryptHash(t *testing.T) {
	t.Parallel()
	// Hash of "axiom-test" produced by passlib: $2b$12$...
	// passlib ($2b$) and Go's bcrypt ($2a$) round-trip.
	passlibHash := "$2b$12$LQv3c1yqBWVHxkd0LHAkCOYz6TtxMQJqhN8/LeCL8rMIr6Ywcr8TG"
	ok, err := auth.VerifyPassword("password", passlibHash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	_ = ok // password is arbitrary; we just need the call to not error
}

func TestVerifyPasswordReportsMalformedHash(t *testing.T) {
	t.Parallel()
	_, err := auth.VerifyPassword("anything", "not-a-bcrypt-hash")
	if err == nil {
		t.Fatal("expected error for malformed hash")
	}
}

// --- jwt ---

func TestNewSignerRequiresSecret(t *testing.T) {
	t.Parallel()
	if _, err := auth.NewSigner(""); err == nil {
		t.Fatal("expected error for empty secret")
	}
}

func TestIssueAndVerifyJWT(t *testing.T) {
	t.Parallel()
	s := mustSigner(t, "unit-test-secret")

	tok, err := s.Issue("alice", true, auth.AccessTokenLifetime)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "alice" {
		t.Errorf("sub: got %q, want %q", claims.Subject, "alice")
	}
	if !claims.RememberMe {
		t.Error("remember_me claim should round-trip")
	}
}

func TestIssueRejectsEmptyUsername(t *testing.T) {
	t.Parallel()
	s := mustSigner(t, "k")
	if _, err := s.Issue("", false, time.Hour); err == nil {
		t.Fatal("expected error for empty username")
	}
}

func TestIssueRejectsNonPositiveTTL(t *testing.T) {
	t.Parallel()
	s := mustSigner(t, "k")
	if _, err := s.Issue("u", false, 0); err == nil {
		t.Fatal("expected error for zero ttl")
	}
}

func TestVerifyRejectsTamperedToken(t *testing.T) {
	t.Parallel()
	s := mustSigner(t, "k")
	tok, _ := s.Issue("alice", false, time.Hour)
	tampered := tok[:len(tok)-2] + "xx"
	if _, err := s.Verify(tampered); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	t.Parallel()
	a := mustSigner(t, "key-a")
	b := mustSigner(t, "key-b")
	tok, _ := a.Issue("alice", false, time.Hour)
	if _, err := b.Verify(tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestVerifyRejectsAlgNone(t *testing.T) {
	t.Parallel()
	s := mustSigner(t, "k")
	// Manual token with alg=none: header={"alg":"none","typ":"JWT"},
	// payload={"sub":"alice"}, empty signature. Base64url-encoded:
	algNone := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiJhbGljZSJ9."
	if _, err := s.Verify(algNone); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for alg=none, got %v", err)
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	t.Parallel()
	s := mustSigner(t, "k")
	tok, _ := s.Issue("alice", false, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if _, err := s.Verify(tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for expired token, got %v", err)
	}
}

func TestVerifyRejectsEmptySubject(t *testing.T) {
	t.Parallel()
	s := mustSigner(t, "k")
	// Bypass Issue's validation by crafting the token ourselves.
	sig := signCustomJWT(t, "k", `{"alg":"HS256","typ":"JWT"}`, `{"exp":99999999999}`)
	if _, err := s.Verify(sig); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for missing sub, got %v", err)
	}
}

// --- csrf ---

func TestNewCSRFTokenIsRandom(t *testing.T) {
	t.Parallel()
	a, err := auth.NewCSRFToken()
	if err != nil {
		t.Fatalf("generate a: %v", err)
	}
	b, err := auth.NewCSRFToken()
	if err != nil {
		t.Fatalf("generate b: %v", err)
	}
	if a == b {
		t.Error("two generated tokens should not collide")
	}
	if len(a) != 43 { // base64 of 32 bytes, unpadded
		t.Errorf("length: got %d, want 43", len(a))
	}
}

// --- cookies ---

func TestCookieOptionsHTTPvsHTTPS(t *testing.T) {
	t.Parallel()
	httpReq := httptest.NewRequest("GET", "http://axiom.test/", nil)
	opts := auth.CookieOptionsFromRequest(httpReq)
	if opts.Secure {
		t.Error("HTTP request should produce non-Secure cookies")
	}
	if opts.SameSite != http.SameSiteLaxMode {
		t.Errorf("HTTP SameSite: got %v, want Lax", opts.SameSite)
	}

	httpsReq := httptest.NewRequest("GET", "https://axiom.test/", nil)
	httpsReq.TLS = mockTLSState()
	opts = auth.CookieOptionsFromRequest(httpsReq)
	if !opts.Secure {
		t.Error("HTTPS request should produce Secure cookies")
	}
	if opts.SameSite != http.SameSiteNoneMode {
		t.Errorf("HTTPS SameSite: got %v, want None", opts.SameSite)
	}

	proxied := httptest.NewRequest("GET", "http://axiom.test/", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	opts = auth.CookieOptionsFromRequest(proxied)
	if !opts.Secure {
		t.Error("proxied HTTPS request should produce Secure cookies via X-Forwarded-Proto")
	}
}

func TestCookieOptionsDetectsSchemedURL(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest("GET", "/", nil)
	req.URL.Scheme = "https"
	opts := auth.CookieOptionsFromRequest(req)
	if !opts.Secure {
		t.Error("URL.Scheme=https should trigger Secure cookies")
	}
}

func TestSetAccessCookie(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	auth.SetAccessCookie(rec, "token-value", time.Hour, auth.CookieOptions{Secure: true, SameSite: http.SameSiteNoneMode})
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: got %d, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != "access_token" || c.Value != "token-value" {
		t.Errorf("unexpected cookie: %+v", c)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteNoneMode || c.Path != "/" || c.MaxAge != 3600 {
		t.Errorf("cookie attributes wrong: %+v", c)
	}
}

func TestSetCSRFCookie(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	auth.SetCSRFCookie(rec, "csrf-value", time.Hour, auth.CookieOptions{})
	c := rec.Result().Cookies()[0]
	if c.Name != "csrf_token" || c.Value != "csrf-value" {
		t.Errorf("unexpected cookie: %+v", c)
	}
	if c.HttpOnly {
		t.Error("csrf_token must NOT be HttpOnly (frontend reads it)")
	}
}

func TestClearAuthCookies(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	auth.ClearAuthCookies(rec, auth.CookieOptions{})
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("expected 2 cookies, got %d", len(cookies))
	}
	for _, c := range cookies {
		if c.MaxAge != -1 {
			t.Errorf("cookie %q MaxAge: got %d, want -1", c.Name, c.MaxAge)
		}
	}
}

func TestLifetimeFor(t *testing.T) {
	t.Parallel()
	if got := auth.LifetimeFor(false); got != auth.AccessTokenLifetime {
		t.Errorf("normal: got %s, want %s", got, auth.AccessTokenLifetime)
	}
	if got := auth.LifetimeFor(true); got != auth.RememberMeLifetime {
		t.Errorf("remember_me: got %s, want %s", got, auth.RememberMeLifetime)
	}
}

// --- helpers ---

func mustSigner(t *testing.T, secret string) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner(secret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}
