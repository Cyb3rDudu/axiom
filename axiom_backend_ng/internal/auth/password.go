// Package auth owns password hashing, JWT signing/verification, CSRF
// token generation, and HTTP cookie helpers for axiom-ng.
//
// Parity with the Python backend (axiom_backend/auth/*) is mandatory:
// cookies signed by the Python backend must authenticate under axiom-ng
// and vice versa.
package auth

import "golang.org/x/crypto/bcrypt"

// BcryptCost matches passlib's default (12) so fresh hashes produced by
// axiom-ng and the Python backend are indistinguishable.
const BcryptCost = 12

// HashPassword returns a bcrypt hash of the plaintext password at
// BcryptCost. Go's bcrypt emits the $2a$ variant; passlib emits $2b$.
// Both are verified by golang.org/x/crypto/bcrypt.CompareHashAndPassword,
// so hashes round-trip between the two backends.
func HashPassword(plaintext string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plaintext), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword returns true when plaintext matches hashed (bcrypt).
// Errors other than a mismatch are surfaced so callers can distinguish
// transient bcrypt failures from bad credentials.
func VerifyPassword(plaintext, hashed string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plaintext))
	if err == nil {
		return true, nil
	}
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return false, nil
	}
	return false, err
}
