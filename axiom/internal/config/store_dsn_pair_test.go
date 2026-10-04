// store_dsn_pair_test.go — DM07 #316: the store DSN is dual-fed
// (canonical AXIOM_STORE_DATABASE_URL over the legacy single-DSN
// AXIOM_DATABASE_URL). These tests pin the four contract rows:
// canonical wins, legacy keeps working through the witness, the
// identical overlap passes, and the both-set-and-different conflict
// refuses in LoadResolved — never resolves by precedence. DSN rows are
// secret: the effective view renders the sanitized projection, and
// values never appear in assertions (only key names and shapes).
package config

import (
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/deprecate"
)

// resolveStoreDSN runs Load under the env pair (inside the restored-env
// window) and returns the resolved DatabaseURL. The pure env reader is
// total and deterministic — the conflict teeth live in LoadResolved.
func resolveStoreDSN(t *testing.T, legacy, canonical string) string {
	t.Helper()
	var resolved string
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_DATABASE_URL", legacy)
		t.Setenv("AXIOM_STORE_DATABASE_URL", canonical)
		resolved = Load().DatabaseURL
	})
	return resolved
}

func TestStoreDSNPairResolution(t *testing.T) {
	deprecate.SetSilent(true)
	t.Cleanup(func() { deprecate.SetSilent(false) })

	if got := resolveStoreDSN(t, "", "postgresql://h/store"); got != "postgresql://h/store" {
		t.Fatalf("canonical only: DatabaseURL = %q", got)
	}
	if got := resolveStoreDSN(t, "postgresql://h/single", ""); got != "postgresql://h/single" {
		t.Fatalf("legacy only (the single-DSN compat shape): DatabaseURL = %q", got)
	}
	// The identical overlap is the harmless one: canonical feeds, no
	// ambiguity, no conflict error later.
	if got := resolveStoreDSN(t, "postgresql://h/same", "postgresql://h/same"); got != "postgresql://h/same" {
		t.Fatalf("identical pair: DatabaseURL = %q", got)
	}
	// The Load-level fallback for both-set-different is deterministic
	// (canonical wins) so the pure env reader stays total — the LOUD
	// refusal is LoadResolved's job (TestStoreDSNPairConflictRefuses).
	if got := resolveStoreDSN(t, "postgresql://h/single", "postgresql://h/store"); got != "postgresql://h/store" {
		t.Fatalf("conflicting pair at Load level: DatabaseURL = %q", got)
	}
}

// TestStoreDSNPairWitness — the legacy single-DSN spelling records its
// use through the deprecation witness exactly like the other legacy
// spellings (the health counter is the cutover-readiness telemetry).
// Counting is relative (before/after) like the compute-worker pair
// tests — the witness is process-global.
func TestStoreDSNPairWitness(t *testing.T) {
	withRestoredEnv(t, func() {
		before := deprecate.Counts()["AXIOM_DATABASE_URL"]
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://h/single")
		t.Setenv("AXIOM_STORE_DATABASE_URL", "")
		if got := Load().DatabaseURL; got != "postgresql://h/single" {
			t.Fatalf("DatabaseURL = %q", got)
		}
		if after := deprecate.Counts()["AXIOM_DATABASE_URL"]; after != before+1 {
			t.Fatal("legacy single-DSN use must count on the witness")
		}
	})
}

// TestStoreDSNPairConflictRefuses — the hard compat rule: both spellings
// set with DIFFERENT values is an operator conflict, not a precedence
// question. LoadResolved refuses; the message names keys, never values.
func TestStoreDSNPairConflictRefuses(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_CONFIG_PATH", "")
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://conflict-a.example/db")
		t.Setenv("AXIOM_STORE_DATABASE_URL", "postgresql://conflict-b.example/db")
		_, _, err := LoadResolved(nil)
		if err == nil {
			t.Fatal("both DSN spellings set with different values must refuse")
		}
		if !strings.Contains(err.Error(), "AXIOM_DATABASE_URL") || !strings.Contains(err.Error(), "AXIOM_STORE_DATABASE_URL") {
			t.Fatalf("the refusal must name both keys: %v", err)
		}
		for _, secret := range []string{"conflict-a", "conflict-b"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatal("the refusal must never echo DSN values")
			}
		}
	})
}

// TestStoreDSNPairIdenticalOverlapBoots — both spellings set to the SAME
// value is the overlap an operator produces mid-migration: it boots.
func TestStoreDSNPairIdenticalOverlapBoots(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_CONFIG_PATH", "")
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://overlap.example/db")
		t.Setenv("AXIOM_STORE_DATABASE_URL", "postgresql://overlap.example/db")
		cfg, _, err := LoadResolved(nil)
		if err != nil {
			t.Fatalf("identical overlap must boot, got %v", err)
		}
		if cfg.DatabaseURL != "postgresql://overlap.example/db" {
			t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
		}
	})
}

// TestDSNRowsRenderSanitizedSecret — every DSN row is secret (DM07):
// the effective view shows the credential-free projection (WHERE the
// pool points) and never the password, for all three DSN keys.
func TestDSNRowsRenderSanitizedSecret(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://u:legacypw@host-a:5432/db")
		t.Setenv("AXIOM_STORE_DATABASE_URL", "postgresql://u:storepw@host-b:5432/db")
		t.Setenv("AXIOM_LIBRARY_DATABASE_URL", "postgresql://u:libpw@host-c:5432/libdb")
		entries := Effective(Load())
		byEnv := map[string]Entry{}
		for _, e := range entries {
			byEnv[e.Env] = e
		}
		for _, key := range []string{"AXIOM_DATABASE_URL", "AXIOM_STORE_DATABASE_URL", "AXIOM_LIBRARY_DATABASE_URL"} {
			e, ok := byEnv[key]
			if !ok {
				t.Fatalf("%s missing from the effective table", key)
			}
			s, isStr := e.Value.(string)
			if !isStr {
				t.Fatalf("%s renders %T, want the sanitized DSN string", key, e.Value)
			}
			if strings.Contains(s, "legacypw") || strings.Contains(s, "storepw") || strings.Contains(s, "libpw") {
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
// value row: plaintext DSNs do not land in config.sqlite (DM07).
func TestEnvImportRowsReferencesDSNs(t *testing.T) {
	withRestoredEnv(t, func() {
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://u:pw@h/db")
		t.Setenv("AXIOM_LIBRARY_DATABASE_URL", "postgresql://u:pw@h/lib")
		t.Setenv("AXIOM_STORE_DATABASE_URL", "postgresql://u:pw@h/store")
		values, refs := EnvImportRows()
		// Pair-aware import (the compute-worker pair rule): the canonical
		// spelling feeds → it imports as the reference; the legacy
		// AXIOM_DATABASE_URL row is skipped (dead config must not land in
		// the file). Neither key may appear as a VALUE row.
		for _, key := range []string{"AXIOM_LIBRARY_DATABASE_URL", "AXIOM_STORE_DATABASE_URL"} {
			if _, has := values[key]; has {
				t.Fatalf("%s imported as a VALUE row — DSNs are reference-only", key)
			}
			if refs[key] != "env" {
				t.Fatalf("%s must import as a secret reference, got %q", key, refs[key])
			}
		}
		if _, has := values["AXIOM_DATABASE_URL"]; has {
			t.Fatal("the shadowed legacy DSN spelling must not import as a value row")
		}
		if refs["AXIOM_DATABASE_URL"] != "" {
			t.Fatal("the shadowed legacy DSN spelling must not import a dead reference (canonical feeds)")
		}
	})
}
