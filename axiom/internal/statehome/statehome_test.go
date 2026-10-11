package statehome

import (
	"os"
	"path/filepath"
	"testing"
)

// #368 (v0.2.4): the one-time ~/.axiom-ng migration is gone with the
// alias generation. statehome is pure resolution again — these tests pin
// Dir's contract only.

func TestDirIsCanonicalPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".axiom"); got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
	// pure resolution: no side effects
	if _, err := os.Lstat(filepath.Join(home, ".axiom")); !os.IsNotExist(err) {
		t.Fatal("Dir must not create the state root")
	}
}

func TestDirNoHomeDegrades(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := Dir(); err == nil {
		t.Fatal("no home must degrade to an error, not a fake path")
	}
}

// A leftover ~/.axiom-ng path from a migrated 0.2.x install is inert —
// resolution never reads or touches it.
func TestLegacyPathIsInert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, ".axiom-ng")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".axiom"); got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy path must be left untouched: %v", err)
	}
}
