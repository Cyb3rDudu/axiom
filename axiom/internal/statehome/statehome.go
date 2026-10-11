// Package statehome resolves the axiom state root (~/.axiom, #352).
// The one-time migration off the legacy ~/.axiom-ng directory ran
// through 0.2.x and is removed with the alias generation (#368,
// v0.2.4): a leftover ~/.axiom-ng symlink from a migrated install is
// inert and can be removed by the operator; pure resolution, no side
// effects — tests and subcommands never touch a real home directory.
package statehome

import (
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
