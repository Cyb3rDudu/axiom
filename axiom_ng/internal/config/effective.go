// effective.go — the resolved-view surface behind `axiom config get
// --effective --json` (F05 #299): one row per environment key the loader
// reads, carrying the EFFECTIVE value and its source (precedence today:
// flag > env > default — flags arrive with F13; today env > default).
//
// Secrets never leave this package in the clear: rows marked secret carry
// a redacted placeholder, and the DSN is projected to its credential-free
// form. A source-parse test (effective_test.go) keeps the table honest —
// every AXIOM_* key read by config.go must have a row, so a new knob
// cannot silently stay invisible.
package config

import (
	"fmt"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry is one resolved configuration row.
type Entry struct {
	// Env is the environment key the row resolves from.
	Env string `json:"env"`
	// Value is the effective value (secrets redacted by renderOutput and
	// the DSN sanitizer).
	Value any `json:"value"`
	// Source is "env" when this key produced the shown value, "default"
	// otherwise — for the single-fed keys that is "is the key set"; for a
	// dual-fed field (FixerCommand, F08 #302) a set-but-shadowed var does
	// NOT feed the value and renders "default". ("flag" joins the
	// precedence with F13.)
	Source string `json:"source"`
}

// envRow couples one loader key with its Config field and secrecy.
type envRow struct {
	env    string
	field  string
	secret bool
}

// envRows is the single mapping from environment keys to Config fields.
// MUST stay complete over config.go's reader calls — TestEffectiveTableCoversAllReadKeys enforces it.
var envRows = []envRow{
	{"AXIOM_ZOTERO_BASE", "ZoteroBaseURL", false},
	{"AXIOM_ZOTERO_LIBRARY", "ZoteroLibraryID", false},
	{"AXIOM_DATABASE_URL", "DatabaseURL", true},
	{"AXIOM_OPENSEARCH_URL", "OpenSearchURL", false},
	{"AXIOM_OPENSEARCH_USERNAME", "OpenSearchUsername", false},
	{"AXIOM_OPENSEARCH_PASSWORD", "OpenSearchPassword", true},
	{"AXIOM_PROCESSOR_SOURCE_SECRET", "ProcessorSourceSecret", true},
	// F10 #304: dispatcher-side compute-worker pointing vars. Canonical
	// AXIOM_COMPUTE_WORKER_* spelling first; the legacy row (mostly
	// AXIOM_PROCESSOR_*) keeps working through the deprecation witness
	// (config.Load records its use). Dual-fed fields render their source
	// from the resolver's truth — see dualFedEnv below. The worker's OWN
	// env contract (read by the Python service) is unchanged per #304.
	{"AXIOM_COMPUTE_WORKER_SOURCE_SECRET", "ProcessorSourceSecret", true},
	{"AXIOM_WS_SECRET", "WSSecret", true},
	{"AXIOM_PROCESSOR_SOURCE_BASE_URL", "ProcessorSourceBaseURL", false},
	{"AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL", "ProcessorSourceBaseURL", false},
	{"AXIOM_PROCESSOR_URL", "ProcessorURL", false},
	{"AXIOM_COMPUTE_WORKER_URL", "ProcessorURL", false},
	{"AXIOM_QUERY_RUNNER_URL", "QueryRunnerURL", false},
	{"AXIOM_INGEST_FALLBACK_URL", "IngestFallbackURL", false},
	{"AXIOM_PROCESSOR_URLS", "ProcessorURLs", false},
	{"AXIOM_COMPUTE_WORKER_URLS", "ProcessorURLs", false},
	{"AXIOM_RUNNER_HEALTH_INTERVAL", "RunnerHealthInterval", false},
	{"AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL", "RunnerHealthInterval", false},
	{"AXIOM_SEARCH_SPARSE_ARM", "SearchSparseArm", false},
	{"AXIOM_SEARCH_GRAPH_ARM", "SearchGraphArm", false},
	{"AXIOM_SEARCH_RERANK", "SearchRerank", false},
	{"AXIOM_SEARCH_FRONTMATTER_FILTER", "SearchFrontmatterFilter", false},
	{"AXIOM_SEARCH_MAX_PER_BOOK", "SearchMaxPerBook", false},
	{"AXIOM_PROCESSOR_TIMEOUT", "ProcessorRequestTimeout", false},
	{"AXIOM_COMPUTE_WORKER_TIMEOUT", "ProcessorRequestTimeout", false},
	{"AXIOM_PROCESSOR_RUNNER_NAME", "ProcessorRunnerName", false},
	{"AXIOM_COMPUTE_WORKER_NAME", "ProcessorRunnerName", false},
	{"AXIOM_DISPATCHER_ENABLED", "DispatcherEnabled", false},
	{"AXIOM_DISPATCHER_WORKER_ID", "DispatcherWorkerID", false},
	{"AXIOM_DISPATCHER_CONCURRENCY", "DispatcherConcurrency", false},
	{"AXIOM_DISPATCHER_PROFILE", "DispatcherProfile", false},
	{"AXIOM_DISPATCHER_LEASE", "DispatcherLeaseDuration", false},
	{"AXIOM_DISPATCHER_PREFLIGHT", "DispatcherPreflightEnabled", false},
	{"AXIOM_FIXER_INVOKER_ENABLED", "FixerInvokerEnabled", false},
	// F08 #302: canonical worker command; the legacy AXIOM_FIXER_CMD
	// keeps working through the deprecation witness (config.Load records
	// its use via deprecate.Use). The third field is the secret/redaction
	// flag — a command PATH is operator-debuggable state, not a credential,
	// so neither row redacts.
	{"AXIOM_REPAIR_WORKER_CMD", "FixerCommand", false},
	{"AXIOM_FIXER_CMD", "FixerCommand", false},
	{"AXIOM_FIXER_CONCURRENCY", "FixerConcurrency", false},
	{"AXIOM_FIXER_INTERVAL", "FixerInterval", false},
	{"AXIOM_FIXER_OCR_TIMEOUT", "FixerOCRTimeout", false},
	{"AXIOM_ARTIFACT_ROOT", "ArtifactRoot", false},
	// F06 #300: Library import surface knobs.
	{"AXIOM_LIBRARY_IMPORT_MAX_BYTES", "LibraryImportMaxBytes", false},
	{"AXIOM_LIBRARY_IMPORT_PROVIDERS", "LibraryImportProviders", false},
	// F12 #306: per-component persistence profile (library engine + own
	// DSN / sqlite path). Not secret — operator-debuggable wiring state.
	{"AXIOM_STORAGE_LIBRARY_DRIVER", "StorageLibraryDriver", false},
	{"AXIOM_LIBRARY_DATABASE_URL", "LibraryDatabaseURL", false},
	{"AXIOM_LIBRARY_SQLITE_PATH", "LibrarySQLitePath", false},
	{"AXIOM_ZOTERO_WRITE_KEY_FILE", "ZoteroWriteKeyFile", false},
	{"AXIOM_QUARANTINE_ROOT", "QuarantineRoot", false},
	{"AXIOM_API_PORT", "APIPort", false},
	{"AXIOM_BIND_ADDR", "BindAddr", false},
	// F11 #305: split-topology binding knobs (runtime deployment config;
	// never part of the public client contract).
	{"AXIOM_LIBRARY_URL", "LibraryURL", false},
	{"AXIOM_STORE_URL", "StoreURL", false},
	{"AXIOM_INTERNAL_LIBRARY_ADDR", "InternalLibraryAddr", false},
	{"AXIOM_INTERNAL_STORE_ADDR", "InternalStoreAddr", false},
	{"AXIOM_COMPONENT_TIMEOUT", "ComponentTimeout", false},
	{"AXIOM_CONTEXTUAL_COLLECTIONS", "ContextualCollectionPaths", false},
	{"AXIOM_CONTEXTUAL_TAGS", "ContextualTags", false},
}

// RedactedValue is the placeholder every secret row carries in output.
const RedactedValue = "<redacted>"

// dualFedEnv maps a dual-fed Config field to its (canonical, legacy) env
// pair — fields resolved by PRECEDENCE in config.go (F08 FixerCommand,
// F10 compute-worker pointing vars). The rows tell the resolver's truth:
// source=env ONLY on the row whose key actually fed the value; a shadowed
// or empty var renders default (its health-counter witness in
// /api/health/deprecations is the "is it set?" surface).
var dualFedEnv = map[string][2]string{
	"FixerCommand":            {"AXIOM_REPAIR_WORKER_CMD", "AXIOM_FIXER_CMD"},
	"ProcessorURL":            {"AXIOM_COMPUTE_WORKER_URL", "AXIOM_PROCESSOR_URL"},
	"ProcessorURLs":           {"AXIOM_COMPUTE_WORKER_URLS", "AXIOM_PROCESSOR_URLS"},
	"ProcessorRunnerName":     {"AXIOM_COMPUTE_WORKER_NAME", "AXIOM_PROCESSOR_RUNNER_NAME"},
	"RunnerHealthInterval":    {"AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL", "AXIOM_RUNNER_HEALTH_INTERVAL"},
	"ProcessorRequestTimeout": {"AXIOM_COMPUTE_WORKER_TIMEOUT", "AXIOM_PROCESSOR_TIMEOUT"},
	"ProcessorSourceSecret":   {"AXIOM_COMPUTE_WORKER_SOURCE_SECRET", "AXIOM_PROCESSOR_SOURCE_SECRET"},
	"ProcessorSourceBaseURL":  {"AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL", "AXIOM_PROCESSOR_SOURCE_BASE_URL"},
}

// Effective renders the resolved view of cfg: one Entry per env row, in
// table order. Secrets are redacted; the DSN is projected without its
// credential part so operators can still see WHERE the process points.
// Source truth here is the env-only view (env > default); the full
// chain (flag > env > file > default) renders through EffectiveChain.
func Effective(cfg Config) []Entry {
	return render(cfg, envOnlySource)
}

// EffectiveChain renders the resolved view with full-chain provenance
// (F13 #307): source flag | env | file | default per key, the Chain
// built by LoadResolved.
func EffectiveChain(cfg Config, ch Chain) []Entry {
	return render(cfg, ch.stage)
}

// RowKeys lists every environment key the loader reads (one per
// envRows row) — callers needing the exact vocabulary (test isolation
// pinning the ambient environment before asserting exact import
// results) use it instead of re-deriving the set.
func RowKeys() []string {
	keys := make([]string, 0, len(envRows))
	for _, row := range envRows {
		keys = append(keys, row.env)
	}
	return keys
}

// render is the shared row renderer; source decides each row's stage
// label.
func render(cfg Config, source func(envRow) string) []Entry {
	v := reflect.ValueOf(cfg)
	out := make([]Entry, 0, len(envRows))
	for _, row := range envRows {
		f := v.FieldByName(row.field)
		if !f.IsValid() {
			panic(fmt.Sprintf("config: effective table field %q missing on Config — table and struct drifted", row.field))
		}
		var value any
		switch {
		case row.env == "AXIOM_DATABASE_URL", row.env == "AXIOM_LIBRARY_DATABASE_URL":
			value = sanitizeDSN(f.String())
		case row.secret:
			value = RedactedValue
		case f.Type() == durationType:
			// durations render as their Go spelling ("30s"), not raw
			// nanoseconds — operator-facing output must stay readable.
			value = f.Interface().(time.Duration).String()
		case f.Kind() == reflect.String:
			// URL-valued rows are non-secret by table decision, but an inline
			// userinfo (scheme://user:pass@host) or a credential query
			// parameter (?password=…, incl. percent-encoded and keyword/value
			// spellings) is a credential the operator typed — values never
			// leave with one attached (renderOutput is the ONE projection;
			// the write gate refuses the same forms on the way IN).
			value = renderOutput(f.String())
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			urls, _ := f.Interface().([]string)
			redacted := make([]string, len(urls))
			for i, u := range urls {
				redacted[i] = renderOutput(u)
			}
			value = redacted
		default:
			value = f.Interface()
		}
		out = append(out, Entry{Env: row.env, Value: value, Source: source(row)})
	}
	return out
}

// envOnlySource is the env-only stage truth (the pre-F13 view): a set
// environment key reads env, everything else default — dual-fed fields
// from the resolver's truth (a shadowed legacy spelling renders
// default).
func envOnlySource(row envRow) string {
	source := SourceDefault
	if _, set := os.LookupEnv(row.env); set {
		source = SourceEnv
	}
	// Dual-fed fields (F08 #302, generalized F10 #304): the generic
	// per-key LookupEnv lied twice on these (with both set, the legacy
	// row rendered the canonical VALUE under source=env; with only
	// legacy set, the canonical row rendered the legacy value under
	// source=default). The pair table above tells the resolver's truth.
	if pair, dual := dualFedEnv[row.field]; dual {
		canonicalSet := os.Getenv(pair[0]) != ""
		legacySet := os.Getenv(pair[1]) != ""
		fed := (row.env == pair[0] && canonicalSet) ||
			(row.env == pair[1] && legacySet && !canonicalSet)
		if fed {
			source = SourceEnv
		} else {
			source = SourceDefault
		}
	}
	return source
}

// credentialQueryRe matches the query parameters pgx honors as
// credentials (pgconn.ParseConfig): a password smuggled as ?password=…
// is a REAL credential, not a dead string — it must never survive
// redaction (output) nor pass the write gate (InlineCredential).
// pgconn DECODES percent-escapes before matching key names (verified:
// ?pass%77ord=x sets cfg.Password), so the pattern matches each key
// letter as literal OR percent-encoded — `pass%77ord=` is a credential
// key exactly like `password=`.
//
//	The VALUE class stops at &, whitespace, AND a double quote: Go's
//	url.Error wraps the URL in quotes (Get "…?password=x" dial …) — a
//	quote-hungry class would eat the closing quote and mangle the
//	diagnosis line's shape (value still gone; the quote is cosmetic
//	but belongs to the engine, not the credential).
var credentialQueryRe = regexp.MustCompile(
	"(?i)(" + encodableKey("password") + "|" + encodableKey("sslpassword") + "|" + encodableKey("passfile") + ")=[^&\\s\"]*")

// encodableKey renders key as a regex fragment matching every character
// as its literal form or its percent-escape (upper- or lowercase hex).
func encodableKey(key string) string {
	var b strings.Builder
	for _, c := range []byte(key) {
		lo := c | 0x20  // 'p'
		hi := c &^ 0x20 // 'P' — %50 and %70 are DIFFERENT escapes
		fmt.Fprintf(&b, "(?:%s|%s|%%%x|%%%x)", string(lo), string(hi), lo, hi)
	}
	return b.String()
}

// RedactQueryCredentials removes credential query-parameter VALUES from
// an arbitrary string (error messages, raw DSNs), matching literal AND
// percent-encoded key spellings. One mechanism backs both the structured
// sanitizer and the free-text error paths (doctor).
func RedactQueryCredentials(s string) string {
	return credentialQueryRe.ReplaceAllString(s, "$1="+RedactedValue)
}

// credentialKeywordRe matches the DSN keyword/value credential form
// with WHITESPACE tolerated around the equals sign — pgconn TRIMS
// " \t\n\r\v\f" from keyword/value keys before matching (pgconn
// config.go, v5.9.2), so `host=h password =KWVAL dbname=d` is a WORKING
// credential the query-parameter pattern above misses (it requires the
// literal `=` directly after the key). The pattern lives on the WRITE
// side only: an unparseable DSN renders as the wholesale redaction
// placeholder, so the render regex stays literal — the gate is
// deliberately at least as wide as anything the loader honors. The
// over-match in a URL query (`?password =x`, which pgx does NOT honor —
// exact-key lookup) is harmless: refused, never mis-opened.
var credentialKeywordRe = regexp.MustCompile(
	"(?i)(?:^|\\s)(?:sslpassword|passfile|password)[ \\t\\n\\r\\v\\f]*=")

// InlineCredential reports whether a NON-secret value still carries a
// credential in a form the loaders honor — the write-surface refusal
// predicate (F13 review: the render side's own credential recognition,
// reused as the gate). Three forms:
//
//   - userinfo WITH a password (scheme://user:pass@…). A bare username
//     (scheme://user@…) is an identity, not a credential — legal.
//   - a query parameter the render side itself redacts: password /
//     sslpassword / passfile, literal AND percent-encoded key spellings
//     (pgconn decodes both — verified in this package's tests). This
//     branch also covers the DSN keyword/value form
//     ("host=h password=kw dbname=d"): the same literal key=value
//     shape, matched to the next whitespace.
//   - the whitespace-spelled keyword/value form ("password =kw"):
//     pgconn trims whitespace from the key, so it is a working
//     credential — see credentialKeywordRe.
//
// The return value names the FORM for the refusal message — never any
// part of the value. Empty return = no credential found.
//
// Residual scope, named: the vocabulary is the credential keys the
// LOADERS honor (password/sslpassword/passfile). Other query
// parameters (a bearer ?token=…, an ?api_key=…) are ordinary values
// to this gate — writable and rendered — because no loader feeds them
// into a credential; if one ever does, its key joins this vocabulary
// the same day.
//
// Deliberate over-match: a free-text value containing a
// credential-shaped fragment (say a filter listing `notes,password=x,todo`)
// is refused too. The rule is render-symmetric — whatever the effective
// view would REDACT is unwritable — and per-key URL-shape detection
// would buy back exactly the smuggle-via-another-key problem the
// symmetry exists to close. A redacted render is never a legal stored
// form; such a value keeps riding the environment.
func InlineCredential(value string) string {
	if u, err := url.Parse(value); err == nil && u.Scheme != "" && u.User != nil {
		if _, hasPW := u.User.Password(); hasPW {
			return "inline userinfo credential (scheme://user:pass@…)"
		}
	}
	if RedactQueryCredentials(value) != value || credentialKeywordRe.MatchString(value) {
		return "query-parameter credential (password/sslpassword/passfile, incl. percent-encoded and keyword/value spellings)"
	}
	return ""
}

// sanitizeDSN strips the credential from a Postgres DSN, keeping scheme,
// host, port, database and params — the whole userinfo is dropped AND
// credential query values (password, sslpassword, passfile — pgx honors
// all three) are redacted. Unparseable values fall back to the redaction
// placeholder (never echo an unknown credential shape).
func sanitizeDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return RedactedValue
	}
	u.User = nil
	u.RawQuery = RedactQueryCredentials(u.RawQuery)
	return u.String()
}

// stripURLUserinfo removes an inline userinfo (scheme://user:pass@…) from
// a URL-valued config row. Non-URL strings parse without scheme or fail
// outright and pass through unchanged.
func stripURLUserinfo(s string) string {
	if s == "" {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.User == nil {
		return s
	}
	u.User = nil
	return u.String()
}

// renderOutput is the output projection for URL-shaped values: userinfo
// stripped, then query credentials redacted — every rendered row and
// every slice element goes through it (the DSN rows keep their stricter
// sanitizeDSN; this covers everything else a URL can carry).
func renderOutput(s string) string {
	return RedactQueryCredentials(stripURLUserinfo(s))
}

// ValidateEnv re-parses the RAW environment against the table's field
// kinds and reports every key whose value the loader would silently
// fall back on (a typo'd duration, a non-numeric port). This is the
// consistency check behind `axiom config validate`; the empty slice
// means the env combination parses cleanly (role-level consistency is
// the caller's composition check). Dual-fed legacy spellings whose
// canonical sibling is set are SKIPPED: the resolver never reads them
// (their value is dead config, not a silent fallback).
func ValidateEnv() []string {
	var problems []string
	for _, row := range envRows {
		raw, set := os.LookupEnv(row.env)
		if !set || raw == "" {
			continue
		}
		if pair, dual := dualFedEnv[row.field]; dual && row.env == pair[1] && os.Getenv(pair[0]) != "" {
			continue // shadowed legacy spelling — never read by the loader
		}
		problems = append(problems, checkRawValue(row, raw)...)
	}
	sort.Strings(problems)
	return problems
}

// checkRawValue validates one non-empty raw value against its table
// row's target kind — the shared rules behind ValidateEnv, the
// config.sqlite validation (ValidateSettings), and the --set flag
// validation (ValidateFlags). A problem names the key, the value, and
// what the loader would do (fall back silently / abort at start).
func checkRawValue(row envRow, raw string) []string {
	cfg := Config{}
	var problems []string
	kind := reflect.ValueOf(cfg).FieldByName(row.field).Kind()
	var err error
	switch kind {
	case reflect.Int, reflect.Int64:
		if reflect.ValueOf(cfg).FieldByName(row.field).Type() == durationType {
			_, err = time.ParseDuration(raw)
		} else {
			_, err = strconv.Atoi(raw)
		}
	case reflect.Bool:
		if !boolRecognized(raw) {
			err = fmt.Errorf("not a boolean (the loader reads only 1/true/yes and 0/false/no)")
		}
	case reflect.String, reflect.Slice:
		// free-form; nothing to re-parse
	}
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s=%q: %v (loader falls back to the default silently)", row.env, raw, err))
	}
	// Vocabulary keys: values the loader would silently fall back on
	// are not parse errors but WORD errors — same reporting channel.
	if row.env == "AXIOM_STORAGE_LIBRARY_DRIVER" && raw != "" && raw != "postgres" && raw != "sqlite" {
		problems = append(problems, fmt.Sprintf("%s=%q: unknown driver (known: postgres, sqlite) — the composition aborts at start", row.env, raw))
	}
	// Value RANGE: a syntactically valid port outside 1..65535 parses
	// fine and dies at bind time — flagged here so every surface (env,
	// file, --set) refuses it before the listener does.
	if row.env == "AXIOM_API_PORT" {
		if p, perr := strconv.Atoi(raw); perr == nil && (p < 1 || p > 65535) {
			problems = append(problems, fmt.Sprintf("%s=%q: out of range (1-65535) — the listener cannot bind it", row.env, raw))
		}
	}
	return problems
}

// boolRecognized mirrors envBoolDefault's grammar EXACTLY: "1", "true"
// and "yes" (any case) are true; "0", "false" and "no" are false.
// Anything else ("t", "y", …) is an unrecognized spelling — the loader
// reads those as FALSE, so a default-TRUE flag silently drops its
// default instead of keeping it — the exact silent-fallback class
// ValidateEnv exists to flag, so it is NOT recognized here either.
func boolRecognized(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "0", "false", "no":
		return true
	}
	return false
}

// durationType identifies time.Duration-typed Config fields (rendered as
// their Go spelling in Effective instead of raw nanoseconds).
var durationType = reflect.TypeOf(time.Duration(0))
