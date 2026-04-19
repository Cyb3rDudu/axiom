package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
)

type ctxKey int

const (
	ctxKeyUsername ctxKey = iota
	ctxKeyClaims
)

// UserResolver loads a user record by username. Implementations typically
// query the database; pass nil to skip DB lookups during middleware
// testing or for endpoints that only need the JWT subject.
type UserResolver interface {
	GetUserByUsername(ctx context.Context, username string) (User, error)
}

// User is the minimal user shape exposed through request context.
// Full user CRUD lives in internal/users once sqlc is wired.
type User struct {
	ID       int32
	Username string
	IsAdmin  bool
	IsActive bool
}

// UserContextConfig configures the UserContext middleware.
type UserContextConfig struct {
	Signer *auth.Signer
	// UserLookup, if non-nil, is called with the JWT subject and
	// populates the User record on the request context. If it returns
	// an error the request proceeds unauthenticated — matching
	// Python's user_context_middleware behaviour.
	UserLookup UserResolver
}

// UserContext extracts a token from the access_token cookie or
// Authorization: Bearer header, validates it, and attaches the
// resulting claims (and optionally a User record) to the request
// context. Missing or invalid tokens do NOT fail the request; they
// simply leave the context empty so RequireAuth can decide.
func UserContext(cfg UserContextConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractToken(r)
			if raw == "" {
				next.ServeHTTP(w, r)
				return
			}
			claims, err := cfg.Signer.Verify(raw)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyClaims, claims)
			ctx = context.WithValue(ctx, ctxKeyUsername, claims.Subject)
			if cfg.UserLookup != nil {
				if user, err := cfg.UserLookup.GetUserByUsername(ctx, claims.Subject); err == nil {
					ctx = WithUser(ctx, user)
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAuth short-circuits requests without a resolved user with HTTP
// 401. Must be mounted after UserContext.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFromContext(r.Context()); !ok {
			writeJSONError(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CSRF enforces the double-submit cookie pattern on POST/PUT/DELETE/PATCH
// requests: csrf_token cookie must equal the X-CSRF-Token header.
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			cookie, err := r.Cookie(auth.CSRFCookie)
			header := r.Header.Get(auth.CSRFHeader)
			if err != nil || cookie.Value == "" || header == "" {
				writeJSONError(w, http.StatusForbidden, "CSRF token missing")
				return
			}
			if cookie.Value != header {
				writeJSONError(w, http.StatusForbidden, "CSRF token mismatch")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// WithUser stores user in ctx. Exported for tests and for handlers that
// want to inject a user manually (e.g. API-key auth flows).
func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, ctxKeyUser, user)
}

// UserFromContext returns the user attached by UserContext (or a manual
// WithUser call). ok is false if no user is attached.
func UserFromContext(ctx context.Context) (User, bool) {
	v, ok := ctx.Value(ctxKeyUser).(User)
	return v, ok
}

// UsernameFromContext returns the JWT subject attached by UserContext.
// Useful when an endpoint needs the username even if the DB lookup
// (UserLookup) was skipped or failed.
func UsernameFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyUsername).(string)
	return v, ok
}

// ClaimsFromContext returns the full JWT claims. Typically callers only
// need the username; this is here for diagnostics.
func ClaimsFromContext(ctx context.Context) (*auth.Claims, bool) {
	v, ok := ctx.Value(ctxKeyClaims).(*auth.Claims)
	return v, ok
}

const ctxKeyUser ctxKey = 2

func extractToken(r *http.Request) string {
	if c, err := r.Cookie(auth.AccessTokenCookie); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}
