// Package version exposes build metadata injected at link time via -ldflags.
package version

var (
	// Version is the semantic version of the binary.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the RFC3339 build timestamp.
	Date = "unknown"
)

// Info bundles the build metadata for JSON responses.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Current returns the build metadata for this binary.
func Current() Info {
	return Info{Version: Version, Commit: Commit, Date: Date}
}
