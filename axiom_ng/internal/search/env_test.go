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
		{"unset → prod default", nil, "axiom-ng-chunks-v1"},
		{"empty → prod default", map[string]string{"AXIOM_OS_INDEX": ""}, "axiom-ng-chunks-v1"},
		{"whitespace → prod default", map[string]string{"AXIOM_OS_INDEX": "   "}, "axiom-ng-chunks-v1"},
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
	if IndexName != "axiom-ng-chunks-v1" {
		t.Errorf("IndexName = %q, want prod default in an unoverridden process", IndexName)
	}
}
