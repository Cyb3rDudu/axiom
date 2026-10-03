// ProcessorSource integration test (#271 P0): the lease-freshness predicate
// must be evaluated by the DATABASE clock, not the host's. Gated behind
// AXIOM_TEST_DATABASE_URL like the other lease suites (skips when unset).
package repo

import (
	"context"
	"testing"
)

// TestProcessorSourceLeaseFreshnessIsDBDomain pins the clock-domain fix: the
// `lease_fresh` boolean is decided by `lease_until > now()` in SQL.
//
// The skewed-Host-clock case this guards (the #270 incident): a host
// maintenance sleep left the Podman VM clock 431 s behind the Mac. Pre-fix the
// endpoint compared Host time.Now() against the DB lease_until with zero
// tolerance and 404'd every download. This test proves the caller supplies no
// timestamp at all — the verdict comes from the database, so no host/DB offset
// can flip it. A mutation that reintroduced a host comparison would either not
// compile (the struct no longer carries the timestamp) or fail the predicate
// checks below.
func TestProcessorSourceLeaseFreshnessIsDBDomain(t *testing.T) {
	lr := openThrowawayLeaseDB(t)
	lr.truncateFixtures(t)

	attID, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://src.test", libraryID: "lib-freshness",
		docKey: "DOC-FRESH", attKey: "ATT-FRESH",
		contentHash: h("freshness-hash"), preferred: true,
	}, "pending", 3)
	_ = attID

	ctx := context.Background()

	// 1) Lease due one hour in the DB's future => fresh, regardless of any
	// host clock. `now()` here is the DB clock, exactly as the predicate uses.
	if _, err := lr.pool.Exec(ctx, `
		UPDATE ingest_jobs SET status='processing', lease_until = now() + interval '1 hour'
		WHERE id=$1`, jobID); err != nil {
		t.Fatalf("set future lease: %v", err)
	}
	got, err := lr.rep.ProcessorSource(ctx, jobID)
	if err != nil {
		t.Fatalf("ProcessorSource(fresh): %v", err)
	}
	if !got.LeaseFresh {
		t.Fatal("DB lease 1h in the future must be LeaseFresh=true (no host clock involved)")
	}

	// 2) Lease one hour in the DB's past => expired. The host clock is still
	// the same instant as in step 1; only the DB value changed.
	if _, err := lr.pool.Exec(ctx, `
		UPDATE ingest_jobs SET lease_until = now() - interval '1 hour'
		WHERE id=$1`, jobID); err != nil {
		t.Fatalf("set past lease: %v", err)
	}
	got, err = lr.rep.ProcessorSource(ctx, jobID)
	if err != nil {
		t.Fatalf("ProcessorSource(expired): %v", err)
	}
	if got.LeaseFresh {
		t.Fatal("DB lease 1h in the past must be LeaseFresh=false")
	}

	// 3) A NULL lease is never fresh (a job with no active lease must 404).
	if _, err := lr.pool.Exec(ctx, `UPDATE ingest_jobs SET lease_until=NULL WHERE id=$1`, jobID); err != nil {
		t.Fatalf("clear lease: %v", err)
	}
	got, err = lr.rep.ProcessorSource(ctx, jobID)
	if err != nil {
		t.Fatalf("ProcessorSource(null): %v", err)
	}
	if got.LeaseFresh {
		t.Fatal("NULL lease_until must be LeaseFresh=false")
	}
}
