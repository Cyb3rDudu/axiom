// Lease protocol integration tests (Gate 1, sections 14.1 + user corrections).
// Run only against a DEDICATED test database: AXIOMNG_TEST_DATABASE_URL must
// point at a database whose name ends in "_test", and every test asserts that
// before any TRUNCATE, so the application database can never be truncated.
// Tests skip (not fail) when no DSN is configured.
package repo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func leaseTestDSN() string { return os.Getenv("AXIOMNG_TEST_DATABASE_URL") }

// requireTestDB returns the database name from the DSN and fails unless it
// clearly denotes a dedicated test database (suffix "_test"). This is the hard
// guard enforced before any destructive statement.
func requireTestDB(t *testing.T, dsn string) string {
	t.Helper()
	name := dsnDatabaseName(dsn)
	if !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to run against non-test database %q (must end in _test)", name)
	}
	return name
}

func dsnDatabaseName(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
			if k := strings.Index(rest, "?"); k >= 0 {
				rest = rest[:k]
			}
			return rest
		}
	}
	return ""
}

// repoTestDatabaseName is a package-private database so this package's
// integration tests cannot collide with the concurrent sync package, which
// shares the user-provided *_test database. `go test ./...` still works because
// each package owns its own dataset.
const repoTestDatabaseName = "axiom_ng_repo_test"

// swapDatabase rebuilds a postgres DSN pointing at a different database name,
// preserving parameters.
func swapDatabase(dsn, db string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + db
	return u.String()
}

// ensureRepoDatabase creates the package-private test database if needed, using
// the user-provided *_test DSN as the source of host/credentials and the
// maintenance "postgres" database as the create target. It is a no-op if the
// database already exists. Returns the effective repo DSN.
func ensureRepoDatabase(t *testing.T) string {
	t.Helper()
	base := leaseTestDSN()
	if base == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping lease integration test")
	}
	requireTestDB(t, base)

	repoDSN := swapDatabase(base, repoTestDatabaseName)
	maintainDSN := swapDatabase(base, "postgres")

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, maintainDSN)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, repoTestDatabaseName).Scan(&exists); err != nil {
		t.Fatalf("check db exists: %v", err)
	}
	if !exists {
		if _, err := pool.Exec(ctx, `CREATE DATABASE `+repoTestDatabaseName); err != nil {
			t.Fatalf("create repo test db: %v", err)
		}
	}
	return repoDSN
}

// leaseRepo wraps a Repo plus its pool so tests can drive both the repository
// API and raw connection-backed assertions/concurrency.
type leaseRepo struct {
	pool *pgxpool.Pool
	rep  *Repo
	dsn  string
}

func openLeaseDB(t *testing.T) *leaseRepo {
	t.Helper()
	repoDSN := ensureRepoDatabase(t)
	ctx := context.Background()
	d, err := db.Open(ctx, repoDSN)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(d.Close)
	return &leaseRepo{pool: d.Pool(), rep: New(d.Pool()), dsn: repoDSN}
}

// truncateFixtures clears the tables under test. Unlike relying on the harness
// open-time guard, it verifies the ACTUAL database of the TRUNCATE session
// (current_database) ends in "_test" immediately before issuing the TRUNCATE, so
// a mis-wired pool can never wipe a non-test database even if the harness guard
// was bypassed.
func (lr *leaseRepo) truncateFixtures(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var dbName string
	if err := lr.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("read current_database: %v", err)
	}
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("REFUSING to truncate: current_database %q does not end in _test", dbName)
	}
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE ingest_jobs, zotero_attachments, zotero_documents, zotero_items,
		         zotero_item_collections, zotero_collections, zotero_sources`); err != nil {
		t.Fatalf("truncate fixtures: %v", err)
	}
}

type seedSpec struct {
	sourceBaseURL string
	libraryID     string
	docKey        string
	attKey        string
	contentHash   *string
	preferred     bool
	deleted       bool
}

// seed inserts a source, document and one attachment per spec and returns the
// fixture ids. Keys are caller-provided so multiple fixtures coexist without
// violating the UNIQUE source/dataset constraints.
func (lr *leaseRepo) seed(t *testing.T, spec seedSpec, jobStatus string, maxAttempts int) (attachmentID, jobID string) {
	t.Helper()
	ctx := context.Background()
	var srcID, docID string
	err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ($1, $2, 'test-server') RETURNING id::text`, spec.sourceBaseURL, spec.libraryID).Scan(&srcID)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	err = lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, $2, 1, 'book', 'Test Doc') RETURNING id::text`, srcID, spec.docKey).Scan(&docID)
	if err != nil {
		t.Fatalf("insert document: %v", err)
	}
	err = lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path,
			content_hash, preferred, deleted)
		VALUES ($1, $2, $3, 1, $4, 'imported_file', 'application/pdf',
			'x.pdf', '/tmp/x.pdf', $5, $6, $7) RETURNING id::text`,
		srcID, docID, spec.attKey, spec.docKey, spec.contentHash, spec.preferred, spec.deleted).Scan(&attachmentID)
	if err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	err = lr.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
		srcID, docID, attachmentID, spec.contentHash, jobStatus, maxAttempts).Scan(&jobID)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return attachmentID, jobID
}

func h(s string) *string { return &s }

// defaultClaim returns a claim with a fixed profile so freeze assertions are
// deterministic. profile_hash and idempotency_key are COMPUTED by the repo, not
// supplied.
func defaultClaim(worker string) ClaimOptions {
	return ClaimOptions{
		WorkerID:      worker,
		LeaseDuration: 120 * time.Second,
		Profile:       []byte(`{"profile":"full-rag-v1"}`),
	}
}

// row is the full ingest_jobs row for assertions (owner, token, lease, timestamps,
// terminal code, completed_at, frozen fields).
type row struct {
	status           string
	attempt          int
	maxAttempts      int
	claimedBy        *string
	leaseToken       *string
	leaseUntil       *time.Time
	lastHeartbeat    *time.Time
	cancelRequested  *time.Time
	nextAttemptAt    *time.Time
	completedAt      *time.Time
	errorCode        *string
	errorMessage     *string
	idempotencyKey   *string
	profileHash      *string
	processorName    *string
	processorVersion *string
	snapshotID       *string
	hasInputSnapshot bool
	hasProfile       bool
}

func (lr *leaseRepo) rowOf(t *testing.T, jobID string) row {
	t.Helper()
	var r row
	err := lr.pool.QueryRow(context.Background(), `
		SELECT status::text, attempt, max_attempts, claimed_by, lease_token::text,
		       lease_until, last_heartbeat_at, cancel_requested_at, next_attempt_at,
		       completed_at, error_code, error_message, idempotency_key, profile_hash,
		       processor_name, processor_version, result->>'snapshot_id',
		       input_snapshot IS NOT NULL, processing_profile IS NOT NULL
		FROM ingest_jobs WHERE id=$1`, jobID).Scan(
		&r.status, &r.attempt, &r.maxAttempts, &r.claimedBy, &r.leaseToken,
		&r.leaseUntil, &r.lastHeartbeat, &r.cancelRequested, &r.nextAttemptAt,
		&r.completedAt, &r.errorCode, &r.errorMessage, &r.idempotencyKey, &r.profileHash,
		&r.processorName, &r.processorVersion, &r.snapshotID,
		&r.hasInputSnapshot, &r.hasProfile)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	return r
}

func (lr *leaseRepo) expire(t *testing.T, jobID string) {
	t.Helper()
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE ingest_jobs SET lease_until=now() - interval '1 second' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
}

// claimOpt returns the ClaimNextJob result or fails.
func (lr *leaseRepo) claim(t *testing.T, opts ClaimOptions) *ClaimedJob {
	t.Helper()
	cj, err := lr.rep.ClaimNextJob(context.Background(), opts)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return cj
}

func TestClaimSameJobNotDoubleGrantedConcurrent(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:1", libraryID: "users/0",
		docKey: "A1", attKey: "A1", contentHash: h("sha256:cc1"), preferred: true,
	}, "pending", 3)

	const workers = 2
	start := make(chan struct{})
	type result struct {
		cj  *ClaimedJob
		err error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each claimer uses an independent, dedicated pool into the SAME
			// package-private repo DB so the two are guaranteed separate DB
			// connections.
			ds, err := db.Open(context.Background(), lr.dsn)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer ds.Close()
			rep := New(ds.Pool())
			opts := defaultClaim(fmt.Sprintf("worker-%d", i))
			<-start
			cj, err := rep.ClaimNextJob(context.Background(), opts)
			results <- result{cj: cj, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	for res := range results {
		if res.err != nil {
			t.Errorf("claimer error: %v", res.err)
		}
		if res.cj != nil {
			winners++
			if winners == 1 && res.cj.JobID != jobID {
				t.Errorf("winner claimed %s, want %s", res.cj.JobID, jobID)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one claimer must win, got %d winners", winners)
	}
	r := lr.rowOf(t, jobID)
	if r.status != "claimed" {
		t.Fatalf("status after concurrent claim = %s", r.status)
	}
	if r.leaseToken == nil || *r.leaseToken == "" {
		t.Fatalf("claimed job has no lease token")
	}
	if r.attempt != 1 {
		t.Fatalf("attempt after concurrent claim = %d, want 1 (incremented exactly once)", r.attempt)
	}
	if r.claimedBy == nil || !strings.HasPrefix(*r.claimedBy, "worker-") {
		t.Fatalf("claimed job has no owner: %v", r.claimedBy)
	}
	if r.leaseUntil == nil {
		t.Fatal("claimed job has no lease_until")
	}
}

func TestHeldRowLockProvesSkipLocked(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, j1 := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:2", libraryID: "users/0",
		docKey: "B1", attKey: "B1", contentHash: h("sha256:cc2"), preferred: true,
	}, "pending", 3)
	_, j2 := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:3", libraryID: "users/1",
		docKey: "B2", attKey: "B2", contentHash: h("sha256:cc3"), preferred: true,
	}, "pending", 3)

	// Hold a lock on j1's row in an independent transaction, then ClaimNextJob
	// must SKIP j1 and claim j2 (real SKIP LOCKED behavior).
	ctx := context.Background()
	holdConn, err := lr.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holdConn.Release()
	holdTx, err := holdConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Lock j1 row via FOR UPDATE.
	if _, err := holdTx.Exec(ctx, `SELECT id FROM ingest_jobs WHERE id=$1 FOR UPDATE`, j1); err != nil {
		t.Fatal(err)
	}

	cj := lr.claim(t, defaultClaim("worker-a"))
	if cj == nil {
		t.Fatal("no job claimed while j1 row locked")
	}
	if cj.JobID != j2 {
		t.Fatalf("claimed %s, want the unlocked job %s (j1 was row-locked)", cj.JobID, j2)
	}

	// Rollback the held lock; j1 becomes claimable again.
	if err := holdTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	cj2 := lr.claim(t, defaultClaim("worker-b"))
	if cj2 == nil || cj2.JobID != j1 {
		t.Fatalf("after releasing lock, expected to claim %s, got %v", j1, cj2)
	}
}

func TestObsoleteTransitionCommitted(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:4", libraryID: "users/0",
		docKey: "C1", attKey: "C1", contentHash: h("sha256:a"), preferred: false,
	}, "pending", 3)
	_ = jobID
	// Unpreferred attachment makes the job obsolete.
	cj := lr.claim(t, defaultClaim("worker-a"))
	if cj != nil {
		t.Fatalf("obsolete job was claimed: %v", cj)
	}
	// The skipped transition must PERSIST after the claim pass committed.
	r := lr.rowOf(t, jobID)
	if r.status != "skipped" {
		t.Fatalf("obsolete job status = %s, want skipped (transition must be committed)", r.status)
	}
	if r.errorCode == nil || *r.errorCode != "SKIPPED" {
		t.Fatalf("skipped reason code mismatch: %v", r.errorCode)
	}
	// It must not remain claimable in a later pass.
	cj2 := lr.claim(t, defaultClaim("worker-b"))
	if cj2 != nil {
		t.Fatalf("committed skipped job claimed again: %v", cj2)
	}
	if lr.rowOf(t, jobID).status != "skipped" {
		t.Fatal("skipped status regressed")
	}
}

func TestNullHashAndForcedRebuildMismatch(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)

	// (a) Attachment hash NULL -> job claimable (no hash baseline).
	_, jNull := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:5", libraryID: "users/0",
		docKey: "D1", attKey: "D1", contentHash: nil, preferred: true,
	}, "pending", 3)
	if cj := lr.claim(t, defaultClaim("worker-a")); cj == nil {
		t.Fatal("NULL current hash should claim")
	} else if cj.JobID != jNull {
		t.Fatalf("expected claim of NULL-hash job %s, got %s", jNull, cj.JobID)
	}

	// (b) Job content_hash set but attachment hash differs -> mismatch -> skipped.
	_, jMismatch := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:6", libraryID: "users/1",
		docKey: "D2", attKey: "D2", contentHash: h("sha256:jobhash"), preferred: true,
	}, "pending", 3)
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE zotero_attachments SET content_hash='sha256:different' WHERE zotero_key='D2'`); err != nil {
		t.Fatal(err)
	}
	if cj := lr.claim(t, defaultClaim("worker-b")); cj != nil {
		t.Fatalf("hash-mismatch job was claimed: %v", cj)
	}
	if lr.rowOf(t, jMismatch).status != "skipped" {
		t.Fatalf("hash-mismatch job should be skipped, got %s", lr.rowOf(t, jMismatch).status)
	}

	// (c) Forced rebuild ignores the hash mismatch.
	_, jForce := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:7", libraryID: "users/2",
		docKey: "D3", attKey: "D3", contentHash: h("sha256:old"), preferred: true,
	}, "pending", 3)
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE ingest_jobs SET force_rebuild=true WHERE id=$1`, jForce); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE zotero_attachments SET content_hash='sha256:new' WHERE zotero_key='D3'`); err != nil {
		t.Fatal(err)
	}
	cj := lr.claim(t, defaultClaim("worker-c"))
	if cj == nil || cj.JobID != jForce {
		t.Fatalf("forced-rebuild hash-mismatch job should claim %s, got %v", jForce, cj)
	}
	if !cj.ForceRebuild {
		t.Fatal("claimed forced-rebuild job did not carry ForceRebuild")
	}

	// (d) NULL job hash with real attachment hash -> mismatch -> skipped.
	_, jNullJobHash := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:8", libraryID: "users/3",
		docKey: "D4", attKey: "D4", contentHash: nil, preferred: true,
	}, "pending", 3)
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE zotero_attachments SET content_hash='sha256:attach' WHERE zotero_key='D4'`); err != nil {
		t.Fatal(err)
	}
	if cj := lr.claim(t, defaultClaim("worker-d")); cj != nil {
		t.Fatalf("NULL-job-hash with real attachment hash should skip, got %v", cj)
	}
	if lr.rowOf(t, jNullJobHash).status != "skipped" {
		t.Fatalf("NULL-job-hash job should be skipped, got %s", lr.rowOf(t, jNullJobHash).status)
	}
}

func TestExpiredJobReclaimedAndExhausted(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)

	// Reclaim below max attempts.
	_, j1 := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:9", libraryID: "users/0",
		docKey: "E1", attKey: "E1", contentHash: h("sha256:1"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if cj == nil || cj.JobID != j1 {
		t.Fatalf("expected first claim of %s, got %v", j1, cj)
	}
	lr.expire(t, j1)
	cj2 := lr.claim(t, defaultClaim("worker-b"))
	if cj2 == nil || cj2.JobID != j1 {
		t.Fatalf("expired job not reclaimed")
	}
	if cj2.Attempt != cj.Attempt+1 {
		t.Fatalf("reclaim attempt = %d, want %d", cj2.Attempt, cj.Attempt+1)
	}
	// Frozen snapshot preserved across reclaim (not regenerated).
	if cj.InputSnapshot == nil || len(cj.InputSnapshot) == 0 {
		t.Fatal("first claim had no frozen input snapshot")
	}
	if string(cj.InputSnapshot) != string(cj2.InputSnapshot) {
		t.Fatal("input snapshot changed across reclaim")
	}

	// Exhaustion: force attempt to max then expire -> terminal LEASE_EXHAUSTED.
	_, j2 := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:10", libraryID: "users/1",
		docKey: "E2", attKey: "E2", contentHash: h("sha256:2"), preferred: true,
	}, "pending", 3)
	// Claim it and bump the persisted attempt to max_attempts (3), then expire.
	if cjx := lr.claim(t, defaultClaim("worker-c")); cjx == nil || cjx.JobID != j2 {
		t.Fatalf("expected claim %s, got %v", j2, cjx)
	}
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE ingest_jobs SET attempt=3, lease_until=now() - interval '1 second' WHERE id=$1`, j2); err != nil {
		t.Fatal(err)
	}
	if cj3 := lr.claim(t, defaultClaim("worker-d")); cj3 != nil {
		t.Fatalf("exhausted job must not be claimed")
	}
	r2 := lr.rowOf(t, j2)
	if r2.status != "failed" {
		t.Fatalf("exhausted job status = %s, want failed", r2.status)
	}
	if r2.errorCode == nil || *r2.errorCode != "LEASE_EXHAUSTED" {
		t.Fatalf("error_code = %v, want LEASE_EXHAUSTED", r2.errorCode)
	}
	if r2.leaseToken != nil || r2.claimedBy != nil || r2.leaseUntil != nil {
		t.Fatal("exhausted job must have lease fields cleared")
	}
	if r2.completedAt == nil {
		t.Fatal("exhausted job must have completed_at set")
	}
}

func TestFutureNextAttemptNotClaimed(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:11", libraryID: "users/0",
		docKey: "F1", attKey: "F1", contentHash: h("sha256:f"), preferred: true,
	}, "pending", 3)
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE ingest_jobs SET next_attempt_at=now() + interval '1 hour' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if cj := lr.claim(t, defaultClaim("worker-a")); cj != nil {
		t.Fatalf("future next_attempt_at job was claimed: %v", cj)
	}
}

func TestCancellationConvergesTerminal(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)

	// (a) Pending + cancel request -> terminal cancelled immediately by claim pass.
	_, jPend := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:12", libraryID: "users/0",
		docKey: "G1", attKey: "G1", contentHash: h("sha256:g"), preferred: true,
	}, "pending", 3)
	if err := lr.rep.RequestCancellation(context.Background(), jPend); err != nil {
		t.Fatal(err)
	}
	if cj := lr.claim(t, defaultClaim("worker-a")); cj != nil {
		t.Fatalf("cancel-requested pending job was claimed")
	}
	if r := lr.rowOf(t, jPend); r.status != "cancelled" {
		t.Fatalf("pending cancel-requested job status = %s, want cancelled", r.status)
	}

	// (b) In-flight claimed/processing + cancel request + lease expired -> cancelled.
	_, jInflight := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:13", libraryID: "users/1",
		docKey: "G2", attKey: "G2", contentHash: h("sha256:h"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-b"))
	if cj == nil || cj.JobID != jInflight {
		t.Fatalf("expected claim %s, got %v", jInflight, cj)
	}
	if err := lr.rep.RequestCancellation(context.Background(), jInflight); err != nil {
		t.Fatal(err)
	}
	// Renewing the lease keeps it out of terminalization.
	if err := lr.rep.RenewLease(context.Background(), cj.LeaseRef, 120*time.Second); err != nil {
		t.Fatal(err)
	}
	lr.expire(t, jInflight)
	if cj2 := lr.claim(t, defaultClaim("worker-c")); cj2 != nil {
		t.Fatalf("expired cancel-requested job claimed")
	}
	if r := lr.rowOf(t, jInflight); r.status != "cancelled" {
		t.Fatalf("expired in-flight cancel-requested status = %s, want cancelled", r.status)
	}
}

func TestPendingCancellationImmediateTerminal(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:14", libraryID: "users/0",
		docKey: "H1", attKey: "H1", contentHash: h("sha256:i"), preferred: true,
	}, "pending", 3)
	if err := lr.rep.RequestCancellation(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	lr.claim(t, defaultClaim("worker-a"))
	r := lr.rowOf(t, jobID)
	if r.status != "cancelled" {
		t.Fatalf("pending cancellation not terminal: %s", r.status)
	}
	if r.completedAt == nil {
		t.Fatal("cancelled job must have completed_at")
	}
}

func TestRecoverDoesNotClobberRenewed(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:15", libraryID: "users/0",
		docKey: "I1", attKey: "I1", contentHash: h("sha256:j"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	// Renew the lease to a far future; expiry no longer applies. Recovery must
	// not touch the job.
	if err := lr.rep.RenewLease(context.Background(), cj.LeaseRef, 3600*time.Second); err != nil {
		t.Fatal(err)
	}
	n, err := lr.rep.RecoverExpiredJobs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recovery touched a renewed job: recovered=%d", n)
	}
	if r := lr.rowOf(t, jobID); r.status != "claimed" {
		t.Fatalf("renewed job status changed to %s", r.status)
	}
}

func TestRecoverClassifiesExpiredExhaustedCancelled(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)

	// Seed three jobs and claim them in FIFO order (no expiry yet, so the
	// sequential claims each get the next unfinished job).
	_, jRec := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:16", libraryID: "users/0",
		docKey: "J1", attKey: "J1", contentHash: h("sha256:k"), preferred: true,
	}, "pending", 3)
	_, jExh := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:17", libraryID: "users/1",
		docKey: "J2", attKey: "J2", contentHash: h("sha256:l"), preferred: true,
	}, "pending", 3)
	_, jCan := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:18", libraryID: "users/2",
		docKey: "J3", attKey: "J3", contentHash: h("sha256:m"), preferred: true,
	}, "pending", 3)

	// worker-a claims jRec (FIFO head), worker-b jExh, worker-c jCan — all while
	// every lease is still valid, so each claim gets the next unfinished job.
	if cj := lr.claim(t, defaultClaim("worker-a")); cj == nil || cj.JobID != jRec {
		t.Fatalf("expected claim %s, got %v", jRec, cj)
	}
	if cj := lr.claim(t, defaultClaim("worker-b")); cj == nil || cj.JobID != jExh {
		t.Fatalf("expected claim %s, got %v", jExh, cj)
	}
	if cj := lr.claim(t, defaultClaim("worker-c")); cj == nil || cj.JobID != jCan {
		t.Fatalf("expected claim %s, got %v", jCan, cj)
	}

	// jRec: worker-a fails -> still reclaimable (attempt<max).
	lr.expire(t, jRec)

	// jExh: worker-b fails to exhaustion (attempt becomes max).
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE ingest_jobs SET attempt=max_attempts, lease_until=now() - interval '1 second' WHERE id=$1`, jExh); err != nil {
		t.Fatal(err)
	}

	// jCan: worker-c gets cancelled in-flight before its lease expires.
	if err := lr.rep.RequestCancellation(context.Background(), jCan); err != nil {
		t.Fatal(err)
	}
	lr.expire(t, jCan)

	nRec, err := lr.rep.RecoverExpiredJobs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if nRec != 1 {
		t.Fatalf("recovered=%d, want 1", nRec)
	}
	if r := lr.rowOf(t, jRec); r.status != "pending" {
		t.Fatalf("recoverable job status = %s, want pending", r.status)
	}
	if r := lr.rowOf(t, jExh); r.status != "failed" || r.errorCode == nil || *r.errorCode != "LEASE_EXHAUSTED" {
		t.Fatalf("exhausted recovery did not terminalize: %+v", r)
	}
	if r := lr.rowOf(t, jCan); r.status != "cancelled" {
		t.Fatalf("cancelled recovery did not terminalize: %s", r.status)
	}
}

func TestStaleTokenFenceRejected(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:19", libraryID: "users/0",
		docKey: "K1", attKey: "K1", contentHash: h("sha256:n"), preferred: true,
	}, "pending", 3)
	if cj := lr.claim(t, defaultClaim("worker-a")); cj == nil {
		t.Fatal("initial claim failed")
	}
	// A second claim of the same UNEXPIRED job must fail (not double-grant).
	if again := lr.claim(t, defaultClaim("worker-b")); again != nil {
		t.Fatalf("unexpired job double-claimed: %v", again)
	}
	// Stale worker-a token (still valid lease) attempts renew with a wrong token.
	stale := LeaseRef{JobID: jobID, WorkerID: "worker-a", LeaseToken: "00000000-0000-0000-0000-000000000000"}
	err := lr.rep.RenewLease(context.Background(), stale, 120*time.Second)
	if !errors.Is(err, ErrLostLease) {
		t.Fatalf("stale token renew = %v, want ErrLostLease", err)
	}
}

func TestInputFrozenAtClaimAndImmutableAcrossRetries(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:20", libraryID: "users/0",
		docKey: "L1", attKey: "L1", contentHash: h("sha256:o"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if !lr.rowOf(t, jobID).hasInputSnapshot {
		t.Fatal("input snapshot was not frozen at claim")
	}
	if cj.InputSnapshot == nil || len(cj.InputSnapshot) == 0 {
		t.Fatal("claimed job returned no input snapshot")
	}

	// The profile hash and idempotency key are COMPUTED deterministically in Go,
	// never taken from the caller.
	wantHash, err := canonicalProfile([]byte(`{"profile":"full-rag-v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cj.ProfileHash == nil || *cj.ProfileHash != wantHash {
		t.Fatalf("profile hash = %v, want computed %s", cj.ProfileHash, wantHash)
	}
	// The frozen key must exactly equal the repo-computed key for this identity.
	wantKey := idempotencyKey(jobID, cj.AttachmentID, h("sha256:o"), wantHash, false)
	if cj.IdempotencyKey == nil || *cj.IdempotencyKey != wantKey {
		t.Fatalf("idempotency key = %v, want computed %s", cj.IdempotencyKey, wantKey)
	}
	if !strings.Contains(*cj.IdempotencyKey, ":sha256:o:") {
		t.Fatalf("idempotency key lacks frozen content hash: %v", cj.IdempotencyKey)
	}

	// Retry: expire and reclaim; stored frozen values must win (immutable).
	lr.expire(t, jobID)
	changed := defaultClaim("worker-b")
	changed.Profile = []byte(`{"profile":"other"}`)
	cj2 := lr.claim(t, changed)
	if cj2 == nil || cj2.JobID != jobID {
		t.Fatalf("expected reclaim %s, got %v", jobID, cj2)
	}
	if string(cj2.InputSnapshot) != string(cj.InputSnapshot) {
		t.Fatal("input snapshot overwritten on reclaim")
	}
	if cj2.IdempotencyKey == nil || *cj2.IdempotencyKey != *cj.IdempotencyKey {
		t.Fatal("stored idempotency key overwritten on reclaim")
	}
	if cj2.ProfileHash == nil || *cj2.ProfileHash != wantHash {
		t.Fatal("stored profile hash overwritten on reclaim")
	}
}

func TestScheduleRetryBounded(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:21", libraryID: "users/0",
		docKey: "M1", attKey: "M1", contentHash: h("sha256:p"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	// Huge negative delay must clamp to 0 (immediately claimable) and, below the
	// attempt ceiling, must be a scheduled retry (not exhaustion).
	outcome, err := lr.rep.ScheduleRetry(context.Background(), cj.LeaseRef, "IO_ERROR", "boom", -5000)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != RetryScheduled {
		t.Fatalf("outcome = %v, want RetryScheduled", outcome)
	}
	r := lr.rowOf(t, jobID)
	if r.status != "pending" {
		t.Fatalf("scheduled retry status = %s, want pending", r.status)
	}
	if r.nextAttemptAt == nil || r.nextAttemptAt.After(time.Now().Add(2*time.Second)) {
		t.Fatalf("clamped retry not immediately due: %v", r.nextAttemptAt)
	}
}

func TestMarkCompletedTxAtomic(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:22", libraryID: "users/0",
		docKey: "N1", attKey: "N1", contentHash: h("sha256:q"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if err := lr.rep.MarkProcessing(context.Background(), cj.LeaseRef); err != nil {
		t.Fatal(err)
	}

	// Commit then verify completed.
	ctx := context.Background()
	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lr.rep.MarkCompletedTx(ctx, tx, cj.LeaseRef, "processor", "1.0", "snapshot-1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rc := lr.rowOf(t, jobID)
	if rc.status != "completed" {
		t.Fatalf("job status = %s, want completed", rc.status)
	}
	if rc.processorName == nil || *rc.processorName != "processor" {
		t.Fatalf("committed completion processor_name = %v", rc.processorName)
	}
	if rc.processorVersion == nil || *rc.processorVersion != "1.0" {
		t.Fatalf("committed completion processor_version = %v", rc.processorVersion)
	}
	if rc.snapshotID == nil || *rc.snapshotID != "snapshot-1" {
		t.Fatalf("committed completion snapshot_id = %v", rc.snapshotID)
	}
	if rc.leaseToken != nil || rc.claimedBy != nil || rc.leaseUntil != nil {
		t.Fatal("committed completion must have cleared the lease")
	}
	if rc.completedAt == nil {
		t.Fatal("committed completion must set completed_at")
	}

	// Rollback path: complete a second job then roll the tx back; it must NOT
	// persist the completed state.
	_, jobID2 := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:23", libraryID: "users/1",
		docKey: "N2", attKey: "N2", contentHash: h("sha256:r"), preferred: true,
	}, "pending", 3)
	cj2 := lr.claim(t, defaultClaim("worker-b"))
	if err := lr.rep.MarkProcessing(context.Background(), cj2.LeaseRef); err != nil {
		t.Fatal(err)
	}
	tx2, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lr.rep.MarkCompletedTx(ctx, tx2, cj2.LeaseRef, "processor", "1.0", "snapshot-2"); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// The exact pre-transaction `processing` row must remain: not completed, owner+
	// token still held, no completed_at, no processor/snapshot recorded.
	r := lr.rowOf(t, jobID2)
	if r.status != "processing" {
		t.Fatalf("rolled-back completion left status = %s, want processing", r.status)
	}
	if r.leaseToken == nil || r.claimedBy == nil || r.leaseUntil == nil {
		t.Fatal("rolled-back completion lost the worker's lease ownership")
	}
	if r.completedAt != nil {
		t.Fatal("rolled-back completion set completed_at")
	}
	// Processor/snapshot provenance must not be recorded either.
	if r.processorName != nil || r.processorVersion != nil || r.snapshotID != nil {
		t.Fatalf("rolled-back completion persisted processor/result state: name=%v ver=%v snap=%v",
			r.processorName, r.processorVersion, r.snapshotID)
	}
}

func TestCompletionRejectsChangedAttachment(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:24", libraryID: "users/0",
		docKey: "O1", attKey: "O1", contentHash: h("sha256:s"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if err := lr.rep.MarkProcessing(context.Background(), cj.LeaseRef); err != nil {
		t.Fatal(err)
	}
	// Attachment hash changes while processing -> completion must be rejected.
	if _, err := lr.pool.Exec(context.Background(),
		`UPDATE zotero_attachments SET content_hash='sha256:changed' WHERE zotero_key='O1'`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = lr.rep.MarkCompletedTx(ctx, tx, cj.LeaseRef, "p", "1", "snap")
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrLostLease) {
		t.Fatalf("completion with changed attachment = %v, want ErrLostLease", err)
	}
	// The job must remain processing with its lease intact (nothing committed).
	r := lr.rowOf(t, jobID)
	if r.status != "processing" {
		t.Fatalf("job status after rejected completion = %s, want processing", r.status)
	}
	if r.leaseToken == nil || r.leaseUntil == nil || r.completedAt != nil {
		t.Fatal("rejected completion must leave the processing job untouched")
	}
}

func TestReleaseOrExpireLease(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:25", libraryID: "users/0",
		docKey: "P1", attKey: "P1", contentHash: h("sha256:t"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if err := lr.rep.ReleaseOrExpireLease(context.Background(), cj.LeaseRef); err != nil {
		t.Fatal(err)
	}
	r := lr.rowOf(t, jobID)
	if r.status != "pending" {
		t.Fatalf("released status = %s, want pending", r.status)
	}
	if r.leaseToken != nil || r.claimedBy != nil || r.leaseUntil != nil {
		t.Fatal("released job must have lease fields cleared")
	}
}

func TestFreshSchemaMigrationLeaseColumns(t *testing.T) {
	lr := openLeaseDB(t)
	ctx := context.Background()
	var n int
	err := lr.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name='ingest_jobs' AND column_name IN
		 ('lease_token','lease_until','last_heartbeat_at','cancel_requested_at',
		  'input_snapshot','processing_profile','profile_hash','idempotency_key',
		  'next_attempt_at')`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if want := 9; n != want {
		t.Fatalf("lease migration columns present = %d, want %d", n, want)
	}
}

func TestUpgrade0005To0006Additive(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	// Simulate a DB at schema version 0005: drop the 0006 lease columns and
	// indexes, remove the 0006 migration record, then insert a pre-existing
	// ingest row. Re-migrating must ADD the columns additively WITHOUT touching
	// the existing row.
	if _, err := lr.pool.Exec(ctx, `
		ALTER TABLE ingest_jobs
		  DROP COLUMN IF EXISTS lease_token,
		  DROP COLUMN IF EXISTS next_attempt_at,
		  DROP COLUMN IF EXISTS last_heartbeat_at,
		  DROP COLUMN IF EXISTS cancel_requested_at,
		  DROP COLUMN IF EXISTS input_snapshot,
		  DROP COLUMN IF EXISTS processing_profile,
		  DROP COLUMN IF EXISTS profile_hash,
		  DROP COLUMN IF EXISTS idempotency_key;
		DROP INDEX IF EXISTS ingest_jobs_claim_pending_idx;
		DROP INDEX IF EXISTS ingest_jobs_claim_expired_idx;
		DELETE FROM schema_migrations WHERE version LIKE '%0006%';
	`); err != nil {
		t.Fatal(err)
	}

	// A pre-existing row carries the OLD schema's columns (no lease columns yet).
	var srcID, docID, attID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id) VALUES ('http://upgrade', 'users/0')
		RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1,'OLD',1,'book','Old') RETURNING id::text`, srcID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode, content_type, filename, preferred)
		VALUES ($1,$2,'OLD',1,'OLD','imported_file','application/pdf','old.pdf',true) RETURNING id::text`, srcID, docID).Scan(&attID); err != nil {
		t.Fatal(err)
	}
	// content_hash must be NULL to satisfy the partial unique index (0002) which
	// covers (attachment_id, content_hash) WHERE force_rebuild=false.
	var oldJobID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, status)
		VALUES ($1,$2,$3,'pending') RETURNING id::text`, srcID, docID, attID).Scan(&oldJobID); err != nil {
		t.Fatal(err)
	}

	// Re-apply migrations (0006 reapplies additively) against the same repo DB.
	d, err := db.Open(ctx, lr.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// The existing row must survive with its (now defaulted) lease columns intact.
	var st string
	if err := lr.pool.QueryRow(ctx, `SELECT status::text FROM ingest_jobs WHERE id=$1`, oldJobID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "pending" {
		t.Fatalf("pre-existing row status changed during upgrade: %s", st)
	}
	// Lease columns are present.
	var hasToken bool
	if err := lr.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name='ingest_jobs' AND column_name='lease_token')`).Scan(&hasToken); err != nil {
		t.Fatal(err)
	}
	if !hasToken {
		t.Fatal("lease_token column missing after 0006 upgrade")
	}
	// The old row is claimable through the new path.
	cj, err := lr.rep.ClaimNextJob(ctx, defaultClaim("worker-upgrade"))
	if err != nil {
		t.Fatal(err)
	}
	if cj == nil || cj.JobID != oldJobID {
		t.Fatalf("upgraded row not claimable: %v", cj)
	}
}

// seedPendingOnly inserts a pending ingest_jobs row with the given (possibly
// NULL) FK values, used to test legacy rows whose parents are absent.
func (lr *leaseRepo) seedPendingOnly(t *testing.T, sourceID, documentID, attachmentID *string, maxAttempts int) string {
	t.Helper()
	ctx := context.Background()
	var jobID string
	err := lr.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, status, max_attempts)
		VALUES ($1, $2, $3, 'pending', $4) RETURNING id::text`,
		sourceID, documentID, attachmentID, maxAttempts).Scan(&jobID)
	if err != nil {
		t.Fatalf("insert raw job: %v", err)
	}
	return jobID
}

// TestExpiredLeaseIsLost tests that a token whose lease has expired (but which
// has NOT yet been reclaimed) is a lost-lease for every worker-owned mutation.
func TestExpiredLeaseIsLost(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:30", libraryID: "users/0",
		docKey: "Q1", attKey: "Q1", contentHash: h("sha256:ea"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	// Advance to processing (completion requires processing), THEN expire the
	// lease WITHOUT reclaiming it. Every check below then validates the expiry
	// fence on a `processing` job, so MarkCompletedTx cannot pass on a false
	// positive from status alone.
	if err := lr.rep.MarkProcessing(context.Background(), cj.LeaseRef); err != nil {
		t.Fatal(err)
	}
	lr.expire(t, jobID)

	stale := cj.LeaseRef
	ctx := context.Background()
	checks := []struct {
		name string
		run  func() error
	}{
		{"renew", func() error { return lr.rep.RenewLease(ctx, stale, 120*time.Second) }},
		{"mark-processing", func() error { return lr.rep.MarkProcessing(ctx, stale) }},
		{"mark-failed", func() error { return lr.rep.MarkFailed(ctx, stale, "IO_ERROR", "x") }},
		{"mark-cancelled", func() error { return lr.rep.MarkCancelled(ctx, stale) }},
		{"mark-skipped", func() error { return lr.rep.MarkSkipped(ctx, stale, "whatever") }},
		{"schedule-retry", func() error { _, err := lr.rep.ScheduleRetry(ctx, stale, "E", "m", 5); return err }},
		{"release", func() error { return lr.rep.ReleaseOrExpireLease(ctx, stale) }},
	}
	for _, c := range checks {
		if err := c.run(); !errors.Is(err, ErrLostLease) {
			t.Errorf("%s on expired lease = %v, want ErrLostLease", c.name, err)
		}
	}

	// MarkCompletedTx is fenced the same way (caller-owned tx must roll back).
	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = lr.rep.MarkCompletedTx(ctx, tx, stale, "p", "1", "snap")
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrLostLease) {
		t.Fatalf("mark-completed on expired lease = %v, want ErrLostLease", err)
	}

	// The row must still be untouched (processing, lease held by worker-a) and
	// claimable again after expiry reclaim.
	r := lr.rowOf(t, jobID)
	if r.status != "processing" {
		t.Fatalf("job status after failed fence = %s, want processing (untouched)", r.status)
	}
	if r.claimedBy == nil || *r.claimedBy != "worker-a" {
		t.Fatalf("ownership changed after failed fence: %v", r.claimedBy)
	}
	if r.leaseToken == nil || *r.leaseToken != cj.LeaseToken {
		t.Fatal("lease token changed after failed fence")
	}

	// After expiry the job is reclaimable by a new worker.
	grown := lr.claim(t, defaultClaim("worker-reclaim"))
	if grown == nil || grown.JobID != jobID {
		t.Fatalf("expired job not reclaimed: %v", grown)
	}
}

// TestMarkCompletedUsesStatementTime proves the completion expiry fence is
// evaluated at STATEMENT time (clock_timestamp), not transaction start.
//
// We set lease_until to a value strictly between the completion transaction's
// own now() (fixed at tx start) and the wall-clock moment MarkCompletedTx runs.
// A now()-based fence would then see a still-valid lease and complete; the
// correct clock_timestamp()-based fence sees it expired and rejects. This is the
// only way to distinguish the two, and it must pass with clock_timestamp.
func TestMarkCompletedUsesStatementTime(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:39", libraryID: "users/0",
		docKey: "T1", attKey: "T1", contentHash: h("sha256:stmt"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if err := lr.rep.MarkProcessing(context.Background(), cj.LeaseRef); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// Begin the completion transaction BEFORE setting the expiry so we can read
	// the transaction's own (stable) now() while the tx is open.
	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var txStart time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&txStart); err != nil {
		t.Fatal(err)
	}
	// Set lease_until to txStart + 100ms via a SEPARATE connection: it is after
	// the transaction's now() but will have elapsed by statement time below.
	if _, err := lr.pool.Exec(ctx,
		`UPDATE ingest_jobs SET lease_until=$2 WHERE id=$1`, jobID, txStart.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// Wait past lease_until on the wall clock so statement-time clock_timestamp()
	// is beyond it while the transaction's now() still equals txStart.
	time.Sleep(300 * time.Millisecond)

	err = lr.rep.MarkCompletedTx(ctx, tx, cj.LeaseRef, "p", "1", "snap")
	if !errors.Is(err, ErrLostLease) {
		t.Fatalf("completion with lease_until between tx-now and statement-time = %v, want ErrLostLease (statement-time fence)", err)
	}
}

// TestNullableParentsTerminalizeToSkipped tests that legacy / broken FK rows are
// terminalized to skipped (not a scan/transaction error), covering each FK
// independently: attachment-only, document-only, and source-only missing.
func TestNullableParentsTerminalizeToSkipped(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)

	// One real source + doc + attachment shared by the fixtures.
	ctx := context.Background()
	var srcID, docID, attID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id) VALUES ('http://np', 'users/0') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1,'NP','1','book','NP') RETURNING id::text`, srcID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode, content_type, filename, preferred)
		VALUES ($1,$2,'NP-A',1,'NP','imported_file','application/pdf','a.pdf',true) RETURNING id::text`, srcID, docID).Scan(&attID); err != nil {
		t.Fatal(err)
	}

	// The ingest_jobs FK columns allow NULL; since each FK REFERENCES an existing
	// row, a non-NULL value is guaranteed to resolve. So the only way to represent
	// a missing parent is a NULL in exactly that FK. Each fixture has exactly one
	// NULL FK and the others set, exercising each independently.
	// (a) attachment missing => PARENT_REMOVED.
	jNoAtt := lr.seedPendingOnly(t, &srcID, &docID, nil, 3)
	// (b) document missing => PARENT_REMOVED.
	jNoDoc := lr.seedPendingOnly(t, &srcID, nil, &attID, 3)
	// (c) source missing => PARENT_REMOVED.
	jNoSrc := lr.seedPendingOnly(t, nil, &docID, &attID, 3)

	// One claim pass must drain all three (maxObsoleteSkips=100) to skipped without
	// error or claim. A single call forces the whole queue behind the head to be
	// cleaned in one pass.
	cj := lr.claim(t, defaultClaim("worker-a"))
	if cj != nil {
		t.Fatalf("a NULL-parent job was claimed: %v", cj)
	}
	// The pass must have drained every one of the three fixtures.
	for _, id := range []string{jNoAtt, jNoDoc, jNoSrc} {
		r := lr.rowOf(t, id)
		if r.status != "skipped" {
			t.Fatalf("job %s status = %s, want skipped (missing parent)", id, r.status)
		}
		if r.errorCode == nil || *r.errorCode != "SKIPPED" {
			t.Fatalf("job %s skip code = %v", id, r.errorCode)
		}
		if r.errorMessage == nil || *r.errorMessage != "PARENT_REMOVED" {
			t.Fatalf("job %s skip reason = %v, want PARENT_REMOVED", id, r.errorMessage)
		}
	}
}

// TestForcedRebuildCompletionHashConsistency tests that both normal and
// forced-rebuild completion validate the CURRENT attachment hash against the
// frozen input_snapshot hash, and that forced rebuild never bypasses it.
func TestForcedRebuildCompletionHashConsistency(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	complete := func(t *testing.T, jobID string, ref LeaseRef) error {
		t.Helper()
		tx, err := lr.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = lr.rep.MarkCompletedTx(ctx, tx, ref, "p", "1", "snap")
		_ = tx.Rollback(ctx)
		return err
	}

	// (a) Normal job: unchanged attachment completes.
	_, jNorm := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:31", libraryID: "users/0",
		docKey: "R1", attKey: "R1", contentHash: h("sha256:n1"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if err := lr.rep.MarkProcessing(ctx, cj.LeaseRef); err != nil {
		t.Fatal(err)
	}
	if err := complete(t, jNorm, cj.LeaseRef); err != nil {
		t.Fatalf("normal unchanged completion failed: %v", err)
	}

	// (b) Forced rebuild with a STALE job hash is claimable (frozen snapshot
	// records the CURRENT attachment hash), and unchanged attachment completes.
	_, jForce := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:32", libraryID: "users/1",
		docKey: "R2", attKey: "R2", contentHash: h("sha256:stale-job-hash"), preferred: true,
	}, "pending", 3)
	// The current attachment hash differs from the stale job hash.
	if _, err := lr.pool.Exec(ctx,
		`UPDATE ingest_jobs SET force_rebuild=true WHERE id=$1`, jForce); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx,
		`UPDATE zotero_attachments SET content_hash='sha256:current-2' WHERE zotero_key='R2'`); err != nil {
		t.Fatal(err)
	}
	cj2 := lr.claim(t, defaultClaim("worker-b"))
	if cj2 == nil || cj2.JobID != jForce {
		t.Fatalf("forced-rebuild stale-hash job should claim %s, got %v", jForce, cj2)
	}
	if err := lr.rep.MarkProcessing(ctx, cj2.LeaseRef); err != nil {
		t.Fatal(err)
	}
	// The frozen snapshot carried the CURRENT hash 'sha256:current-2'; attachment
	// unchanged => completion succeeds.
	if err := complete(t, jForce, cj2.LeaseRef); err != nil {
		t.Fatalf("forced-rebuild unchanged completion failed: %v", err)
	}

	// (c) Forced rebuild whose attachment CHANGES after claim => completion fails.
	_, jForceChange := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:33", libraryID: "users/2",
		docKey: "R3", attKey: "R3", contentHash: h("sha256:stale-3"), preferred: true,
	}, "pending", 3)
	if _, err := lr.pool.Exec(ctx,
		`UPDATE ingest_jobs SET force_rebuild=true WHERE id=$1`, jForceChange); err != nil {
		t.Fatal(err)
	}
	cj3 := lr.claim(t, defaultClaim("worker-c"))
	if cj3 == nil || cj3.JobID != jForceChange {
		t.Fatalf("expected claim %s, got %v", jForceChange, cj3)
	}
	if err := lr.rep.MarkProcessing(ctx, cj3.LeaseRef); err != nil {
		t.Fatal(err)
	}
	// Change the attachment AFTER the claim froze its hash.
	if _, err := lr.pool.Exec(ctx,
		`UPDATE zotero_attachments SET content_hash='sha256:changed-after-claim' WHERE zotero_key='R3'`); err != nil {
		t.Fatal(err)
	}
	if err := complete(t, jForceChange, cj3.LeaseRef); !errors.Is(err, ErrLostLease) {
		t.Fatalf("forced-rebuild attachment-change completion = %v, want ErrLostLease", err)
	}

	// (d) NULL snapshot hash (NULL-hash attachment at claim) can never complete.
	_, jNull := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:34", libraryID: "users/3",
		docKey: "R4", attKey: "R4", contentHash: nil, preferred: true,
	}, "pending", 3)
	// The attachment and job both have a NULL hash at claim, so the frozen
	// snapshot records a NULL content_hash and the job is claimable (NULL==NULL).
	if _, err := lr.pool.Exec(ctx,
		`UPDATE zotero_attachments SET content_hash=NULL WHERE zotero_key='R4'`); err != nil {
		t.Fatal(err)
	}
	cj4 := lr.claim(t, defaultClaim("worker-d"))
	if cj4 == nil || cj4.JobID != jNull {
		t.Fatalf("expected claim %s, got %v", jNull, cj4)
	}
	if err := lr.rep.MarkProcessing(ctx, cj4.LeaseRef); err != nil {
		t.Fatal(err)
	}
	if err := complete(t, jNull, cj4.LeaseRef); !errors.Is(err, ErrLostLease) {
		t.Fatalf("NULL-snapshot-hash completion = %v, want ErrLostLease", err)
	}
}

// TestFinalAttemptExhaustion tests that no pending job can carry attempt >=
// max_attempts: retry and release at the ceiling terminalize instead.
func TestFinalAttemptExhaustion(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	// (a) Retry below the limit => pending / RetryScheduled.
	_, jBelow := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:35", libraryID: "users/0",
		docKey: "S1", attKey: "S1", contentHash: h("sha256:s1"), preferred: true,
	}, "pending", 3)
	cjb := lr.claim(t, defaultClaim("worker-a"))
	if out, err := lr.rep.ScheduleRetry(ctx, cjb.LeaseRef, "IO_ERROR", "boom", 5); err != nil {
		t.Fatal(err)
	} else if out != RetryScheduled {
		t.Fatalf("below-limit retry outcome = %v, want RetryScheduled", out)
	}
	if r := lr.rowOf(t, jBelow); r.status != "pending" {
		t.Fatalf("below-limit retry status = %s, want pending", r.status)
	}

	// (b) Retry at the last attempt => failed/RETRY_EXHAUSTED, no lease, completed_at.
	_, jLast := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:36", libraryID: "users/1",
		docKey: "S2", attKey: "S2", contentHash: h("sha256:s2"), preferred: true,
	}, "pending", 3)
	cjl := lr.claim(t, defaultClaim("worker-b"))
	// Force this to be the last attempt (already consumed max attempts).
	if _, err := lr.pool.Exec(ctx,
		`UPDATE ingest_jobs SET attempt=max_attempts WHERE id=$1`, jLast); err != nil {
		t.Fatal(err)
	}
	out, err := lr.rep.ScheduleRetry(ctx, cjl.LeaseRef, "IO_ERROR", "boom", 5)
	if err != nil {
		t.Fatal(err)
	}
	if out != RetryExhausted {
		t.Fatalf("last-attempt retry outcome = %v, want RetryExhausted", out)
	}
	r := lr.rowOf(t, jLast)
	if r.status != "failed" {
		t.Fatalf("last-attempt retry status = %s, want failed", r.status)
	}
	if r.errorCode == nil || *r.errorCode != "RETRY_EXHAUSTED" {
		t.Fatalf("last-attempt error_code = %v, want RETRY_EXHAUSTED", r.errorCode)
	}
	if r.leaseToken != nil || r.claimedBy != nil || r.leaseUntil != nil {
		t.Fatal("exhausted job must have lease fields cleared")
	}
	if r.completedAt == nil {
		t.Fatal("exhausted job must have completed_at set")
	}
	if r.nextAttemptAt != nil {
		t.Fatal("exhausted job must have next_attempt_at cleared")
	}

	// (c) Release on the last attempt => failed/RETRY_EXHAUSTED, not stranded pending.
	_, jRel := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:37", libraryID: "users/2",
		docKey: "S3", attKey: "S3", contentHash: h("sha256:s3"), preferred: true,
	}, "pending", 3)
	cjr := lr.claim(t, defaultClaim("worker-c"))
	if _, err := lr.pool.Exec(ctx,
		`UPDATE ingest_jobs SET attempt=max_attempts WHERE id=$1`, jRel); err != nil {
		t.Fatal(err)
	}
	if err := lr.rep.ReleaseOrExpireLease(ctx, cjr.LeaseRef); err != nil {
		t.Fatal(err)
	}
	rr := lr.rowOf(t, jRel)
	if rr.status != "failed" {
		t.Fatalf("release on last attempt status = %s, want failed (no stranded pending)", rr.status)
	}
	if rr.errorCode == nil || *rr.errorCode != "RETRY_EXHAUSTED" {
		t.Fatalf("release-exhausted code = %v, want RETRY_EXHAUSTED", rr.errorCode)
	}
	if rr.completedAt == nil {
		t.Fatal("release-exhausted must have completed_at")
	}

	// (d) terminalizeStale cleans an existing exhausted pending row EVEN with a
	// non-NULL next_attempt_at (here a future timetable), which is the realistic
	// leftover from earlier retry/release paths. Restoring the defective
	// 'next_attempt_at IS NULL' predicate would let this strand, so it is the
	// regression guard for the broadened cleanup.
	_, jStranded := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:38", libraryID: "users/3",
		docKey: "S4", attKey: "S4", contentHash: h("sha256:s4"), preferred: true,
	}, "pending", 3)
	if _, err := lr.pool.Exec(ctx,
		`UPDATE ingest_jobs SET attempt=max_attempts, next_attempt_at=now() + interval '1 hour' WHERE id=$1`, jStranded); err != nil {
		t.Fatal(err)
	}
	// Any claim pass runs terminalizeStale.
	_ = lr.claim(t, defaultClaim("worker-d"))
	sr := lr.rowOf(t, jStranded)
	if sr.status != "failed" {
		t.Fatalf("exhausted-pending cleanup status = %s, want failed", sr.status)
	}
	if sr.errorCode == nil || *sr.errorCode != "RETRY_EXHAUSTED" {
		t.Fatalf("cleanup code = %v, want RETRY_EXHAUSTED", sr.errorCode)
	}
	if sr.nextAttemptAt != nil {
		t.Fatal("cleanup must clear the stranded next_attempt_at")
	}
	if sr.leaseToken != nil || sr.completedAt == nil {
		t.Fatal("cleanup must clear the lease and set completed_at")
	}
}
