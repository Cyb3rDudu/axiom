package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
)

type stubLookup struct {
	byName map[string]authctx.User
	err    error
}

func (s *stubLookup) GetUserByUsername(_ context.Context, name string) (authctx.User, error) {
	if s.err != nil {
		return authctx.User{}, s.err
	}
	u, ok := s.byName[name]
	if !ok {
		return authctx.User{}, errors.New("not found")
	}
	return u, nil
}

func mustSigner(t *testing.T) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner("unit-test-secret")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

func TestUserContextAttachesClaimsFromCookie(t *testing.T) {
	t.Parallel()
	s := mustSigner(t)
	tok, _ := s.Issue("alice", false, time.Hour)
	lookup := &stubLookup{byName: map[string]authctx.User{"alice": {ID: 42, Username: "alice"}}}

	mw := UserContext(UserContextConfig{Signer: s, UserLookup: lookup})
	var got authctx.User
	var ok bool
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = authctx.UserFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: tok})
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !ok {
		t.Fatal("user not attached")
	}
	if got.ID != 42 || got.Username != "alice" {
		t.Errorf("user: got %+v", got)
	}
}

func TestUserContextAttachesFromAuthorizationHeader(t *testing.T) {
	t.Parallel()
	s := mustSigner(t)
	tok, _ := s.Issue("bob", false, time.Hour)
	lookup := &stubLookup{byName: map[string]authctx.User{"bob": {Username: "bob"}}}

	mw := UserContext(UserContextConfig{Signer: s, UserLookup: lookup})
	var username string
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := authctx.UsernameFrom(r.Context()); ok {
			username = u
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if username != "bob" {
		t.Errorf("username: got %q", username)
	}
}

func TestUserContextDoesNotFailOnMissingToken(t *testing.T) {
	t.Parallel()
	s := mustSigner(t)
	mw := UserContext(UserContextConfig{Signer: s})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if _, ok := authctx.UserFrom(r.Context()); ok {
			t.Error("unexpected user attached on unauthenticated request")
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/public", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Error("handler should be invoked")
	}
}

func TestUserContextIgnoresInvalidToken(t *testing.T) {
	t.Parallel()
	s := mustSigner(t)
	mw := UserContext(UserContextConfig{Signer: s})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authctx.UserFrom(r.Context()); ok {
			t.Error("invalid token should not attach user")
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: "garbage"})
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestUserContextSkipsLookupFailure(t *testing.T) {
	t.Parallel()
	s := mustSigner(t)
	tok, _ := s.Issue("missing", false, time.Hour)
	lookup := &stubLookup{err: errors.New("db down")}

	mw := UserContext(UserContextConfig{Signer: s, UserLookup: lookup})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authctx.UserFrom(r.Context()); ok {
			t.Error("user should not be attached when lookup fails")
		}
		if _, ok := authctx.UsernameFrom(r.Context()); !ok {
			t.Error("username should still be attached from JWT")
		}
		if _, ok := authctx.ClaimsFrom(r.Context()); !ok {
			t.Error("claims should be attached")
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: tok})
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestRequireAuth401WithoutUser(t *testing.T) {
	t.Parallel()
	h := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not run")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestRequireAuthPassesWithUser(t *testing.T) {
	t.Parallel()
	h := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req = req.WithContext(authctx.WithUser(req.Context(), authctx.User{ID: 1}))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Errorf("status: got %d", rec.Code)
	}
}

func TestCSRFAllowsSafeMethods(t *testing.T) {
	t.Parallel()
	h := CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET should bypass CSRF, got %d", rec.Code)
	}
}

func TestCSRFRejectsMissingTokens(t *testing.T) {
	t.Parallel()
	h := CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/change-password", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing csrf: got %d", rec.Code)
	}
}

func TestCSRFRejectsMismatch(t *testing.T) {
	t.Parallel()
	h := CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/change-password", nil)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookie, Value: "cookie-val"})
	req.Header.Set(auth.CSRFHeader, "header-val")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mismatch: got %d", rec.Code)
	}
}

func TestCSRFPassesOnMatchingTokens(t *testing.T) {
	t.Parallel()
	called := false
	h := CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/change-password", nil)
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookie, Value: "same"})
	req.Header.Set(auth.CSRFHeader, "same")
	h.ServeHTTP(rec, req)
	if !called {
		t.Errorf("handler not invoked despite matching tokens (code %d)", rec.Code)
	}
}
