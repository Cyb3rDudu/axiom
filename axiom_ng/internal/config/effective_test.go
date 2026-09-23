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
	} {
		t.Setenv(env, val)
		problems := ValidateEnv()
		if len(problems) == 0 || !strings.Contains(problems[0], env) {
			t.Fatalf("%s=%s must be flagged, got %v", env, val, problems)
		}
		os.Unsetenv(env)
	}
	// yes/no are recognized boolean spellings (the loader's own extension).
	t.Setenv("AXIOM_SEARCH_RERANK", "no")
	if problems := ValidateEnv(); len(problems) != 0 {
		t.Fatalf("no must be accepted as false, got %v", problems)
	}
}
