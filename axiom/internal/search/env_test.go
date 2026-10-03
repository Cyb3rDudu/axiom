package search

import "testing"

// AXIOM_OS_INDEX resolution (#dev-env): unset keeps the production default
// bit-identical; set (even padded) overrides; blank falls back.
func TestIndexNameFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unset → prod default", nil, "axiom-chunks-v1"},
		{"empty → prod default", map[string]string{"AXIOM_OS_INDEX": ""}, "axiom-chunks-v1"},
		{"whitespace → prod default", map[string]string{"AXIOM_OS_INDEX": "   "}, "axiom-chunks-v1"},
		{"set → override", map[string]string{"AXIOM_OS_INDEX": "axiom-dev-chunks-v1"}, "axiom-dev-chunks-v1"},
		{"padded → trimmed override", map[string]string{"AXIOM_OS_INDEX": " axiom-dev-chunks-v1 "}, "axiom-dev-chunks-v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := indexNameFromEnv(func(k string) string { return c.env[k] })
			if got != c.want {
				t.Errorf("indexNameFromEnv() = %q, want %q", got, c.want)
			}
		})
	}
	// The package-level var must equal the default when the environment of
	// THIS test process carries no override (guard against init-order rot).
	// NOTE: `go test` is not hermetic w.r.t. this var — an AXIOM_OS_INDEX
	// exported by the invoking shell fires this guard too (by design: the
	// var resolves once at process start, like the code it mirrors).
	if IndexName != "axiom-chunks-v1" {
		t.Errorf("IndexName = %q, want prod default in an unoverridden process", IndexName)
	}
}

// TestLegacyIndexForCreate pins the rename-transition mapping (#352): the
// guard arms only for the canonical default; dev indexes, explicit
// overrides and the legacy name itself stay ungoverned (the legacy name
// must never be guarded against its own transition tooling).
func TestLegacyIndexForCreate(t *testing.T) {
	cases := []struct {
		index string
		want  string
	}{
		{"axiom-chunks-v1", LegacyIndexName},
		{"axiom-ng-chunks-v1", ""}, // staying on the legacy index is the rollback path
		{"axiom-dev-chunks-v1", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := LegacyIndexForCreate(tc.index); got != tc.want {
			t.Errorf("LegacyIndexForCreate(%q) = %q, want %q", tc.index, got, tc.want)
		}
	}
	if LegacyIndexName != "axiom-ng-chunks-v1" {
		t.Errorf("LegacyIndexName = %q, want the pre-#352 production name", LegacyIndexName)
	}
}
