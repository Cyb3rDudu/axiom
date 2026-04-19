package auth

import (
	"crypto/rand"
	"encoding/base64"
)

// NewCSRFToken returns a URL-safe random string matching Python's
// secrets.token_urlsafe(32). Python emits base64(urlsafe, unpadded) of
// 32 random bytes, producing a 43-character token.
func NewCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
