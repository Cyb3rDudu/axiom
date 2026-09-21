package dispatcher

// Read/write index agreement witness: the outbox drainer must resolve to
// the SAME chunks index the search side reads — search.IndexName is the
// single source of truth, outboxIndexName follows it (outbox.go).
//
// The package vars resolve at init, so an in-process t.Setenv is inert;
// the test re-execs its own binary with AXIOM_OS_INDEX set (same pattern
// as cmd/axiom-ng/mode_exit_it_test.go). Red probe: revert outbox.go's
// `var outboxIndexName = search.IndexName` to a hard-coded const while
// search keeps the env knob — the child run fails, because the drainer
// index no longer follows the override.
import (
	"os"
	"os/exec"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

const agreementProbeIndex = "axiom-os-index-coupling-probe"

func TestOutboxIndexFollowsSearchIndex(t *testing.T) {
	if os.Getenv("AXIOM_OS_INDEX") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=TestOutboxIndexFollowsSearchIndex", "-test.v")
		cmd.Env = append(os.Environ(), "AXIOM_OS_INDEX="+agreementProbeIndex)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("drainer index decoupled from search.IndexName (child run with AXIOM_OS_INDEX=%s failed): %v\n%s",
				agreementProbeIndex, err, out)
		}
		return
	}
	// child: env is set — BOTH sides must have moved with it
	if outboxIndexName != search.IndexName {
		t.Fatalf("outbox index %q decoupled from search.IndexName %q", outboxIndexName, search.IndexName)
	}
	if search.IndexName != agreementProbeIndex {
		t.Fatalf("search.IndexName = %q, want env override %q", search.IndexName, agreementProbeIndex)
	}
}
