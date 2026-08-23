package version

import "testing"

func env(v string) func(string) string { return func(string) string { return v } }

// #205 §5: refuse = non-release build AND production port AND no explicit
// opt-out. Boundary ports: 8010/8016 free, 8011/8015 production range;
// 26000 is the new production API port (25999/26001 free).
func TestDebugBindRefused(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		port int
		val  string
		want bool
	}{
		{"debug 8011 no optout", "debug", 8011, "", true},
		{"debug 8015 no optout", "debug", 8015, "", true},
		{"debug 26000 no optout", "debug", 26000, "", true},
		{"debug 8010 free port", "debug", 8010, "", false},
		{"debug 8016 free port", "debug", 8016, "", false},
		{"debug 25999 free port", "debug", 25999, "", false},
		{"debug 26001 free port", "debug", 26001, "", false},
		{"debug 8011 optout", "debug", 8011, "1", false},
		{"debug 8013 optout", "debug", 8013, "1", false},
		{"debug 26000 optout", "debug", 26000, "1", false},
		{"release 8011 no optout", "release", 8011, "", false},
		{"release 8015 no optout", "release", 8015, "", false},
		{"release 26000 no optout", "release", 26000, "", false},
		{"release 8010", "release", 8010, "", false},
		{"debug 8012 partial optout", "debug", 8012, "yes", true},
		{"debug 26000 partial optout", "debug", 26000, "yes", true},
	}
	for _, c := range cases {
		if got := DebugBindRefused(c.typ, c.port, env(c.val)); got != c.want {
			t.Errorf("%s: DebugBindRefused(%q,%d,env=%q) = %v, want %v",
				c.name, c.typ, c.port, c.val, got, c.want)
		}
	}
}
