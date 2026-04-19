package auth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"testing"
)

// mockTLSState returns a zero-value *tls.ConnectionState so tests don't
// need a real TLS handshake to mark a request as HTTPS.
func mockTLSState() *tls.ConnectionState {
	return &tls.ConnectionState{}
}

// signCustomJWT produces an HS256-signed JWT from raw header and claim
// JSON strings, bypassing the Signer's validation. Used to craft tokens
// with missing claims for negative test cases.
func signCustomJWT(t *testing.T, secret, header, claims string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	payload := enc([]byte(header)) + "." + enc([]byte(claims))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "." + enc(mac.Sum(nil))
}
