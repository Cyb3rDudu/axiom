// pathmap.go — sync path translation for containerized deployments (#379).
//
// On hosts where axiom runs in a container, the Zotero attachment blobs
// live outside the container filesystem while the Zotero local API
// reports file:// URIs with host-absolute paths. AXIOM_ZOTERO_PATH_MAP
// (format HOST_PREFIX:CONTAINER_PREFIX, comma-separated for more than
// one pair) rewrites those paths so the container can open them.
//
// The translation lives HERE, at the single derivation point (the
// LocalFilePath result), so mirror projection, store intake and the
// repair invoker all see the same translated path — there is no second
// translation layer anywhere else. Durability is the mirror upsert
// itself (local_path=EXCLUDED.local_path): after a deployment change,
// one re-sync rewrites the stored paths; no migration SQL is involved.
package zoteroprovider

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// PathPair is one HOST_PREFIX:CONTAINER_PREFIX rewrite rule.
type PathPair struct{ From, To string }

// pathPairs is installed once at composition startup (before any sync
// runs); nil/empty means identity — byte-identical behavior, no map.
var pathPairs atomic.Pointer[[]PathPair]

// SetPathPairs installs the path translation. Call once at startup with
// the parsed AXIOM_ZOTERO_PATH_MAP value; nil or empty restores
// identity. Malformed input never reaches here — ParsePathMap (and the
// composition guard calling it) fails fast instead.
func SetPathPairs(pairs []PathPair) {
	if len(pairs) == 0 {
		pathPairs.Store(nil)
		return
	}
	pathPairs.Store(&pairs)
}

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

// translatePath applies the installed pairs at path-segment boundaries:
// From matches only as a whole path prefix (/prefixX is NOT /prefix).
// The first matching pair wins; no match returns the path unchanged.
func translatePath(p string) string {
	pairs := pathPairs.Load()
	if pairs == nil {
		return p
	}
	for _, pp := range *pairs {
		if p == pp.From || (strings.HasPrefix(p, pp.From) && strings.HasPrefix(p[len(pp.From):], "/")) {
			return pp.To + p[len(pp.From):]
		}
	}
	return p
}
