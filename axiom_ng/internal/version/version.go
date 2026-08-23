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

// Banner is the single identity line used by `axiom-ng --version` and
// /api/health so both always agree (#205 DoD).
func Banner() string {
	return fmt.Sprintf("axiom-ng %s (commit %s, %s build)", Version, Commit, BuildType)
}
