package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token lifetimes match axiom_backend/auth/security.py.
const (
	// AccessTokenLifetime is the default access token lifetime (8 hours).
	AccessTokenLifetime = 480 * time.Minute
	// RememberMeLifetime is applied when the client sets remember_me=true at login (30 days).
	RememberMeLifetime = 30 * 24 * time.Hour
)

// ErrInvalidToken is returned when a token fails signature verification,
// has expired, or lacks the required "sub" claim.
var ErrInvalidToken = errors.New("auth: invalid token")

// Claims is the axiom JWT payload shape. "sub" holds the username (not
// the numeric user id — this matches the Python implementation). The
// "remember_me" flag is persisted in the token so refresh flows can
// choose the right expiry without a database round-trip.
type Claims struct {
	RememberMe bool `json:"remember_me,omitempty"`
	jwt.RegisteredClaims
}

// Signer wraps a single HS256 symmetric key. Instances are goroutine-safe.
type Signer struct {
	secret []byte
	now    func() time.Time
}

// NewSigner constructs a signer. secret must be non-empty; axiom-ng
// refuses to run with a blank JWT secret on purpose (unlike the Python
// backend's hard-coded fallback, which exists only for tests).
func NewSigner(secret string) (*Signer, error) {
	if secret == "" {
		return nil, errors.New("auth: JWT signing secret is required (set JWT_SECRET_KEY)")
	}
	return &Signer{secret: []byte(secret), now: time.Now}, nil
}

// Issue mints a signed token for the given username. ttl must be
// positive; for parity with Python, pass AccessTokenLifetime or
// RememberMeLifetime.
func (s *Signer) Issue(username string, rememberMe bool, ttl time.Duration) (string, error) {
	if username == "" {
		return "", errors.New("auth: username required")
	}
	if ttl <= 0 {
		return "", errors.New("auth: ttl must be positive")
	}
	now := s.now()
	claims := Claims{
		RememberMe: rememberMe,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	str, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return str, nil
}

// Verify parses and validates raw, returning the extracted claims on
// success. Signature, expiry, and the presence of a subject are checked.
func (s *Signer) Verify(raw string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method %q", ErrInvalidToken, t.Method.Alg())
		}
		return s.secret, nil
	}, jwt.WithTimeFunc(s.now))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: missing subject", ErrInvalidToken)
	}
	return claims, nil
}
