// Package version carries the build stamp injected via -ldflags at build
// time (see Makefile target `rag`). `go build` without ldflags yields the
// dev default below — a visibly marked debug build (#205 §5).
package version

import "fmt"

var (
	// Version is set by the release build (git describe output).
	Version = "dev"
	// Commit is set by the release build (git rev-parse --short HEAD).
	Commit = "none"
	// BuildType is "release" for stamped artifacts, "debug" otherwise.
	BuildType = "debug"
)

// BuildTypeRelease is the BuildType stamped by `make rag` (ldflags).
const BuildTypeRelease = "release"

// DebugBindRefused reports whether a non-release build must refuse to bind
// a production port (#205 §5). Production ports: 8011 (API) and 8013–8015
// (dispatcher instances). Opt out for local dev with
// AXIOM_ALLOW_DEBUG_BIND=1.
func DebugBindRefused(buildType string, port int, allowEnv func(string) string) bool {
	return buildType != BuildTypeRelease && port >= 8011 && port <= 8015 &&
		allowEnv("AXIOM_ALLOW_DEBUG_BIND") != "1"
}

// Banner is the single identity line used by `axiom-ng --version` and
// /api/health so both always agree (#205 DoD).
func Banner() string {
	return fmt.Sprintf("axiom-ng %s (commit %s, %s build)", Version, Commit, BuildType)
}
