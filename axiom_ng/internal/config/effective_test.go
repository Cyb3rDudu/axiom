// effective_test.go — witnesses for the resolved-view surface (F05 #299).
// The completeness sonde parses config.go's SOURCE for every AXIOM_* key
// the readers reference and asserts the table has a row — a new knob
// cannot silently stay invisible to `axiom config get --effective`.
package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/deprecate"
)

// TestEffectiveTableCoversAllReadKeys — drift guard between the loader
// and the table (the usage-lint pattern: parse the source, compare sets).
func TestEffectiveTableCoversAllReadKeys(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	keyRe := regexp.MustCompile(`"(AXIOM_[A-Z0-9_]+)"`)
	seen := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(string(src), -1) {
		seen[m[1]] = true
	}
	have := map[string]bool{}
	for _, row := range envRows {
		if have[row.env] {
			t.Fatalf("duplicate table row for %s", row.env)
		}
		have[row.env] = true
	}
	for key := range seen {
		if !have[key] {
			t.Errorf("config.go reads %s but the effective table has no row — add it", key)
		}
	}
	for key := range have {
		if !seen[key] {
			t.Errorf("effective table row %s is read by nothing in config.go — drop it", key)
		}
	}
}

// TestEffectiveMarksSourcesAndRedacts — source annotation follows
// LookupEnv (set-but-empty counts as env), secrets never carry values,
// the DSN is projected credential-free.
func TestEffectiveMarksSourcesAndRedacts(t *testing.T) {
	t.Setenv("AXIOM_API_PORT", "8123")
	t.Setenv("AXIOM_OPENSEARCH_URL", "") // set-empty = the disabled state = source env
	t.Setenv("AXIOM_DATABASE_URL", "postgresql://u:topsecret@db.example:5432/axiom?sslmode=disable")
	t.Setenv("AXIOM_WS_SECRET", "wssecret")

	entries := Effective(Load())
	byEnv := map[string]Entry{}
	for _, e := range entries {
		byEnv[e.Env] = e
	}
	if e := byEnv["AXIOM_API_PORT"]; e.Source != "env" || e.Value != 8123 {
		t.Fatalf("AXIOM_API_PORT: %+v", e)
	}
	if e := byEnv["AXIOM_BIND_ADDR"]; e.Source != "default" {
		t.Fatalf("unset key must read default: %+v", e)
	}
	if e := byEnv["AXIOM_OPENSEARCH_URL"]; e.Source != "env" || e.Value != "" {
		t.Fatalf("set-empty OS URL: %+v", e)
	}
	if e := byEnv["AXIOM_WS_SECRET"]; e.Value != RedactedValue {
		t.Fatalf("ws secret leaked: %+v", e)
	}
	e := byEnv["AXIOM_DATABASE_URL"]
	if e.Value != "postgresql://db.example:5432/axiom?sslmode=disable" {
		t.Fatalf("DSN must be credential-free, got %v", e.Value)
	}
	// Non-vacuous sonde: the sanitized projection keeps host+db.
	if !strings.Contains(e.Value.(string), "db.example") {
		t.Fatal("sanitizer removed the host — redaction over-reached")
	}
}

// TestEffectiveCarriesFixerInterval — the F04 deferral (fixer-interval
// test seam) is resolvable through the config surface.
func TestEffectiveCarriesFixerInterval(t *testing.T) {
	t.Setenv("AXIOM_FIXER_INTERVAL", "45s")
	cfg := Load()
	if cfg.FixerInterval != 45*time.Second {
		t.Fatalf("FixerInterval = %v, want 45s", cfg.FixerInterval)
	}
	found := false
	for _, e := range Effective(cfg) {
		if e.Env == "AXIOM_FIXER_INTERVAL" {
			found = true
			if e.Source != "env" {
				t.Fatalf("source %s, want env", e.Source)
			}
			// Durations render as their Go spelling, not raw nanoseconds.
			if e.Value != "45s" {
				t.Fatalf("AXIOM_FIXER_INTERVAL value = %v (%T), want the string \"45s\"", e.Value, e.Value)
			}
		}
	}
	if !found {
		t.Fatal("AXIOM_FIXER_INTERVAL missing from effective view")
	}
	// Default seam: unset → the invoker default 30s, source default.
	t.Setenv("AXIOM_FIXER_INTERVAL", "")
	if cfg := Load(); cfg.FixerInterval != 30*time.Second {
		t.Fatalf("default FixerInterval = %v, want 30s", cfg.FixerInterval)
	}
}

// TestValidateEnvFlagsSilentFallbacks — the raw-env re-parser catches
// what the loader would silently ignore (typo'd duration, non-numeric
// port, non-boolean flag).
func TestValidateEnvFlagsSilentFallbacks(t *testing.T) {
	if problems := ValidateEnv(); len(problems) != 0 {
		t.Fatalf("clean env must validate, got %v", problems)
	}
	for env, val := range map[string]string{
		"AXIOM_API_PORT":          "eighty",
		"AXIOM_DISPATCHER_LEASE":  "5 minutes",
		"AXIOM_SEARCH_RERANK":     "maybe",
		"AXIOM_FIXER_CONCURRENCY": "1.5",
		// strconv-only spellings ("t", "y") are NOT loader grammar —
		// validate must flag them instead of blessing a silent fallback
		// to the default.
		"AXIOM_SEARCH_SPARSE_ARM":         "t",
		"AXIOM_SEARCH_FRONTMATTER_FILTER": "y",
	} {
		t.Setenv(env, val)
		problems := ValidateEnv()
		if len(problems) == 0 || !strings.Contains(problems[0], env) {
			t.Fatalf("%s=%s must be flagged, got %v", env, val, problems)
		}
		os.Unsetenv(env)
	}
	// Only the loader's own boolean spellings are recognized: 1/true/yes
	// (true) and 0/false/no (false), case-insensitive.
	for _, val := range []string{"no", "false", "0", "TRUE", "Yes"} {
		t.Setenv("AXIOM_SEARCH_RERANK", val)
		if problems := ValidateEnv(); len(problems) != 0 {
			t.Fatalf("%q must be accepted as a recognized boolean, got %v", val, problems)
		}
	}
}

// Credential-query sonde (review round 3): pgx honors ?password= (and
// sslpassword/passfile) as REAL credentials — they must never survive any
// redaction surface, structured (sanitizeDSN) or free-text
// (RedactQueryCredentials, the doctor error paths).
func TestQueryCredentialsRedacted(t *testing.T) {
	dsn := "postgres://u@127.0.0.1:1/db?password=SUPERSECRET_XYZ_42&sslmode=disable"
	got := sanitizeDSN(dsn)
	if strings.Contains(got, "SUPERSECRET_XYZ_42") {
		t.Fatalf("query password survived sanitizeDSN: %s", got)
	}
	if !strings.Contains(got, "sslmode=disable") || !strings.Contains(got, "127.0.0.1:1/db") {
		t.Fatalf("sanitizer removed non-credential parts: %s", got)
	}
	if g := RedactQueryCredentials("failed to parse `postgres://u:p@h/db?password=QUERY&x=1`"); strings.Contains(g, "QUERY") {
		t.Fatalf("free-text redaction leaked: %s", g)
	}
	// sslpassword and passfile are credentials too.
	if g := RedactQueryCredentials("?sslpassword=abc&passfile=/p&other=v"); strings.Contains(g, "abc") || strings.Contains(g, "/p&") {
		t.Fatalf("sibling credential keys leaked: %s", g)
	}
}

// URL-valued rows never carry an inline userinfo (review round 3): a
// user:pass@ typed into a non-secret URL row is still a credential.
func TestURLRowsDropUserinfo(t *testing.T) {
	t.Setenv("AXIOM_OPENSEARCH_URL", "http://admin:osPW@127.0.0.1:9200")
	t.Setenv("AXIOM_QUERY_RUNNER_URL", "http://k:qpw@127.0.0.1:8112")
	for _, e := range Effective(Load()) {
		switch v := e.Value.(type) {
		case string:
			if strings.Contains(v, ":ospw@") || strings.Contains(v, "osPW@") || strings.Contains(v, "qpw@") {
				t.Fatalf("userinfo survived in %s: %v", e.Env, e.Value)
			}
		case []string:
			for _, u := range v {
				if strings.Contains(strings.ToLower(u), "qpw@") {
					t.Fatalf("userinfo survived in %s: %v", e.Env, e.Value)
				}
			}
		}
	}
}

// A command path is not a credential (F08 review round 2): the legacy
// AXIOM_FIXER_CMD envRow must NOT be flagged secret — flipping its
// redaction flag back to true turns this test red, so operators keep
// seeing the actual worker path in `axiom config get --effective`.
func TestEffectiveWorkerCommandRowNotRedacted(t *testing.T) {
	deprecate.SetSilent(true)
	t.Cleanup(func() { deprecate.SetSilent(false) })
	t.Setenv("AXIOM_REPAIR_WORKER_CMD", "") // neutral: the legacy row is what we assert
	t.Setenv("AXIOM_FIXER_CMD", "/opt/axiom/bin/some-worker")

	for _, e := range Effective(Load()) {
		if e.Env != "AXIOM_FIXER_CMD" {
			continue
		}
		if e.Source != "env" {
			t.Fatalf("AXIOM_FIXER_CMD source = %q, want env", e.Source)
		}
		if e.Value != "/opt/axiom/bin/some-worker" {
			t.Fatalf("worker command path must show its value, got %v (redacted? %v)", e.Value, RedactedValue)
		}
		return
	}
	t.Fatal("AXIOM_FIXER_CMD missing from effective view")
}

// Percent-encoded credential keys (review round 3 minor): pgconn decodes
// escapes before matching query names (?pass%77ord= sets cfg.Password),
// so the redaction must catch encoded spellings too — the contract is
// absolute ("secret values never appear in any output").
func TestEncodedCredentialKeysRedacted(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u@h/db?pass%77ord=ENCODEDKEY&sslmode=disable",
		"postgres://u@h/db?%50assword=MIXED&x=1",
		"postgres://u@h/db?sslpass%77ord=S&passfile=/f",
	} {
		if got := sanitizeDSN(dsn); strings.Contains(got, "ENCODEDKEY") || strings.Contains(got, "MIXED") {
			t.Fatalf("encoded credential key survived: %s -> %s", dsn, got)
		}
	}
	if got := RedactQueryCredentials("parse `postgres://u@h/db?pass%77ord=ENC`"); strings.Contains(got, "ENC") {
		t.Fatalf("free-text encoded key leaked: %s", got)
	}
	// Structured redaction keeps the rest of the query intact.
	if got := sanitizeDSN("postgres://u@h/db?pass%77ord=ENC&sslmode=disable"); !strings.Contains(got, "sslmode=disable") {
		t.Fatalf("non-credential params must survive, got %s", got)
	}
}
