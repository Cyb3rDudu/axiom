package statehome

import (
	"os"
	"path/filepath"
	"testing"
)

// The migration probe (#352 acceptance): the two install orders every
// deployment hits, with the actual bytes on the line — the file content
// must survive the move identically (a migration that loses or alters
// content goes red here, not in production).
func TestMigrateMovesLegacyIntact(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, ".axiom-ng")
	canonical := filepath.Join(home, ".axiom")

	// legacy install: quarantine originals + the sqlite file + the key
	for _, p := range []string{"quarantine/orig-1.pdf", "library.sqlite", "write-api-key"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(legacy, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		// distinct payload per file — a swap or truncation cannot pass
		if err := os.WriteFile(filepath.Join(legacy, p), []byte("payload:"+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(legacy, canonical); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// contents landed intact, byte for byte
	for _, p := range []string{"quarantine/orig-1.pdf", "library.sqlite", "write-api-key"} {
		got, err := os.ReadFile(filepath.Join(canonical, p))
		if err != nil {
			t.Fatalf("migrated file %s missing under canonical root: %v", p, err)
		}
		if string(got) != "payload:"+p {
			t.Fatalf("migrated file %s altered: %q", p, got)
		}
	}

	// compat symlink: legacy path resolves to the canonical root
	fi, err := os.Lstat(legacy)
	if err != nil {
		t.Fatalf("legacy path must remain as compat symlink: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("legacy path must be a symlink, got mode %v", fi.Mode())
	}
	canonResolved, _ := filepath.EvalSymlinks(canonical)
	legacyResolved, lerr := filepath.EvalSymlinks(legacy)
	if lerr != nil || legacyResolved != canonResolved {
		t.Fatalf("legacy symlink must resolve to %s, got %s (%v)", canonResolved, legacyResolved, lerr)
	}
	// through the symlink the same bytes are reachable (rollback safety)
	if got, err := os.ReadFile(filepath.Join(legacy, "write-api-key")); err != nil || string(got) != "payload:write-api-key" {
		t.Fatalf("legacy path must keep resolving post-migration, got %q (%v)", got, err)
	}
}

// Fresh install: neither root exists → migration is a no-op and must
// NOT create the canonical directory (nothing owns disk on a dry check).
func TestMigrateFreshInstallIsNoOp(t *testing.T) {
	home := t.TempDir()
	if err := migrate(filepath.Join(home, ".axiom-ng"), filepath.Join(home, ".axiom")); err != nil {
		t.Fatalf("fresh migration failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".axiom")); !os.IsNotExist(err) {
		t.Fatalf("fresh install must not create the canonical root, stat err: %v", err)
	}
}

// Migrated install: canonical present → no-op (idempotent; a second
// start after a successful migration touches nothing).
func TestMigrateIdempotentAfterMigration(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".axiom")
	if err := os.MkdirAll(filepath.Join(canonical, "quarantine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "library.sqlite"), []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}
	// both-present must not merge, move or delete anything
	stray := filepath.Join(home, ".axiom-ng")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := migrate(stray, canonical); err != nil {
		t.Fatalf("both-present migration failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(canonical, "library.sqlite"))
	if err != nil || string(got) != "stable" {
		t.Fatalf("canonical state must be untouched when both roots exist, got %q (%v)", got, err)
	}
	if fi, err := os.Lstat(stray); err != nil || !fi.IsDir() {
		t.Fatalf("stray legacy dir must stay untouched when both roots exist (%v)", err)
	}
}

// Legacy path exists as a SYMLINK (already migrated by an earlier run,
// or operator-pre-created): not a real directory → not ours to touch.
// The migration must no-op — neither creating the canonical root nor
// rewiring the legacy link (statehome.go: !fi.IsDir() branch).
func TestMigrateLegacySymlinkIsNoOp(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "state-elsewhere")
	legacy := filepath.Join(home, ".axiom-ng")
	canonical := filepath.Join(home, ".axiom")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, legacy); err != nil {
		t.Fatal(err)
	}

	if err := migrate(legacy, canonical); err != nil {
		t.Fatalf("symlink-legacy migration failed: %v", err)
	}
	if _, err := os.Lstat(canonical); !os.IsNotExist(err) {
		t.Fatalf("canonical root must not be created when legacy is a symlink, stat err: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(legacy); err != nil {
		t.Fatalf("legacy symlink must stay resolvable: %v", err)
	} else if want, _ := filepath.EvalSymlinks(target); resolved != want {
		t.Fatalf("legacy symlink must still resolve to its original target %s, got %s", want, resolved)
	}
}
