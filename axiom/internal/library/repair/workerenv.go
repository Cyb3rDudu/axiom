// workerenv.go — DM07 #316: the repair worker child runs on an
// EXPLICITLY CONSTRUCTED minimal environment, never on an inherited
// one. The worker is credential-free by contract: it carries no
// database access (neither DSN, no WS/OpenSearch/source secrets) — its
// inputs are the request arguments, its outputs the artifact + report.
// The allowlist below is the whole worker-side env vocabulary; the
// denylist witness in workerenv_test.go pins the known credential
// names as ABSENT (checked by name, values never printed).
package repair

import (
	"os"
	"sort"
	"strings"
)

// workerEnvAllow is the exact allowlist: tool-level basics (PATH, HOME,
// TMPDIR, locale/timezone — tesseract, pandoc and the python stack need
// them), outbound-HTTP configuration (proxy + CA bundle names — the
// repair track's DeepSeek/RAG calls legitimately carry them), plus the
// repair track's OWN knobs (wrapper overrides, the RAG edge URL, the
// fixsvc's DeepSeek credentials — the worker's own, not the parent's
// database credentials).
var workerEnvAllow = map[string]bool{
	"PATH":                true,
	"HOME":                true,
	"TMPDIR":              true,
	"TZ":                  true,
	"LANG":                true,
	"HTTP_PROXY":          true,
	"HTTPS_PROXY":         true,
	"NO_PROXY":            true,
	"http_proxy":          true,
	"https_proxy":         true,
	"no_proxy":            true,
	"SSL_CERT_FILE":       true,
	"SSL_CERT_DIR":        true,
	"REQUESTS_CA_BUNDLE":  true,
	"AXIOM_FIXER":         true,
	"AXIOM_FIXER_APP":     true,
	"AXIOM_RUNNER_PYTHON": true,
	"AXIOM_RAG_URL":       true,
	"DEEPSEEK_API_KEY":    true,
	"DEEPSEEK_BASE_URL":   true,
	"DEEPSEEK_MODEL":      true,
}

// workerEnvAllowPrefix covers name-scoped families (LC_* locale vars,
// AXIOM_FIXSVC_* fixsvc switches).
var workerEnvAllowPrefix = []string{"LC_", "AXIOM_FIXSVC_"}

// workerEnvDeniedNames is the denylist WITNESS vocabulary — the known
// credential variable names that must never reach the worker. Used by
// the test sonde (names only; a value would already be a bug).
// Deliberately NOT identical to the compute worker's Python
// CREDENTIAL_ENV_NAMES (axiom-compute-worker config.py): this sonde also
// denies identity variables (AXIOM_OPENSEARCH_USERNAME — the worker
// never needs the OS identity either); the Python boot guard lists
// credential variables only. Review the sibling list when extending
// either side.
var workerEnvDeniedNames = []string{
	"AXIOM_DATABASE_URL",
	"AXIOM_STORE_DATABASE_URL",
	"AXIOM_LIBRARY_DATABASE_URL",
	"AXIOM_WS_SECRET",
	"AXIOM_OPENSEARCH_PASSWORD",
	"AXIOM_OPENSEARCH_USERNAME",
	"AXIOM_PROCESSOR_SOURCE_SECRET",
	"AXIOM_COMPUTE_WORKER_SOURCE_SECRET",
}

// workerEnv constructs the worker child environment from the parent
// process: only allowlisted names copy over (sorted for determinism —
// the child env is asserted, not eyeballed), plus the caller's extras
// (e.g. AXIOM_FIX_SH_TIMEOUT). Everything else — including every
// credential the parent holds — stays behind.
func workerEnv(extras ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if workerEnvAllowed(name) {
			out = append(out, kv)
		}
	}
	sort.Strings(out)
	return append(out, extras...)
}

func workerEnvAllowed(name string) bool {
	if workerEnvAllow[name] {
		return true
	}
	for _, p := range workerEnvAllowPrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
