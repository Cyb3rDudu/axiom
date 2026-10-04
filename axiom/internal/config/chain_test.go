// chain_test.go — the F13 #307 resolution-chain witnesses: the
// precedence sonde (each stage set + overridden shows the right source
// and value), the mutation sonde (cutting a stage lets the next one
// win), the env-only bootstrap proof (no file = byte-identical Load),
// and the validation teeth (unknown key, type violation, secret
// refusal, vocabulary) for file rows and --set flags.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/config/configstore"
)

// TestMain pins the chain's file location to an ABSENT path for the
// whole package (the internal/cli shape): a config.sqlite on the test
// host must never leak into these witnesses, and a future LoadResolved
// call without its own override reads nothing instead of the operator's
// machine state. Chain tests that want a file pin their own path.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "axiom-config-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	os.Setenv("AXIOM_CONFIG_PATH", dir+"/absent/config.sqlite")
	os.Exit(m.Run())
}

// withRestoredEnv snapshots and restores the process environment around
// fn — LoadResolved materializes into it by design, so the restore also
// UNSETS keys the wrapped run created (a materialized file/flag value
// must not masquerade as env in the next test).
func withRestoredEnv(t *testing.T, fn func()) {
	t.Helper()
	snapshot := os.Environ()
	restore := func() {
		inSnapshot := map[string]bool{}
		for _, kv := range snapshot {
			k, _, _ := strings.Cut(kv, "=")
			inSnapshot[k] = true
		}
		for _, kv := range snapshot {
			k, _, _ := strings.Cut(kv, "=")
			os.Unsetenv(k)
		}
		for _, kv := range snapshot {
			k, v, _ := strings.Cut(kv, "=")
			os.Setenv(k, v)
		}
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			if !inSnapshot[k] {
				os.Unsetenv(k)
			}
		}
	}
	defer restore()
	fn()
}

// fileBackedConfig points the chain at a fresh config.sqlite.
func fileBackedConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("AXIOM_CONFIG_PATH", filepath.Join(t.TempDir(), "config.sqlite"))
	path, err := configstore.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// rowOf finds one entry in the rendered view.
func rowOf(t *testing.T, entries []Entry, env string) Entry {
	t.Helper()
	for _, e := range entries {
		if e.Env == env {
			return e
		}
	}
	t.Fatalf("no row for %s", env)
	return Entry{}
}

// TestPrecedenceChainEachStageWins — the precedence sonde: a probe key
// climbs the chain stage by stage; each stronger stage overrides the
// weaker and --effective names the feeding stage.
func TestPrecedenceChainEachStageWins(t *testing.T) {
	withRestoredEnv(t, func() {
		path := fileBackedConfig(t)
		st, err := configstore.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.Set("AXIOM_API_PORT", "9101"); err != nil {
			t.Fatal(err)
		}

		// file over default
		os.Unsetenv("AXIOM_API_PORT")
		cfg, ch, err := LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIPort != 9101 {
			t.Fatalf("file stage: APIPort = %d, want 9101", cfg.APIPort)
		}
		if e := rowOf(t, EffectiveChain(cfg, ch), "AXIOM_API_PORT"); e.Source != SourceFile || e.Value != 9101 {
			t.Fatalf("file stage row = %+v, want source=file value=9101", e)
		}

		// env over file
		t.Setenv("AXIOM_API_PORT", "9102")
		cfg, ch, err = LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIPort != 9102 {
			t.Fatalf("env stage: APIPort = %d, want 9102", cfg.APIPort)
		}
		if e := rowOf(t, EffectiveChain(cfg, ch), "AXIOM_API_PORT"); e.Source != SourceEnv {
			t.Fatalf("env stage row = %+v, want source=env", e)
		}

		// flag over env over file
		cfg, ch, err = LoadResolved(map[string]string{"AXIOM_API_PORT": "9103"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIPort != 9103 {
			t.Fatalf("flag stage: APIPort = %d, want 9103", cfg.APIPort)
		}
		if e := rowOf(t, EffectiveChain(cfg, ch), "AXIOM_API_PORT"); e.Source != SourceFlag {
			t.Fatalf("flag stage row = %+v, want source=flag", e)
		}
	})
}

// TestPrecedenceMutationSonde — cutting a stage lets the next one win:
// without the flag the env wins, without env+flag the file wins,
// without all three the default. The chain must move, not freeze.
// Each cut models a FRESH process (the one-resolution-per-process
// contract in LoadResolved's doc): the probe key's env is cleared to
// the stage's truth before each resolution — a materialized value
// from an earlier resolution never masquerades as env.
func TestPrecedenceMutationSonde(t *testing.T) {
	withRestoredEnv(t, func() {
		path := fileBackedConfig(t)
		st, err := configstore.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.Set("AXIOM_BIND_ADDR", "10.0.0.9"); err != nil {
			t.Fatal(err)
		}

		t.Setenv("AXIOM_BIND_ADDR", "10.0.0.8")
		cfg, ch, err := LoadResolved(map[string]string{"AXIOM_BIND_ADDR": "10.0.0.7"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BindAddr != "10.0.0.7" {
			t.Fatalf("flag must win, got %q", cfg.BindAddr)
		}
		// flag stage cut: fresh process without the flag, env unchanged
		os.Setenv("AXIOM_BIND_ADDR", "10.0.0.8")
		cfg, ch, err = LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BindAddr != "10.0.0.8" || rowOf(t, EffectiveChain(cfg, ch), "AXIOM_BIND_ADDR").Source != SourceEnv {
			t.Fatalf("cutting the flag must expose env, got %q", cfg.BindAddr)
		}
		os.Unsetenv("AXIOM_BIND_ADDR") // env stage cut
		cfg, ch, err = LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BindAddr != "10.0.0.9" || rowOf(t, EffectiveChain(cfg, ch), "AXIOM_BIND_ADDR").Source != SourceFile {
			t.Fatalf("cutting env must expose the file row, got %q", cfg.BindAddr)
		}
		if err := st.Unset("AXIOM_BIND_ADDR"); err != nil { // file stage cut
			t.Fatal(err)
		}
		os.Unsetenv("AXIOM_BIND_ADDR") // fresh process: no stale materialization
		cfg, ch, err = LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BindAddr != defaultBindAddr {
			t.Fatalf("cutting the file must expose the default, got %q", cfg.BindAddr)
		}
		if e := rowOf(t, EffectiveChain(cfg, ch), "AXIOM_BIND_ADDR"); e.Source != SourceDefault {
			t.Fatalf("the default cut must render source=default, got %+v", e)
		}
	})
}

// TestEnvOnlyBootstrapIdenticalToLoad — the container path proof: with
// NO config.sqlite, LoadResolved resolves byte-identically to the pure
// env Load (the chain is a no-op without a file — every existing
// witness's premise).
func TestEnvOnlyBootstrapIdenticalToLoad(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_CONFIG_PATH", filepath.Join(t.TempDir(), "absent", "config.sqlite"))
		t.Setenv("AXIOM_API_PORT", "8111")
		t.Setenv("AXIOM_SEARCH_RERANK", "false")
		want := Load()
		got, ch, err := LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("env-only resolution must equal pure Load")
		}
		if !ch.File["AXIOM_API_PORT"] == false || len(ch.File) != 0 {
			t.Fatalf("no file → no file provenance, got %v", ch.File)
		}
		if e := rowOf(t, EffectiveChain(got, ch), "AXIOM_API_PORT"); e.Source != SourceEnv {
			t.Fatalf("source = %s, want env", e.Source)
		}
	})
}

// TestInvalidFileRefusesResolution — a present-but-invalid file is a
// LOUD error (never a silent fall-back): secret values, unknown keys,
// unparseable values.
func TestInvalidFileRefusesResolution(t *testing.T) {
	withRestoredEnv(t, func() {
		path := fileBackedConfig(t)
		st, err := configstore.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		for key, value := range map[string]string{
			"AXIOM_WS_SECRET":              "the-ws-secret", // secret VALUE — refused
			"AXIOM_NO_SUCH_KEY":            "x",             // unknown vocabulary
			"AXIOM_API_PORT":               "not-a-port",    // type violation
			"AXIOM_STORAGE_LIBRARY_DRIVER": "oracle",        // vocabulary violation
		} {
			if err := st.Set(key, value); err != nil {
				t.Fatal(err)
			}
		}
		_, _, err = LoadResolved(nil)
		if err == nil {
			t.Fatal("invalid file must refuse resolution")
		}
		for _, want := range []string{"AXIOM_WS_SECRET", "AXIOM_NO_SUCH_KEY", "AXIOM_API_PORT", "AXIOM_STORAGE_LIBRARY_DRIVER"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error must name %s, got: %v", want, err)
			}
		}
	})
}

// TestLoadResolvedNoHomeIsEnvOnly — an unresolvable default path (no
// home dir, no override) must NOT abort the boot: the chain degrades
// to the env-only bootstrap, the pre-F13 no-HOME behavior (writers
// keep the loud error; doctor's config-file check keeps reporting it).
func TestLoadResolvedNoHomeIsEnvOnly(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_CONFIG_PATH", "") // un-pin: exercise the DEFAULT path
		t.Setenv("HOME", "")              // darwin/linux: UserHomeDir errors
		t.Setenv("AXIOM_API_PORT", "8123")
		cfg, ch, err := LoadResolved(nil)
		if err != nil {
			t.Fatalf("no-home boot must stay env-only, got %v", err)
		}
		if cfg.APIPort != 8123 {
			t.Fatalf("APIPort = %d, want the env value", cfg.APIPort)
		}
		if len(ch.File) != 0 {
			t.Fatalf("no path → no file provenance, got %v", ch.File)
		}
	})
}

// TestParseSetFlags — both --set shapes, ordering, and syntax teeth.
func TestParseSetFlags(t *testing.T) {
	flags, rest, err := ParseSetFlags([]string{"serve", "--set", "AXIOM_API_PORT=9001", "--set=AXIOM_BIND_ADDR=0.0.0.0", "api"})
	if err != nil {
		t.Fatal(err)
	}
	if flags["AXIOM_API_PORT"] != "9001" || flags["AXIOM_BIND_ADDR"] != "0.0.0.0" {
		t.Fatalf("flags = %v", flags)
	}
	if !reflect.DeepEqual(rest, []string{"serve", "api"}) {
		t.Fatalf("rest = %v", rest)
	}
	if _, _, err := ParseSetFlags([]string{"--set"}); err == nil {
		t.Fatal("dangling --set must error")
	}
	if _, _, err := ParseSetFlags([]string{"--set", "NOEQUALS"}); err == nil {
		t.Fatal("missing = must error")
	}
}

// TestValidateFlagsTeeth — unknown key, type violation, value range,
// inline credential, secret refusal (the good cases ride the precedence
// sonde).
func TestValidateFlagsTeeth(t *testing.T) {
	problems := ValidateFlags(map[string]string{
		"AXIOM_NOPE":             "x",
		"AXIOM_API_PORT":         "eighty",
		"AXIOM_WS_SECRET":        "s3cret",
		"AXIOM_SEARCH_RERANK":    "maybe",
		"AXIOM_FIXER_INTERVAL":   "5mins",
		"AXIOM_QUERY_RUNNER_URL": "http://u:pw@runner:8012",
	})
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"AXIOM_NOPE", "AXIOM_API_PORT", "AXIOM_WS_SECRET", "AXIOM_SEARCH_RERANK", "AXIOM_FIXER_INTERVAL", "AXIOM_QUERY_RUNNER_URL", "credential"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems must name %s, got: %v", want, problems)
		}
	}
	if problems := ValidateFlags(map[string]string{"AXIOM_API_PORT": "9001", "AXIOM_SEARCH_RERANK": "false"}); len(problems) != 0 {
		t.Fatalf("valid flags must pass, got %v", problems)
	}
	if problems := ValidateFlags(map[string]string{"AXIOM_API_PORT": "70000"}); len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "out of range") {
		t.Fatalf("out-of-range port must be refused, got %v", problems)
	}
}

// TestValidateSettingsTeeth — file-row rules: unknown key, secret
// value, empty-as-unset, value range, inline credential, legal empty
// only for the set-empty-disable key.
func TestValidateSettingsTeeth(t *testing.T) {
	problems := ValidateSettings(map[string]string{
		"AXIOM_NOPE":                 "x",
		"AXIOM_DATABASE_URL":         "postgresql://u:p@h/db",
		"AXIOM_SEARCH_RERANK":        "",
		"AXIOM_DISPATCHER_LEASE":     "5x",
		"AXIOM_API_PORT":             "99999",
		"AXIOM_LIBRARY_DATABASE_URL": "postgresql://u:pw@h/lib",
	}, map[string]string{"AXIOM_WS_SECRET": "env"})
	joined := strings.Join(problems, "\n")
	// DM07 #316: both DSN rows are secret — the password-bearing library
	// DSN now trips the SECRET refusal (references only) before any
	// inline-credential check; the inline-credential teeth stay pinned on
	// non-secret URL rows (TestValidateFlagsTeeth / TestValidateSettingsTeeth above).
	for _, want := range []string{"AXIOM_NOPE", "AXIOM_DATABASE_URL", "AXIOM_SEARCH_RERANK", "AXIOM_DISPATCHER_LEASE", "AXIOM_API_PORT", "out of range", "AXIOM_LIBRARY_DATABASE_URL", "secret keys carry references"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems must name %s, got: %v", want, problems)
		}
	}
	if problems := ValidateSettings(map[string]string{"AXIOM_OPENSEARCH_URL": ""}, nil); len(problems) != 0 {
		t.Fatalf("the set-empty disable row is legal, got %v", problems)
	}
	// DM07 #316: every DSN row is secret — config.sqlite carries the
	// reference only, even credential-free projections (the value rides
	// env / the OS secret store; WHERE the pool points stays visible via
	// `config get --effective`, sanitized).
	if problems := ValidateSettings(map[string]string{"AXIOM_LIBRARY_DATABASE_URL": "postgresql://h/lib"}, nil); len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "secret keys carry references") {
		t.Fatalf("a DSN row must be reference-only (DM07), got %v", problems)
	}
	if problems := ValidateSettings(map[string]string{"AXIOM_STORE_DATABASE_URL": "postgresql://h/store"}, nil); len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "secret keys carry references") {
		t.Fatalf("the canonical store DSN row must be reference-only (DM07), got %v", problems)
	}
	if problems := ValidateSettings(nil, map[string]string{"AXIOM_WS_SECRET": "keychain"}); len(problems) == 0 {
		t.Fatal("unknown ref source must be a problem")
	}
	if problems := ValidateSettings(nil, map[string]string{"AXIOM_API_PORT": "env"}); len(problems) == 0 {
		t.Fatal("ref on a non-secret key must be a problem")
	}
}

// TestEmptyDisablesKeysCoverLoader — source-parse guard: every key
// config.go reads through envEmptyDisables (set-but-empty is
// MEANINGFUL) must be in emptyDisablesKeys, or the chain's env-stage
// truth silently drifts from the loader.
func TestEmptyDisablesKeysCoverLoader(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	re := regexp.MustCompile(`envEmptyDisables\("(AXIOM_[A-Z0-9_]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatal("sonde is vacuous — envEmptyDisables call shape drifted")
	}
	for key := range seen {
		if !emptyDisablesKeys[key] {
			t.Errorf("config.go reads %s with set-but-empty semantics but emptyDisablesKeys does not know it", key)
		}
	}
	for key := range emptyDisablesKeys {
		if !seen[key] {
			t.Errorf("emptyDisablesKeys carries %s but the loader never reads it with envEmptyDisables", key)
		}
	}
}

// TestDualFedChainStages — dual-fed fields carry chain provenance on
// the spelling that fed the value; the shadowed legacy spelling stays
// default (the F08/F10 truth generalized over the chain).
func TestDualFedChainStages(t *testing.T) {
	withRestoredEnv(t, func() {
		fileBackedConfig(t)
		t.Setenv("AXIOM_PROCESSOR_URL", "http://legacy:8012") // legacy spelling in env
		cfg, ch, err := LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		entries := EffectiveChain(cfg, ch)
		if e := rowOf(t, entries, "AXIOM_PROCESSOR_URL"); e.Source != SourceEnv {
			t.Fatalf("legacy row must read env, got %+v", e)
		}
		if e := rowOf(t, entries, "AXIOM_COMPUTE_WORKER_URL"); e.Source != SourceDefault {
			t.Fatalf("shadowed canonical row must read default, got %+v", e)
		}
		if cfg.ProcessorURL != "http://legacy:8012" {
			t.Fatalf("ProcessorURL = %q", cfg.ProcessorURL)
		}
		// Canonical in env shadows legacy everywhere.
		t.Setenv("AXIOM_COMPUTE_WORKER_URL", "http://canon:8012")
		cfg, ch, err = LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		entries = EffectiveChain(cfg, ch)
		if e := rowOf(t, entries, "AXIOM_COMPUTE_WORKER_URL"); e.Source != SourceEnv {
			t.Fatalf("canonical row must read env, got %+v", e)
		}
		if e := rowOf(t, entries, "AXIOM_PROCESSOR_URL"); e.Source != SourceDefault {
			t.Fatalf("shadowed legacy row must read default, got %+v", e)
		}
		// File row on the legacy spelling feeds when env is absent.
		os.Unsetenv("AXIOM_COMPUTE_WORKER_URL")
		os.Unsetenv("AXIOM_PROCESSOR_URL")
		path, _ := configstore.DefaultPath()
		st, _ := configstore.Open(path)
		if err := st.Set("AXIOM_PROCESSOR_URL", "http://file:8012"); err != nil {
			t.Fatal(err)
		}
		st.Close()
		cfg, ch, err = LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		entries = EffectiveChain(cfg, ch)
		if e := rowOf(t, entries, "AXIOM_PROCESSOR_URL"); e.Source != SourceFile {
			t.Fatalf("legacy file row must read file, got %+v", e)
		}
		if cfg.ProcessorURL != "http://file:8012" {
			t.Fatalf("ProcessorURL = %q, want the file row", cfg.ProcessorURL)
		}
	})
}

// TestDualFedFieldLevelChain — the probe matrix pinning the
// field-level decision: the chain (flag > env > file) binds the LOGICAL
// knob on dual-fed pairs; the canonical-over-legacy spelling rule
// breaks ties WITHIN a stage, never across stages. (i) env on the
// legacy spelling beats a file row on the canonical one; (ii) a --set
// on the legacy spelling beats env on the canonical one (materialized
// onto the canonical key, source=flag); (iii) --set naming both
// spellings resolves to the canonical-named flag.
func TestDualFedFieldLevelChain(t *testing.T) {
	withRestoredEnv(t, func() {
		path := fileBackedConfig(t)
		st, err := configstore.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.Set("AXIOM_COMPUTE_WORKER_URL", "http://file-canonical:8012"); err != nil {
			t.Fatal(err)
		}

		// (i) env=legacy vs file=canonical: the ENVIRONMENT wins the field.
		os.Setenv("AXIOM_PROCESSOR_URL", "http://env-legacy:8012")
		cfg, ch, err := LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ProcessorURL != "http://env-legacy:8012" {
			t.Fatalf("(i) ProcessorURL = %q, want the env value", cfg.ProcessorURL)
		}
		entries := EffectiveChain(cfg, ch)
		if e := rowOf(t, entries, "AXIOM_COMPUTE_WORKER_URL"); e.Source != SourceDefault {
			t.Fatalf("(i) shadowed canonical file row must render default, got %+v", e)
		}
		if e := rowOf(t, entries, "AXIOM_PROCESSOR_URL"); e.Source != SourceEnv {
			t.Fatalf("(i) legacy row must render env, got %+v", e)
		}

		// (ii) env=canonical vs --set legacy: the FLAG wins the field.
		os.Unsetenv("AXIOM_PROCESSOR_URL")
		os.Setenv("AXIOM_COMPUTE_WORKER_URL", "http://env-canonical:8012")
		cfg, ch, err = LoadResolved(map[string]string{"AXIOM_PROCESSOR_URL": "http://flag-legacy:8012"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ProcessorURL != "http://flag-legacy:8012" {
			t.Fatalf("(ii) ProcessorURL = %q, want the flag value", cfg.ProcessorURL)
		}
		if e := rowOf(t, EffectiveChain(cfg, ch), "AXIOM_COMPUTE_WORKER_URL"); e.Source != SourceFlag {
			t.Fatalf("(ii) canonical row must render flag, got %+v", e)
		}

		// (iii) --set on both spellings: the canonical-named flag wins.
		os.Unsetenv("AXIOM_COMPUTE_WORKER_URL")
		cfg, _, err = LoadResolved(map[string]string{
			"AXIOM_PROCESSOR_URL":      "http://flag-legacy:8012",
			"AXIOM_COMPUTE_WORKER_URL": "http://flag-canonical:8012",
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ProcessorURL != "http://flag-canonical:8012" {
			t.Fatalf("(iii) ProcessorURL = %q, want the canonical-named flag value", cfg.ProcessorURL)
		}
		// (iv) env=canonical vs file row on the LEGACY spelling: the
		// environment still wins the field (the pair skip is symmetric).
		os.Setenv("AXIOM_COMPUTE_WORKER_URL", "http://env-canonical:8012")
		os.Unsetenv("AXIOM_PROCESSOR_URL")
		fst, err := configstore.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := fst.Set("AXIOM_PROCESSOR_URL", "http://file-legacy:8012"); err != nil {
			t.Fatal(err)
		}
		fst.Close()
		cfg, ch, err = LoadResolved(nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ProcessorURL != "http://env-canonical:8012" {
			t.Fatalf("(iv) ProcessorURL = %q, want the env value — a legacy file row must not beat canonical env", cfg.ProcessorURL)
		}
		if e := rowOf(t, EffectiveChain(cfg, ch), "AXIOM_COMPUTE_WORKER_URL"); e.Source != SourceEnv {
			t.Fatalf("(iv) canonical row must render env, got %+v", e)
		}
		// (v) flag=canonical vs env=legacy: the flag wins, source=flag.
		os.Setenv("AXIOM_PROCESSOR_URL", "http://env-legacy:8012")
		os.Unsetenv("AXIOM_COMPUTE_WORKER_URL")
		cfg, ch, err = LoadResolved(map[string]string{"AXIOM_COMPUTE_WORKER_URL": "http://flag-canonical:8012"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ProcessorURL != "http://flag-canonical:8012" {
			t.Fatalf("(v) ProcessorURL = %q, want the flag value", cfg.ProcessorURL)
		}
		if e := rowOf(t, EffectiveChain(cfg, ch), "AXIOM_COMPUTE_WORKER_URL"); e.Source != SourceFlag {
			t.Fatalf("(v) canonical row must render flag, got %+v", e)
		}
	})
}
