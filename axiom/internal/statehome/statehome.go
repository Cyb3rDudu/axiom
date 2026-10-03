// Package statehome resolves the axiom state root (~/.axiom, #352) and
// carries the one-time migration off the legacy ~/.axiom-ng directory:
// when the canonical root is absent and the legacy root exists as a real
// directory, its contents move once and ~/.axiom-ng remains as a symlink
// to ~/.axiom — rollback-safe (the legacy path keeps resolving) and
// free of dual state (one directory of record).
//
// Migrate runs at process start (cli.Run), NOT lazily inside Dir:
// path resolution must stay side-effect-free so tests and subcommands
// never touch a real home directory. Deployments perform the
// authoritative move in their own start sequence; a failed migration
// logs and leaves the legacy root untouched so a retry or the operator
// can finish it. Failures never block startup.
package statehome

import (
	"log"
	"os"
	"path/filepath"
)

// Dir returns the canonical state root (~/.axiom). No migration, no
// creation — pure resolution; an unresolvable home is the caller's
// error to handle (defaults degrade to their documented fallbacks).
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", os.ErrInvalid
	}
	return filepath.Join(home, ".axiom"), nil
}

// Migrate performs the one-time legacy → canonical move. Idempotent:
// canonical present → no-op (fresh install or already migrated);
// legacy absent → no-op. Both-present is a deliberate no-op (never
// merge). Errors are returned, never fatal to the caller.
func Migrate() error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil // no home → nothing we own; defaults degrade elsewhere
	}
	return migrate(filepath.Join(home, ".axiom-ng"), filepath.Join(home, ".axiom"))
}

// migrate moves legacy onto canonical and leaves the legacy path as a
// symlink to it.
func migrate(legacy, canonical string) error {
	if _, err := os.Lstat(canonical); err == nil {
		return nil // canonical already there (fresh install or migrated)
	} else if !os.IsNotExist(err) {
		return err
	}
	fi, err := os.Lstat(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // fresh install: nothing to move
		}
		return err
	}
	if !fi.IsDir() {
		return nil // a file or symlink (already migrated) — not ours to touch
	}
	if err := os.Rename(legacy, canonical); err != nil {
		return err
	}
	if err := os.Symlink(canonical, legacy); err != nil {
		// Contents are safe under canonical; only the compat path is
		// missing — say exactly what broke, the move itself succeeded.
		log.Printf("statehome: legacy compat symlink %s could not be created: %v (contents live under %s)", legacy, err, canonical)
	}
	return nil
}
