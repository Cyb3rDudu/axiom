package version

import "testing"

func env(v string) func(string) string { return func(string) string { return v } }

// #205 §5: refuse = non-release build AND production port AND no explicit
// opt-out. Boundary ports: 8010/8016 free, 8011/8015 production range.
// #352 identity witness: the dev-default banner, literally. The other
// banner pins in the tree are self-referential (they compare against
// Banner() output), so a drifted product name would pass them; this
// literal goes red if the legacy "axiom-ng" prefix (or any format
// drift) comes back. Release builds override via -ldflags, not here.
func TestBannerDefaultLiteral(t *testing.T) {
	want := "axiom dev (commit none, debug build)"
	if got := Banner(); got != want {
		t.Fatalf("default banner = %q, want %q", got, want)
	}
}

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
		{"debug 8010 free port", "debug", 8010, "", false},
		{"debug 8016 free port", "debug", 8016, "", false},
		{"debug 8011 optout", "debug", 8011, "1", false},
		{"debug 8013 optout", "debug", 8013, "1", false},
		{"release 8011 no optout", "release", 8011, "", false},
		{"release 8015 no optout", "release", 8015, "", false},
		{"release 8010", "release", 8010, "", false},
		{"debug 8012 partial optout", "debug", 8012, "yes", true},
	}
	for _, c := range cases {
		if got := DebugBindRefused(c.typ, c.port, env(c.val)); got != c.want {
			t.Errorf("%s: DebugBindRefused(%q,%d,env=%q) = %v, want %v",
				c.name, c.typ, c.port, c.val, got, c.want)
		}
	}
}
