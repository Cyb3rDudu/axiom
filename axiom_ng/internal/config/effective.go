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
	"strconv"
	"strings"
	"time"
)

// Entry is one resolved configuration row.
type Entry struct {
	// Env is the environment key the row resolves from.
	Env string `json:"env"`
	// Value is the effective value (secrets redacted by RenderValue).
	Value any `json:"value"`
	// Source is "env" when the key is set in the environment, "default"
	// otherwise. ("flag" joins the precedence with F13.)
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
	{"AXIOM_WS_SECRET", "WSSecret", true},
	{"AXIOM_PROCESSOR_SOURCE_BASE_URL", "ProcessorSourceBaseURL", false},
	{"AXIOM_PROCESSOR_URL", "ProcessorURL", false},
	{"AXIOM_QUERY_RUNNER_URL", "QueryRunnerURL", false},
	{"AXIOM_INGEST_FALLBACK_URL", "IngestFallbackURL", false},
	{"AXIOM_PROCESSOR_URLS", "ProcessorURLs", false},
	{"AXIOM_RUNNER_HEALTH_INTERVAL", "RunnerHealthInterval", false},
	{"AXIOM_SEARCH_SPARSE_ARM", "SearchSparseArm", false},
	{"AXIOM_SEARCH_GRAPH_ARM", "SearchGraphArm", false},
	{"AXIOM_SEARCH_RERANK", "SearchRerank", false},
	{"AXIOM_SEARCH_FRONTMATTER_FILTER", "SearchFrontmatterFilter", false},
	{"AXIOM_SEARCH_MAX_PER_BOOK", "SearchMaxPerBook", false},
	{"AXIOM_PROCESSOR_TIMEOUT", "ProcessorRequestTimeout", false},
	{"AXIOM_PROCESSOR_RUNNER_NAME", "ProcessorRunnerName", false},
	{"AXIOM_DISPATCHER_ENABLED", "DispatcherEnabled", false},
	{"AXIOM_DISPATCHER_WORKER_ID", "DispatcherWorkerID", false},
	{"AXIOM_DISPATCHER_CONCURRENCY", "DispatcherConcurrency", false},
	{"AXIOM_DISPATCHER_PROFILE", "DispatcherProfile", false},
	{"AXIOM_DISPATCHER_LEASE", "DispatcherLeaseDuration", false},
	{"AXIOM_DISPATCHER_PREFLIGHT", "DispatcherPreflightEnabled", false},
	{"AXIOM_FIXER_INVOKER_ENABLED", "FixerInvokerEnabled", false},
	// F08 #302: canonical worker command; the legacy AXIOM_FIXER_CMD keeps
	// working through the deprecation witness (config.Load records its use).
	{"AXIOM_REPAIR_WORKER_CMD", "FixerCommand", false},
	{"AXIOM_FIXER_CMD", "FixerCommand", true},
	{"AXIOM_FIXER_CONCURRENCY", "FixerConcurrency", false},
	{"AXIOM_FIXER_INTERVAL", "FixerInterval", false},
	{"AXIOM_FIXER_OCR_TIMEOUT", "FixerOCRTimeout", false},
	{"AXIOM_ARTIFACT_ROOT", "ArtifactRoot", false},
	// F06 #300: Library import surface knobs.
	{"AXIOM_LIBRARY_IMPORT_MAX_BYTES", "LibraryImportMaxBytes", false},
	{"AXIOM_LIBRARY_IMPORT_PROVIDERS", "LibraryImportProviders", false},
	{"AXIOM_ZOTERO_WRITE_KEY_FILE", "ZoteroWriteKeyFile", false},
	{"AXIOM_QUARANTINE_ROOT", "QuarantineRoot", false},
	{"AXIOM_API_PORT", "APIPort", false},
	{"AXIOM_BIND_ADDR", "BindAddr", false},
	{"AXIOM_CONTEXTUAL_COLLECTIONS", "ContextualCollectionPaths", false},
	{"AXIOM_CONTEXTUAL_TAGS", "ContextualTags", false},
}

// RedactedValue is the placeholder every secret row carries in output.
const RedactedValue = "<redacted>"

// Effective renders the resolved view of cfg: one Entry per env row, in
// table order. Secrets are redacted; the DSN is projected without its
// credential part so operators can still see WHERE the process points.
func Effective(cfg Config) []Entry {
	v := reflect.ValueOf(cfg)
	out := make([]Entry, 0, len(envRows))
	for _, row := range envRows {
		f := v.FieldByName(row.field)
		if !f.IsValid() {
			panic(fmt.Sprintf("config: effective table field %q missing on Config — table and struct drifted", row.field))
		}
		source := "default"
		if _, set := os.LookupEnv(row.env); set {
			source = "env"
		}
		var value any
		switch {
		case row.env == "AXIOM_DATABASE_URL":
			value = sanitizeDSN(f.String())
		case row.secret:
			value = RedactedValue
		case f.Type() == durationType:
			// durations render as their Go spelling ("30s"), not raw
			// nanoseconds — operator-facing output must stay readable.
			value = f.Interface().(time.Duration).String()
		case f.Kind() == reflect.String:
			// URL-valued rows are non-secret by table decision, but an inline
			// userinfo (scheme://user:pass@host) is a credential the operator
			// typed — values never leave with one attached.
			value = stripURLUserinfo(f.String())
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			urls, _ := f.Interface().([]string)
			redacted := make([]string, len(urls))
			for i, u := range urls {
				redacted[i] = stripURLUserinfo(u)
			}
			value = redacted
		default:
			value = f.Interface()
		}
		out = append(out, Entry{Env: row.env, Value: value, Source: source})
	}
	return out
}

// credentialQueryKeys are the query parameters pgx honors as credentials
// (pgconn.ParseConfig): a password smuggled as ?password=… is a REAL
// credential, not a dead string — it must never survive redaction.
// pgconn DECODES percent-escapes before matching key names (verified:
// ?pass%77ord=x sets cfg.Password), so the pattern matches each key
// letter as literal OR percent-encoded — `pass%77ord=` is a credential
// key exactly like `password=`.
var credentialQueryRe = regexp.MustCompile(
	"(?i)(" + encodableKey("password") + "|" + encodableKey("sslpassword") + "|" + encodableKey("passfile") + ")=[^&\\s]*")

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

// ValidateEnv re-parses the RAW environment against the table's field
// kinds and reports every key whose value the loader would silently
// fall back on (a typo'd duration, a non-numeric port). This is the
// consistency check behind `axiom config validate`; the empty slice
// means the env combination parses cleanly (role-level consistency is
// the caller's composition check).
func ValidateEnv() []string {
	cfg := Config{}
	var problems []string
	for _, row := range envRows {
		raw, set := os.LookupEnv(row.env)
		if !set || raw == "" {
			continue
		}
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
