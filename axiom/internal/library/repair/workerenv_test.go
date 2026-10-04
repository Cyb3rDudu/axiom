// workerenv_test.go — DM07 #316 witness: the repair worker child env is
// allowlist-constructed, every known credential name is absent, and the
// assertions name KEYS, never values (no env dumps — the same rule the
// production check follows).
package repair

import (
	"strings"
	"testing"
)

// TestWorkerEnvAllowlistPresentDenylistAbsent — the constructed env
// carries the minimal required variables (allowlist check) and none of
// the known credential variables (denylist check). Values are matched
// only as fixtures planted for THIS test, never printed.
func TestWorkerEnvAllowlistPresentDenylistAbsent(t *testing.T) {
	t.Setenv("PATH", "/fixture/bin")
	t.Setenv("HOME", "/fixture/home")
	t.Setenv("TMPDIR", "/fixture/tmp")
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_CTYPE", "de_DE.UTF-8")
	t.Setenv("AXIOM_RAG_URL", "http://127.0.0.1:8011")
	t.Setenv("DEEPSEEK_API_KEY", "fixture-deepseek-key")
	t.Setenv("AXIOM_FIXSVC_NO_SYNC", "1")
	// hostile parent env: every credential name the parent may hold
	for _, name := range workerEnvDeniedNames {
		t.Setenv(name, "fixture-do-not-leak")
	}
	// plus an arbitrary non-allowlisted var — not a credential, still
	// none of the worker's business
	t.Setenv("SOME_UNRELATED_VAR", "x")

	env := workerEnv("AXIOM_FIX_SH_TIMEOUT=900")
	have := map[string]string{}
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		have[name] = val
	}
	// allowlist check: the minimal required variables are present
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_CTYPE", "AXIOM_RAG_URL", "DEEPSEEK_API_KEY", "AXIOM_FIXSVC_NO_SYNC", "AXIOM_FIX_SH_TIMEOUT"} {
		if have[name] == "" {
			t.Errorf("required worker var %s missing from the constructed env (names only: %v)", name, keysOf(env))
		}
	}
	// denylist check: known credential names are absent
	for _, name := range workerEnvDeniedNames {
		if _, present := have[name]; present {
			t.Errorf("credential var %s leaked into the worker env", name)
		}
	}
	if _, present := have["SOME_UNRELATED_VAR"]; present {
		t.Error("non-allowlisted var copied — the env must be constructed, not inherited")
	}
	// sorted determinism: assertions stay stable across OS env orderings
	if n := len(env) - 1; n > 1 && !sortedAsc(env[:n]) {
		t.Errorf("constructed env must sort deterministically (extras last), got %v", keysOf(env))
	}
}

func keysOf(env []string) []string {
	out := make([]string, len(env))
	for i, kv := range env {
		out[i], _, _ = strings.Cut(kv, "=")
	}
	return out
}

func sortedAsc(env []string) bool {
	for i := 1; i < len(env); i++ {
		if env[i-1] > env[i] {
			return false
		}
	}
	return true
}

// TestWorkerEnvNeverEchoesValues — failure diagnostics operate on
// NAMES; a broken construction must not turn into a value dump. The
// workerenv.go code paths log/assert nothing value-shaped; this test
// pins that the denied vocabulary itself carries no values through
// workerEnv (name-only witness).
func TestWorkerEnvNeverEchoesValues(t *testing.T) {
	t.Setenv("AXIOM_DATABASE_URL", "postgresql://u:fixture-secret@h/db")
	env := workerEnv()
	for _, kv := range env {
		if strings.Contains(kv, "fixture-secret") {
			t.Fatal("a credential VALUE crossed into the worker env")
		}
	}
}
