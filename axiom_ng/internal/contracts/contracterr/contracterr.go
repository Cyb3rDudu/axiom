// Package contracterr is the typed error world of the component seams
// (F03, #297). Every Library/Store contract method reports failures as
// these types — never as bare strings or implementation-internal errors.
//
// Rules binding all seam implementers and consumers (the #264/#271/#294
// lessons):
//
//   - Implementations MAP internal errors into contract errors (wrapping
//     the original as Cause); consumers CLASSIFY via ClassOf/Retryable and
//     errors.As — never by parsing error strings. No error-text matching
//     across seam boundaries, in either direction.
//   - Retryable() is true for Unavailable only. Deadline means the
//     caller's budget was exhausted — a retry needs a fresh (extended)
//     deadline, so automatic same-deadline retries must not fire.
//   - Idempotency key/payload divergence is an *IdempotencyMismatch
//     (classified Conflict): same key with a different payload can never
//     silently return the earlier operation's result.
//
// This package must stay transport-neutral (see internal/contracts lint):
// stdlib imports only.
package contracterr

import (
	"errors"
	"fmt"
)

// Component names the seam an error crossed (ADR-0001 terminology).
type Component string

const (
	ComponentLibrary Component = "library"
	ComponentStore   Component = "store"
)

// Class is the closed set of error classes. HTTP adapters (F11) map each
// class to exactly one status: NotFound→404, InvalidArgument→400,
// Conflict→409, Unavailable→503, Deadline→504, Internal→500.
type Class string

const (
	ClassNotFound        Class = "not_found"
	ClassInvalidArgument Class = "invalid_argument"
	ClassConflict        Class = "conflict"
	ClassUnavailable     Class = "unavailable" // retryable
	ClassDeadline        Class = "deadline"
	ClassInternal        Class = "internal"
)

// RetryableClasses reports whether errors of class c should be retried
// (with backoff) by seam callers. Unavailable: yes. Deadline: only with a
// fresh, extended budget — not an automatic retry. Everything else is
// terminal for the caller's purpose.
func (c Class) Retryable() bool { return c == ClassUnavailable }

// Error is the typed seam error. Component says which seam it crossed,
// Class drives retry/status decisions, Message is human context (never
// parsed), Cause optionally carries the implementation's original error.
type Error struct {
	Component Component
	Class     Class
	Message   string
	Cause     error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %s: %v", e.Component, e.Class, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s: %s", e.Component, e.Class, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// Retryable reports whether the error's class is retryable.
func (e *Error) Retryable() bool { return e.Class.Retryable() }

// New builds a typed error without a cause.
func New(c Component, class Class, msg string) *Error {
	return &Error{Component: c, Class: class, Message: msg}
}

// Wrap builds a typed error around an implementation error (the mapping
// direction implementations use — internal cause in, contract error out).
func Wrap(c Component, class Class, cause error, msg string) *Error {
	return &Error{Component: c, Class: class, Message: msg, Cause: cause}
}

// IdempotencyMismatch is its own type (not just a Class field): a stored
// idempotency key was replayed with a DIFFERENT payload. It classifies as
// Conflict and is never retryable — the caller must either reuse the
// original payload byte-for-byte or pick a new key.
type IdempotencyMismatch struct {
	Component Component
	// Key is the idempotency key whose stored payload diverged.
	Key string
}

func (e *IdempotencyMismatch) Error() string {
	return fmt.Sprintf("%s: conflict: idempotency key %q was already used with a different payload", e.Component, e.Key)
}

// ClassOf returns the error's contract class, if the error (or anything
// it wraps) is a contract error. Idempotency mismatches report
// ClassConflict.
func ClassOf(err error) (Class, bool) {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Class, true
	}
	var im *IdempotencyMismatch
	if errors.As(err, &im) {
		return ClassConflict, true
	}
	return "", false
}

// Retryable reports whether err is a contract error of a retryable
// class. Non-contract errors are not retryable (unknown = do not hammer).
func Retryable(err error) bool {
	class, ok := ClassOf(err)
	return ok && class.Retryable()
}
