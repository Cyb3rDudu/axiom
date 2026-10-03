// contracterr_test.go — the typed error world's own checks (#297):
// classification through wrapping, the retryable rule, idempotency
// mismatch as its own Conflict type, and the no-string-matching rule
// being structural (class comparison, never message comparison).
package contracterr

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassOfAndRetryable(t *testing.T) {
	cases := []struct {
		err   error
		class Class
		retry bool
	}{
		{New(ComponentLibrary, ClassNotFound, "x"), ClassNotFound, false},
		{New(ComponentLibrary, ClassInvalidArgument, "x"), ClassInvalidArgument, false},
		{New(ComponentStore, ClassConflict, "x"), ClassConflict, false},
		{New(ComponentStore, ClassUnavailable, "x"), ClassUnavailable, true},
		{New(ComponentStore, ClassDeadline, "x"), ClassDeadline, false},
		{New(ComponentStore, ClassInternal, "x"), ClassInternal, false},
		{&IdempotencyMismatch{Component: ComponentStore, Key: "k"}, ClassConflict, false},
	}
	for _, c := range cases {
		if got, ok := ClassOf(c.err); !ok || got != c.class {
			t.Errorf("ClassOf(%v) = %s,%v want %s", c.err, got, ok, c.class)
		}
		if Retryable(c.err) != c.retry {
			t.Errorf("Retryable(%v) = %v, want %v", c.err, Retryable(c.err), c.retry)
		}
	}
	if _, ok := ClassOf(errors.New("plain")); ok {
		t.Error("plain errors are not contract errors")
	}
	if Retryable(errors.New("plain")) {
		t.Error("non-contract errors must not be retryable (unknown = do not hammer)")
	}
}

func TestClassificationSurvivesWrapping(t *testing.T) {
	inner := Wrap(ComponentLibrary, ClassUnavailable, errors.New("conn refused"), "backend down")
	outer := fmt.Errorf("attempt 2: %w", inner)
	if !Retryable(outer) {
		t.Fatal("class lost through fmt.Errorf wrapping — implementations wrap causes")
	}
	var ce *Error
	if !errors.As(outer, &ce) || ce.Component != ComponentLibrary || ce.Message != "backend down" {
		t.Fatalf("errors.As did not recover the contract error: %+v", ce)
	}
	if errors.Unwrap(outer) != inner {
		t.Fatal("unwrap chain broken")
	}
}

func TestIdempotencyMismatchIsItsOwnType(t *testing.T) {
	err := &IdempotencyMismatch{Component: ComponentLibrary, Key: "import-42"}
	var mm *IdempotencyMismatch
	if !errors.As(error(err), &mm) || mm.Key != "import-42" {
		t.Fatal("IdempotencyMismatch not recoverable via errors.As with its key")
	}
	if class, ok := ClassOf(err); !ok || class != ClassConflict {
		t.Fatalf("mismatch class = %s,%v want conflict", class, ok)
	}
	if Retryable(err) {
		t.Fatal("mismatch must never be retryable")
	}
	if got, want := err.Error(), `library: conflict: idempotency key "import-42" was already used with a different payload`; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestErrorTextIncludesContext(t *testing.T) {
	cause := errors.New("boom")
	e := Wrap(ComponentStore, ClassInternal, cause, "persist failed")
	want := "store: internal: persist failed: boom"
	if e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
}
