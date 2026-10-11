// store_dsn_pair_test.go — #368 (ADR 0001 executed, v0.2.4): the store
// DSN is single-fed. The legacy single-DSN AXIOM_DATABASE_URL spelling
// is GONE: the loader ignores it, the refusal surfaces name the canonical
// successor AXIOM_STORE_DATABASE_URL, and the old dual-fed conflict
// guard is dead with the second spelling. DSN rows stay secret (DM07):
// the effective view renders the sanitized projection, values never
// appear in assertions (only key names and shapes).
package config

import (
	"strings"
	"testing"
)

// TestStoreDSNCanonicalOnly — the loader reads AXIOM_STORE_DATABASE_URL
// only; a set legacy spelling changes nothing.
func TestStoreDSNCanonicalOnly(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://h/legacy")
		t.Setenv("AXIOM_STORE_DATABASE_URL", "")
		if got := Load().DatabaseURL; got != "" {
			t.Fatalf("legacy AXIOM_DATABASE_URL must not feed the loader, got %q", got)
		}
		t.Setenv("AXIOM_STORE_DATABASE_URL", "postgresql://h/store")
		if got := Load().DatabaseURL; got != "postgresql://h/store" {
			t.Fatalf("canonical DatabaseURL = %q", got)
		}
	})
}

// TestStoreDSNLegacyRefusalNamesSuccessor — every refusal surface names
// the canonical successor for the removed spelling.
func TestStoreDSNLegacyRefusalNamesSuccessor(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://h/legacy")
		if problems := ValidateEnv(); !strings.Contains(strings.Join(problems, "\n"), "AXIOM_STORE_DATABASE_URL") {
			t.Fatalf("ValidateEnv must name the successor, got %v", problems)
		}
	})
	problems := ValidateFlags(map[string]string{"AXIOM_DATABASE_URL": "postgresql://h/legacy"})
	if !strings.Contains(strings.Join(problems, "\n"), "AXIOM_STORE_DATABASE_URL") {
		t.Fatalf("ValidateFlags must name the successor, got %v", problems)
	}
}

// TestStoreDSNIdenticalOverlapBoots — a leftover legacy spelling next to
// the canonical one no longer conflicts (it feeds nothing).
func TestStoreDSNIdenticalOverlapBoots(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_CONFIG_PATH", "")
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://overlap.example/db")
		t.Setenv("AXIOM_STORE_DATABASE_URL", "postgresql://overlap.example/db")
		cfg, _, err := LoadResolved(nil)
		if err != nil {
			t.Fatalf("inert legacy spelling must not block boot, got %v", err)
		}
		if cfg.DatabaseURL != "postgresql://overlap.example/db" {
			t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
		}
	})
}

// TestDSNRowsRenderSanitizedSecret — every DSN row is secret (DM07):
// the effective view shows the credential-free projection (WHERE the
// pool points) and never the password, for both DSN keys.
func TestDSNRowsRenderSanitizedSecret(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_STORE_DATABASE_URL", "postgresql://u:storepw@host-b:5432/db")
		t.Setenv("AXIOM_LIBRARY_DATABASE_URL", "postgresql://u:libpw@host-c:5432/libdb")
		entries := Effective(Load())
		byEnv := map[string]Entry{}
		for _, e := range entries {
			byEnv[e.Env] = e
		}
		for _, key := range []string{"AXIOM_STORE_DATABASE_URL", "AXIOM_LIBRARY_DATABASE_URL"} {
			e, ok := byEnv[key]
			if !ok {
				t.Fatalf("%s missing from the effective table", key)
			}
			s, isStr := e.Value.(string)
			if !isStr {
				t.Fatalf("%s renders %T, want the sanitized DSN string", key, e.Value)
			}
			if strings.Contains(s, "storepw") || strings.Contains(s, "libpw") {
				t.Fatalf("%s renders a credential: %q", key, s)
			}
			if !strings.Contains(s, "host-") {
				t.Fatalf("%s must keep the credential-free host projection, got %q", key, s)
			}
		}
	})
}

// TestEnvImportRowsReferencesDSNs — the env importer (config set
// import-env) turns every DSN key into a secret REFERENCE, never a
// value row: plaintext DSNs do not land in config.sqlite (DM07). The
// removed spelling imports nothing.
func TestEnvImportRowsReferencesDSNs(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://u:pw@h/db")
		t.Setenv("AXIOM_LIBRARY_DATABASE_URL", "postgresql://u:pw@h/lib")
		t.Setenv("AXIOM_STORE_DATABASE_URL", "postgresql://u:pw@h/store")
		values, refs := EnvImportRows()
		for _, key := range []string{"AXIOM_LIBRARY_DATABASE_URL", "AXIOM_STORE_DATABASE_URL"} {
			if _, has := values[key]; has {
				t.Fatalf("%s imported as a VALUE row — DSNs are reference-only", key)
			}
			if refs[key] != "env" {
				t.Fatalf("%s must import as a secret reference, got %q", key, refs[key])
			}
		}
		if _, has := values["AXIOM_DATABASE_URL"]; has {
			t.Fatal("the removed legacy DSN spelling must not import as a value row")
		}
		if refs["AXIOM_DATABASE_URL"] != "" {
			t.Fatal("the removed legacy DSN spelling must not import a reference")
		}
	})
}
