package dispatcher

// #270-Rest integration tests (real DB): claim deferral while a KG
// consolidation/maintenance pass holds the advisory lock — the structural
// coordination chosen for the 2026-09-14 incident class (clock drift itself
// was #271; this gate bounds lane contention on top). The advisory lock is
// the existing mutual-exclusion seam of every KG maintenance pass; reading
// it adds no state and is naturally transient (opens when the pass commits).

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// takeKGLock opens the KG maintenance advisory lock on its own connection
// and returns hold/release. Session-level (not xact) so the test controls
// the window precisely; the detector only reads pg_locks.
func (h *dispatchHarness) takeKGLock(t *testing.T) (release func()) {
	t.Helper()
	// the literal must equal repo.kgMaintenanceLockKey (0x4158494f4d4b4701)
	// — drift fails loudly at the KGMaintenanceActive sanity assert below
	conn, err := h.pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(4708594015363876609)`); err != nil {
		conn.Release()
		t.Fatalf("take kg lock: %v", err)
	}
	return func() {
		defer conn.Release()
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock(4708594015363876609)`); err != nil {
			t.Logf("release kg lock: %v", err)
		}
	}
}

// TestKGGateDefersClaimWhileConsolidationActive — the regression pin: an
// ingest wave SURVIVES an active KG run. While the advisory lock is held,
// the dispatcher defers claiming; the moment it releases, the job is
// claimed and processed normally.
func TestKGGateDefersClaimWhileConsolidationActive(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "KG1", 3)

	// sanity: detector sees the held lock (session-level variant shares the
	// pg_locks shape with the xact lock the consolidation takes)
	release := h.takeKGLock(t)
	if active, err := h.rep.KGMaintenanceActive(context.Background()); err != nil || !active {
		t.Fatalf("KGMaintenanceActive = %v/%v, want true while held", active, err)
	}

	fp := newFakeProcessor(t)
	fp.statuses = []string{"completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`
	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 700*time.Millisecond)

	if got := h.jobStatus(t, jobID); got != "pending" {
		t.Fatalf("status = %q, want pending — the kg gate must defer the claim while a consolidation is active (#270)", got)
	}

	// pass commits → lock releases → the claim proceeds. #298: Run is
	// single-shot (ready/stopped close exactly once), so the post-release
	// claim runs on a FRESH dispatcher — same harness and fake processor,
	// same claim+completion proof.
	release()
	d2 := newDispatcher(t, h, fp, Config{})
	runFor(t, d2, context.Background(), 4*time.Second)
	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed after the KG run finished", got)
	}
	if active, err := h.rep.KGMaintenanceActive(context.Background()); err != nil || active {
		t.Fatalf("KGMaintenanceActive = %v/%v, want false after release", active, err)
	}
}

// TestKGMaintenanceActiveScopedToOwnDatabase — the review mutation pin for
// the database filter: a KG pass holding the advisory lock in a DIFFERENT
// database (a scratch/test DB on the same instance) must NOT read as
// active here. Removing the `database = current_database()` filter turns
// this red (the cross-db lock would freeze production claiming — the
// original finding, and the source of the wave-IT flakes).
func TestKGMaintenanceActiveScopedToOwnDatabase(t *testing.T) {
	h := openDispatchDB(t)

	// a SECOND database on the same instance
	other := "axiom_ng_dispatch_kgscope_test"
	maint, err := pgxpool.New(context.Background(), cloneDSN(mustParseDSN(t), "postgres"))
	if err != nil {
		t.Fatalf("maint pool: %v", err)
	}
	var exists bool
	if err := maint.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, other).Scan(&exists); err != nil {
		t.Fatalf("check db: %v", err)
	}
	if !exists {
		if _, err := maint.Exec(context.Background(), `CREATE DATABASE `+other); err != nil {
			t.Fatalf("create db: %v", err)
		}
	}
	maint.Close()
	otherPool, err := pgxpool.New(context.Background(), cloneDSN(mustParseDSN(t), other))
	if err != nil {
		t.Fatalf("other pool: %v", err)
	}
	defer otherPool.Close()

	// hold the KG advisory lock IN THE OTHER DATABASE
	conn, err := otherPool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(4708594015363876609)`); err != nil {
		t.Fatalf("lock in other db: %v", err)
	}

	// the detector on OUR database must NOT see it
	if active, err := h.rep.KGMaintenanceActive(context.Background()); err != nil {
		t.Fatalf("detector: %v", err)
	} else if active {
		t.Fatal("KG lock held in ANOTHER database must not read as active — the detector lost its database scope")
	}

	// sanity: the same lock in OUR database still reads as active
	release := h.takeKGLock(t)
	defer release()
	if active, err := h.rep.KGMaintenanceActive(context.Background()); err != nil || !active {
		t.Fatalf("own-db lock must read as active: %v/%v", active, err)
	}
}

// mustParseDSN re-parses the harness DSN for cloneDSN use.
func mustParseDSN(t *testing.T) *url.URL {
	t.Helper()
	base := os.Getenv("AXIOM_TEST_DATABASE_URL")
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return u
}
