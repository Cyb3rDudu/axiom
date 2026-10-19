// pathmap.go — sync path translation for containerized deployments (#379).
//
// On hosts where axiom runs in a container, the Zotero attachment blobs
// live outside the container filesystem while the Zotero local API
// reports file:// URIs with host-absolute paths. AXIOM_ZOTERO_PATH_MAP
// (format HOST_PREFIX:CONTAINER_PREFIX, comma-separated for more than
// one pair) rewrites those paths so the container can open them.
//
// The pairs are parsed once at composition startup and CARRIED by the
// consumers (sync Service, mirror Repo, provider Options) — the same
// config-path style as every other option; no package-level state. Each
// consumer applies TranslatePath to the LocalFilePath result at its own
// derivation site. Durability is the mirror upsert itself
// (local_path=EXCLUDED.local_path): after a deployment change, one
// re-sync rewrites the stored paths; no migration SQL is involved.
package zoteroprovider

import (
	"fmt"
	"strings"
)

// PathPair is one HOST_PREFIX:CONTAINER_PREFIX rewrite rule.
type PathPair struct{ From, To string }

// ParsePathMap parses the AXIOM_ZOTERO_PATH_MAP value. Empty/whitespace
// input yields no pairs (unset = identity). Each pair is
// HOST_PREFIX:CONTAINER_PREFIX with non-empty sides; a missing colon or
// an empty side is malformed — the caller fails fast at startup, never
// a silent no-op. Trailing slashes on either side normalize away, and
// pairs keep their order (first match wins).
func ParsePathMap(v string) ([]PathPair, error) {
	var out []PathPair
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue // formatting slack, same as the URL-list precedent
		}
		from, to, ok := strings.Cut(part, ":")
		from, to = strings.TrimSuffix(strings.TrimSpace(from), "/"), strings.TrimSuffix(strings.TrimSpace(to), "/")
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("malformed pair %q — expected HOST_PREFIX:CONTAINER_PREFIX with non-empty sides", part)
		}
		out = append(out, PathPair{From: from, To: to})
	}
	return out, nil
}

// TranslatePath applies the pairs at path-segment boundaries: From
// matches only as a whole path prefix (/prefixX is NOT /prefix). The
// first matching pair wins; no match (or nil pairs) returns the path
// unchanged. Pure function — callers own the pairs they carry.
func TranslatePath(pairs []PathPair, p string) string {
	for _, pp := range pairs {
		if p == pp.From || (strings.HasPrefix(p, pp.From) && strings.HasPrefix(p[len(pp.From):], "/")) {
			return pp.To + p[len(pp.From):]
		}
	}
	return p
}
