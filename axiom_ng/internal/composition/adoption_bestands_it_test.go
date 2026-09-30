// adoption_bestands_it_test.go — the F12 #306 DoD witness "Legacy-
// Adoption der Library gegen eine Kopie der Bestands-DB": VerifyAdoption
// over a RESTORED COPY of the real Bestands/dev database must report
// adoptable with a complete ledger (the read-only check; the production
// cutover stays DM09). The restore itself is the operator step (the
// #295 freeze-dump pattern): pg_dump -Fc the Bestands DB, restore into a
// scratch database, point AXIOM_F12_BESTANDS_DSN at it. Live-gated like
// the Dev-Live fingerprint witness — the committed synthetic shapes live
// in adoption_isolation_it_test.go; THIS test runs on the dev host
// against the real copy.
package composition

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/pglib"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLibraryAdoptionAgainstBestandsCopy(t *testing.T) {
	// Strict gate: ONLY the dedicated env (never AXIOM_TEST_DATABASE_URL
	// — CI's go-db-it sets that against a fresh DB without library
	// tables, and this witness must SKIP there, not fail; the restored
	// copy is a dev-host artifact like the freeze dump).
	dsn := os.Getenv("AXIOM_F12_BESTANDS_DSN")
	if dsn == "" {
		t.Skip("AXIOM_F12_BESTANDS_DSN not set; the Bestands-copy witness runs on the dev host")
	}
	if !strings.Contains(strings.Split(dsn, "?")[0], "_test") && !strings.Contains(strings.Split(dsn, "?")[0], "bestands") {
		t.Fatalf("refusing to run against a non-witness database %q (restore the copy into a scratch DB first)", dsn)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open Bestands copy: %v", err)
	}
	t.Cleanup(pool.Close)
	rep, err := pglib.VerifyAdoption(context.Background(), pool)
	if err != nil {
		t.Fatalf("VerifyAdoption over the Bestands copy: %v", err)
	}
	if !rep.Adoptable {
		t.Fatalf("the Bestands copy must be adoptable (ledger complete), got %+v", rep)
	}
	if len(rep.LibraryTables) == 0 {
		t.Fatal("the Bestands copy carries the library namespace — none found (restored the wrong database?)")
	}
	if len(rep.Missing) != 0 {
		t.Fatalf("the Bestands copy's ledger misses %v — silent adoption would mask drift", rep.Missing)
	}
	t.Logf("Bestands copy: %d library tables, ledger %v — %s", len(rep.LibraryTables), rep.LedgerVersions, rep.Reason)
}
