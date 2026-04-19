package version

import "testing"

func TestCurrentReturnsLinkerFlags(t *testing.T) {
	t.Parallel()
	prevV, prevC, prevD := Version, Commit, Date
	Version, Commit, Date = "1.2.3", "deadbeef", "2026-04-19"
	t.Cleanup(func() { Version, Commit, Date = prevV, prevC, prevD })

	got := Current()
	want := Info{Version: "1.2.3", Commit: "deadbeef", Date: "2026-04-19"}
	if got != want {
		t.Errorf("Current(): got %+v, want %+v", got, want)
	}
}
