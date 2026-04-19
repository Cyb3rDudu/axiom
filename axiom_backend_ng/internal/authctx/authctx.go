// Package authctx holds the request-context keys for authenticated state
// so that both the server middleware and the api handlers can read the
// current user without creating an import cycle.
package authctx

import (
	"context"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
)

// User is the minimal user shape exposed through request context.
// Full user CRUD lives in internal/repo.
type User struct {
	ID       int32
	Username string
	IsAdmin  bool
	IsActive bool
}

type ctxKey int

const (
	keyUser ctxKey = iota
	keyUsername
	keyClaims
)

// WithUser attaches a user to ctx.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, keyUser, u)
}

// WithUsername attaches the JWT subject without requiring a DB lookup.
func WithUsername(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, keyUsername, username)
}

// WithClaims attaches the full JWT claims for diagnostics.
func WithClaims(ctx context.Context, c *auth.Claims) context.Context {
	return context.WithValue(ctx, keyClaims, c)
}

// UserFrom returns the attached user, if any.
func UserFrom(ctx context.Context) (User, bool) {
	v, ok := ctx.Value(keyUser).(User)
	return v, ok
}

// UsernameFrom returns the attached JWT subject.
func UsernameFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyUsername).(string)
	return v, ok
}

// ClaimsFrom returns the attached JWT claims.
func ClaimsFrom(ctx context.Context) (*auth.Claims, bool) {
	v, ok := ctx.Value(keyClaims).(*auth.Claims)
	return v, ok
}
