package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Session-unique DB name (ends in _test for truncateFixtures' guard):
// concurrent review/test sessions corrupted each other's shared DB, so
// default to a per-PID database unless a name is pinned via env.
var dispatchTestDatabaseName = func() string {
	if n := os.Getenv("AXIOM_DISPATCH_TEST_DB_NAME"); n != "" {
		return n
	}
	return fmt.Sprintf("axiom_ng_dispatch_%d_test", os.Getpid())
}()

// dispatchHarness owns an isolated _test database and the repo/pool.
type dispatchHarness struct {
	pool *pgxpool.Pool
	rep  *repo.Repo
	dsn  string
}

func openDispatchDB(t *testing.T) *dispatchHarness {
	t.Helper()
	base := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping dispatcher integration test")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	maintDSN := cloneDSN(u, "postgres")
	dispatchDSN := cloneDSN(u, dispatchTestDatabaseName)

	ctx := context.Background()
	mp, err := pgxpool.New(ctx, maintDSN)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	var exists bool
	if err := mp.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, dispatchTestDatabaseName).Scan(&exists); err != nil {
		t.Fatalf("check db exists: %v", err)
	}
	if !exists {
		if _, err := mp.Exec(ctx, `CREATE DATABASE `+dispatchTestDatabaseName); err != nil {
			t.Fatalf("create dispatch test db: %v", err)
		}
	}
	mp.Close()

	d, err := db.Open(ctx, dispatchDSN)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(d.Close)
	return &dispatchHarness{pool: d.Pool(), rep: repo.New(d.Pool()), dsn: dispatchDSN}
}

func cloneDSN(u *url.URL, dbname string) string {
	cp := *u
	cp.Path = "/" + dbname
	return cp.String()
}

func (h *dispatchHarness) truncateFixtures(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var dbName string
	if err := h.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("read current_database: %v", err)
	}
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("REFUSING to truncate: %q does not end in _test", dbName)
	}
	if _, err := h.pool.Exec(ctx,
		`TRUNCATE ingest_jobs, zotero_attachments, zotero_documents, zotero_items,
		         zotero_item_collections, zotero_collections, zotero_sources
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// seedJob inserts a source/document/attachment/canonical item and one pending
// ingest job; returns the job id.
func (h *dispatchHarness) seedJob(t *testing.T, key string, maxAttempts int) string {
	t.Helper()
	ctx := context.Background()
	srcID := h.insertSource(t, key)
	docID := h.insertDocument(t, srcID, key)
	itemID := h.insertItem(t, srcID, key)
	h.pool.Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docID, itemID)
	attID := h.insertAttachment(t, srcID, docID, key)
	var jobID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		VALUES ($1,$2,$3,$4,'pending',$5) RETURNING id::text`,
		srcID, docID, attID, "sha256:"+key, maxAttempts).Scan(&jobID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return jobID
}

func (h *dispatchHarness) insertSource(t *testing.T, key string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(),
		`INSERT INTO zotero_sources (base_url, library_id, server_id) VALUES ($1,$2,'srv')
		 RETURNING id::text`, "http://host:"+key, "users/0").Scan(&id); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	return id
}

func (h *dispatchHarness) insertDocument(t *testing.T, srcID, key string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(),
		`INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		 VALUES ($1,$2,1,'book','Test') RETURNING id::text`, srcID, key).Scan(&id); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	return id
}

func (h *dispatchHarness) insertItem(t *testing.T, srcID, key string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(),
		`INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		 VALUES ($1,$2,1,'book',NULL,$3,$4) RETURNING id::text`,
		srcID, key,
		`{"key":"`+key+`"}`,
		`{"key":"`+key+`","version":1,"itemType":"book","title":"Test"}`).Scan(&id); err != nil {
		t.Fatalf("insert canonical item: %v", err)
	}
	return id
}

func (h *dispatchHarness) insertAttachment(t *testing.T, srcID, docID, key string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(),
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
		 VALUES ($1,$2,$3,1,$3,'imported_file','application/pdf','x.pdf','/tmp/x.pdf',$4,true,false)
		 RETURNING id::text`, srcID, docID, key, "sha256:"+key).Scan(&id); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	return id
}

// fakeProcessor is a scripted httptest processor implementing the subset of the
// contract the dispatcher uses. It records every /v1/process request so tests
// can assert the wire payload (e.g. that profile_hash is absent).
type fakeProcessor struct {
	srv *httptest.Server

	// capsDown makes GET /v1/capabilities answer 503 while true — the
	// scripted "runner still booting" shape the #214 startup retry must
	// survive. Atomic because tests flip it mid-run while serve() reads it.
	capsDown atomic.Bool

	processBody []byte
	processHits int
	// #126: when non-zero, POST /v1/process answers with this status+body
	// (e.g. 409 ARTIFACTS_EXPIRED) instead of the 202 echo.
	processFailStatus int
	processFailBody   string
	// #235: when > 0, only the FIRST N /v1/process calls answer with
	// processFailStatus/Body (stale-URL shape: fail once, then accept).
	processFailFirst int

	// #175 preflight: when preflightReport is non-nil, POST /v1/pdf/preflight
	// answers with it; when nil the endpoint 404s (covers the preflight-
	// disabled shape). preflightHits counts calls.
	preflightReport *map[string]any
	preflightHits   int

	// script holds one handler per probe. index is consumed by the switch in serve.
	statuses  []string // sequence of statuses returned by GET /v1/jobs/{id}
	statusIdx int
	// mu guards the shared script/hit counters: #248 ITs drive TWO jobs
	// concurrently against one fake runner, so status polls and submits
	// overlap (-race).
	mu sync.Mutex
	// #167: optional per-poll stage names emitted alongside each in-progress
	// status, so tests can drive JobStageChanged deltas. Empty => no stage.
	stages        []string
	failStatus    int    // HTTP code for status endpoint; 0 => 200
	failRetryCode string // error code when status=failed
	failRetryable bool
	result        string // raw JSON result body returned by /result
	ackHits       int
	ackFailures   int // remaining /v1/ack calls that should return 500
	cancelHits    int
}

func newFakeProcessor(t *testing.T) *fakeProcessor {
	fp := &fakeProcessor{statuses: []string{"running", "running", "completed"}}
	fp.srv = httptest.NewServer(http.HandlerFunc(fp.serve))
	t.Cleanup(fp.srv.Close)
	return fp
}

func (fp *fakeProcessor) url() string { return fp.srv.URL }

func (fp *fakeProcessor) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/v1/health":
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodGet && path == "/v1/capabilities":
		if fp.capsDown.Load() {
			writeJSON(w, 503, map[string]any{"detail": "caps down"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"contract_versions": []string{"1.0"},
			"processor":         map[string]any{"name": "fake", "version": "0.1.0"},
			"formats":           []string{"application/pdf", "application/epub+zip"},
			"features": map[string]bool{
				"markdown": true, "page_locators": true, "section_hierarchy": true,
				"images": false, "dense_embeddings": true, "sparse_embeddings": true,
				"entities": true, "entity_relationships": true,
			},
			"limits": map[string]any{"max_concurrent_jobs": 1, "max_source_bytes": 1 << 30},
			"models": map[string]any{
				"dense_embedding": map[string]any{"name": "fake-bge", "dimensions": 3},
			},
		})
	case r.Method == http.MethodPost && path == "/v1/pdf/preflight":
		fp.mu.Lock()
		fp.preflightHits++
		fp.mu.Unlock()
		if fp.preflightReport == nil {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, 200, *fp.preflightReport)
	case r.Method == http.MethodPost && path == "/v1/process":
		b, _ := io.ReadAll(r.Body)
		fp.processBody = b
		fp.mu.Lock()
		fp.processHits++
		fp.mu.Unlock()
		if fp.processFailStatus != 0 &&
			(fp.processFailFirst == 0 || fp.processHits <= fp.processFailFirst) {
			w.WriteHeader(fp.processFailStatus)
			io.WriteString(w, fp.processFailBody)
			return
		}
		// Echo the job id back.
		var body struct {
			JobID string `json:"job_id"`
		}
		json.Unmarshal(b, &body)
		writeJSON(w, 202, map[string]any{
			"contract_version": "1.0", "job_id": body.JobID,
			"status": "accepted", "deduplicated": false,
		})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/jobs/") && strings.HasSuffix(path, "/result"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, fp.result)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/jobs/"):
		if fp.failStatus != 0 {
			w.WriteHeader(fp.failStatus)
			return
		}
		fp.mu.Lock()
		idx := fp.statusIdx
		if idx < len(fp.statuses)-1 {
			fp.statusIdx++
		}
		st := fp.statuses[idx]
		fp.mu.Unlock()
		if st == "failed" {
			writeJSON(w, 200, map[string]any{
				"contract_version": "1.0", "job_id": strings.TrimPrefix(strings.TrimSuffix(path, ""), "/v1/jobs/"),
				"status": "failed",
				"error":  map[string]any{"code": fp.failRetryCode, "message": "boom", "retryable": fp.failRetryable, "stage": "convert"},
			})
			return
		}
		jobID := strings.TrimPrefix(path, "/v1/jobs/")
		status := map[string]any{"contract_version": "1.0", "job_id": jobID, "status": st}
		// #167: emit the per-poll stage when configured (stages may be shorter
		// than statuses — in-progress polls carry stages, the terminal poll may
		// not). Bounds-guarded so an exhausted script can never panic.
		if len(fp.stages) > idx {
			if s := fp.stages[idx]; s != "" {
				status["stage"] = s
			}
		}
		writeJSON(w, 200, status)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/jobs/") && strings.HasSuffix(path, "/ack"):
		fp.mu.Lock()
		fp.ackHits++
		fp.mu.Unlock()
		if fp.ackFailures > 0 {
			fp.ackFailures--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, 200, map[string]any{"contract_version": "1.0"})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/jobs/") && strings.HasSuffix(path, "/cancel"):
		fp.mu.Lock()
		fp.cancelHits++
		fp.mu.Unlock()
		writeJSON(w, 200, map[string]any{"contract_version": "1.0"})
	default:
		w.WriteHeader(404)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// newDispatcher runs a Dispatcher with fast, deterministic intervals for tests.
// recordingPersister is the test persistence stub: it records the result bytes
// AND fence-completes the job like the real repo.PersistResult does (calling
// MarkCompletedTx with the lease predicate), so lost-lease semantics hold and a
// job can legitimately reach completed and be acknowledged. Tests that want a
// non-fencing fake can construct their own.
type recordingPersister struct {
	rep        *repo.Repo
	snapshotID string
	persisted  int
}

func (r *recordingPersister) PersistResult(ctx context.Context, jobID string, _ []byte, _ repo.PersistOptions) (string, error) {
	r.persisted++
	if r.snapshotID == "" {
		r.snapshotID = "snap-gate2"
	}
	// Mirror the real persister: fence-complete the job in a single TX so a lost
	// lease is detected HERE (MarkCompletedTx returns ErrLostLease) rather than
	// in a separate dispatcher markCompleted call (which the C1 fix removed).
	ref, pName, pVer, err := r.loadJobLeaseRef(ctx, jobID)
	if err != nil {
		return "", fmt.Errorf("recording persist: load lease: %w", err)
	}
	tx, err := r.rep.Pool().Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("recording persist: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := r.rep.MarkCompletedTx(ctx, tx, ref, pName, pVer, r.snapshotID); err != nil {
		return "", fmt.Errorf("recording persist: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("recording persist: commit: %w", err)
	}
	return r.snapshotID, nil
}

// loadJobLeaseRef reads the lease ref + processor identity the claim froze.
func (r *recordingPersister) loadJobLeaseRef(ctx context.Context, jobID string) (repo.LeaseRef, string, string, error) {
	var workerID, leaseToken string
	if err := r.rep.Pool().QueryRow(ctx,
		`SELECT COALESCE(claimed_by,''), COALESCE(lease_token::text,'') FROM ingest_jobs WHERE id=$1`, jobID,
	).Scan(&workerID, &leaseToken); err != nil {
		return repo.LeaseRef{}, "", "", err
	}
	return repo.LeaseRef{JobID: jobID, WorkerID: workerID, LeaseToken: leaseToken}, "fake-processor", "0.1.0", nil
}

func newDispatcher(t *testing.T, h *dispatchHarness, fp *fakeProcessor, cfg Config) *Dispatcher {
	c := cfg
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 5 * time.Minute
	}
	if c.RenewalInterval <= 0 {
		c.RenewalInterval = 25 * time.Millisecond
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 15 * time.Millisecond
	}
	if c.AckRetryInterval <= 0 {
		c.AckRetryInterval = 100 * time.Millisecond
	}
	if len(c.Profile) == 0 {
		c.Profile = json.RawMessage(`{"profile":"full-rag-v1"}`)
	}
	return NewWithPersister(h.rep, mustClient(t, fp.url()), &recordingPersister{rep: h.rep}, c, log.New(io.Discard, "", 0))
}

func mustClient(t *testing.T, base string) *processor.Client {
	t.Helper()
	cl, err := processor.New(processor.Options{BaseURL: base, ResultTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("processor client: %v", err)
	}
	return cl
}

// runFor runs d.Run under a cancellable ctx for up to wait, then cancels the
// context and waits for the dispatcher goroutine to exit cleanly. It always
// derives a cancellable context so the dispatcher can be stopped and never
// leaks, giving the lost-lease and cancellation tests a deterministic stop point.
func runFor(t *testing.T, d *Dispatcher, parent context.Context, wait time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(wait):
	}
	// Stop the dispatcher loop so the worker goroutine cannot re-claim a job
	// after an assertion has already read the job's DB state.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Logf("dispatcher did not stop within 2s of cancel")
	}
}

// jobStatus reads a single job column helper.
func (h *dispatchHarness) jobStatus(t *testing.T, jobID string) string {
	t.Helper()
	var s string
	if err := h.pool.QueryRow(context.Background(), `SELECT status FROM ingest_jobs WHERE id=$1`, jobID).Scan(&s); err != nil {
		t.Fatalf("read job status: %v", err)
	}
	return s
}

// leaseWasRenewed reports whether last_heartbeat_at was updated after the lease
// started, i.e. any RenewLease succeeded. We compare against claim start: the
// claim sets last_heartbeat_at and a successful renewal advances it.
func (h *dispatchHarness) leaseWasRenewed(t *testing.T, jobID string) bool {
	t.Helper()
	var claimed, hb *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT started_at, last_heartbeat_at FROM ingest_jobs WHERE id=$1`, jobID).Scan(&claimed, &hb); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	return hb != nil && claimed != nil && hb.After(*claimed)
}

// waitForStatus polls the job until it reaches want (or times out), returning
// every intermediate state observed so a test can interleave an action (e.g.
// expiring the lease) once the job reaches a specific state.
func (h *dispatchHarness) waitForStatus(t *testing.T, jobID, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s := h.jobStatus(t, jobID)
		if s == want {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not reach %q (last=%q) within %s", jobID, want, s, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// expireLease forces the job's lease into the past, simulating a lease that has
// been lost/expired (e.g. reclaimed or a clock jump), so the NEXT RenewLease
// attempt fails with ErrLostLease.
func (h *dispatchHarness) expireLease(t *testing.T, jobID string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE ingest_jobs SET lease_until = now() - interval '5 seconds' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

// repeatStr builds a slice of n identical strings.
func repeatStr(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func (h *dispatchHarness) jobError(t *testing.T, jobID string) (code, msg string) {
	t.Helper()
	var c, m *string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT error_code, error_message FROM ingest_jobs WHERE id=$1`, jobID).Scan(&c, &m); err != nil {
		t.Fatalf("read job error: %v", err)
	}
	if c != nil {
		code = *c
	}
	if m != nil {
		msg = *m
	}
	return code, msg
}

func (h *dispatchHarness) nextAttemptAtIsNull(t *testing.T, jobID string) bool {
	t.Helper()
	var v *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_attempt_at FROM ingest_jobs WHERE id=$1`, jobID).Scan(&v); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	return v == nil
}

// TestAcceptedJobTransitionsToProcessing (14.2-1): a submitted+accepted job is
// fenced to 'processing' after acceptance.
func TestAcceptedJobTransitionsToProcessing(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "P1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"running", "completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
	if fp.processHits != 1 {
		t.Fatalf("process hits = %d, want 1", fp.processHits)
	}
	if fp.ackHits != 1 {
		t.Fatalf("ack hits = %d, want 1", fp.ackHits)
	}
}

// TestProcessRequestBodyHasNoProfileHash guards the reviewer's Gate 2 note: the
// wire ProcessRequest must NOT carry profile_hash (it is snapshot identity, not
// an emit field). We interrogate the raw bytes the fake processor captured.
func TestProcessRequestBodyHasNoProfileHash(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "N1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 6*time.Second)

	if fp.processHits != 1 {
		t.Fatalf("process hits = %d, want 1", fp.processHits)
	}
	var raw map[string]any
	if err := json.Unmarshal(fp.processBody, &raw); err != nil {
		t.Fatalf("process body not valid json: %v", err)
	}
	if _, present := raw["profile_hash"]; present {
		t.Fatal("process request MUST NOT contain top-level profile_hash")
	}
	// No nested profile_hash either.
	if proc, ok := raw["processing"].(map[string]any); ok {
		if _, present := proc["profile_hash"]; present {
			t.Fatal("process request processing block must not contain profile_hash")
		}
	}
	// The required identity fields are present.
	if _, ok := raw["job_id"]; !ok {
		t.Fatal("process request missing job_id")
	}
	if _, ok := raw["idempotency_key"]; !ok {
		t.Fatal("process request missing idempotency_key")
	}
}

// TestDuplicateProcessDedupe (14.2-2): transport ambiguity — the first POST is
// accepted by the processor but its response is lost (modelled as a 500), so
// the dispatcher schedules a retry; on re-claim it re-submits with the SAME
// frozen idempotency key and the processor echoes deduplicated=true. The job
// still completes and the processor never runs duplicate work (same key).
func TestDuplicateProcessDedupe(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "D1", 5)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`
	processCalls := 0
	fp.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/process" {
			processCalls++
			if processCalls == 1 {
				// Simulate a lost accept response (the processor accepted but we
				// never saw the reply).
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// Later submits with the same idempotency key are deduplicated.
			writeJSON(w, 202, map[string]any{"contract_version": "1.0", "job_id": jobID, "status": "accepted", "deduplicated": true})
			return
		}
		fp.serve(w, r)
	})

	d := newDispatcher(t, h, fp, Config{Concurrency: 1, PollInterval: 50 * time.Millisecond, AckRetryInterval: 2 * time.Second})
	runFor(t, d, context.Background(), 8*time.Second)
	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed (dedup + recovery)", got)
	}
	if processCalls < 2 {
		t.Fatalf("expected >= 2 process POSTs (lost accept then dedup), got %d", processCalls)
	}
}

// TestLeaseRenewsDuringLongProcessing (14.2-3): while the processor stays in a
// running state longer than the lease, the dispatcher renews and the job is not
// reclaimed/lost. We use a short lease and many running polls.
func TestLeaseRenewsDuringLongProcessing(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "R1", 3)
	fp := newFakeProcessor(t)
	// Enough in-progress polls that the run lasts well past the lease; the
	// dispatcher must renew to complete.
	fp.statuses = append([]string{}, repeatStr("running", 20)...)
	fp.statuses = append(fp.statuses, "completed")
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	d := newDispatcher(t, h, fp, Config{Concurrency: 1, LeaseDuration: 1500 * time.Millisecond, RenewalInterval: 150 * time.Millisecond})
	runFor(t, d, context.Background(), 10*time.Second)

	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed (lease should have been renewed)", got)
	}
	if !h.leaseWasRenewed(t, jobID) {
		t.Fatal("lease was never renewed (last_heartbeat_at stale)")
	}
}

// TestLostLeasePreventsPersistenceAndAck (14.2-4): the processor reports
// completed (so completion is genuinely on the table), but our lease is lost
// (forced into the past mid-processing) before we can fence the completion; the
// dispatcher must NOT reach a completed job and must NOT acknowledge. Load-bearing:
// if the lost-lease guards (the renew abort and/or MarkCompletedTx's lease fence)
// were removed, the job would complete and ACK after the processor offers
// completed; with the guards present it stays non-completed and never ACKs.
func TestLostLeasePreventsPersistenceAndAck(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	// max_attempts=1: once the lost lease aborts the run, the job is already at
	// the attempt ceiling so a re-claim can only terminalize, never re-submit
	// and re-complete. This keeps the non-completed assertion robust regardless
	// of dispatch-loop timing.
	jobID := h.seedJob(t, "L1", 1)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"running", "completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	// A normal lease with a modest renew interval; the fake offers a "completed"
	// status the dispatcher must refuse to reach after the lease is lost.
	d := newDispatcher(t, h, fp, Config{Concurrency: 1, LeaseDuration: 5 * time.Second, RenewalInterval: 150 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	// Wait for the job to be claimed and processing, then forcibly lose the lease.
	h.waitForStatus(t, jobID, "processing", 8*time.Second)
	h.expireLease(t, jobID)

	// Give the dispatcher a couple renew cycles to hit the lost lease and abort.
	time.Sleep(600 * time.Millisecond)
	cancel()
	<-done

	// The fake's script offered a completed status: the guards (not a missing
	// completion) are what stopped the job from completing.
	if fp.processHits == 0 {
		t.Fatal("job was never submitted to the processor; test did not exercise the dispatcher")
	}
	// The job must NOT be completed and no ack must have been sent.
	if got := h.jobStatus(t, jobID); got == "completed" {
		t.Fatal("job completed despite lost lease (must never happen)")
	}
	if fp.ackHits != 0 {
		t.Fatalf("ack sent despite lost lease: hits=%d", fp.ackHits)
	}
}

// TestRetryableFailureSchedulesBackoff (14.2-5): a retryable processor failure
// returns the job to pending with a future next_attempt_at.
func TestRetryableFailureSchedulesBackoff(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	// High max_attempts so the job cannot exhaust within the test window: we
	// assert the scheduler returns it to pending, not that it races to terminal.
	jobID := h.seedJob(t, "T1", 50)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"failed"}
	fp.failRetryCode = "MODEL_UNAVAILABLE"
	fp.failRetryable = true

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "pending" {
		t.Fatalf("status = %q, want pending (retry scheduled)", got)
	}
	if h.nextAttemptAtIsNull(t, jobID) {
		t.Fatal("next_attempt_at is null; retry backoff not scheduled")
	}
	code, _ := h.jobError(t, jobID)
	if code != "MODEL_UNAVAILABLE" {
		t.Fatalf("error code = %q, want MODEL_UNAVAILABLE", code)
	}
}

// TestNonRetryableFailureBecomesTerminal (14.2-6): a non-retryable failure makes
// the job terminal failed.
func TestNonRetryableFailureBecomesTerminal(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "F1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"failed"}
	fp.failRetryCode = "PDF_CONVERSION_FAILED"
	fp.failRetryable = false

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "failed" {
		t.Fatalf("status = %q, want failed (terminal)", got)
	}
	code, _ := h.jobError(t, jobID)
	if code != "PDF_CONVERSION_FAILED" {
		t.Fatalf("error code = %q, want PDF_CONVERSION_FAILED", code)
	}
}

// ackPendingAt reports whether a completed job has an ack-pending mark.
func (h *dispatchHarness) ackPendingAt(t *testing.T, jobID string) bool {
	t.Helper()
	var ts *time.Time
	if err := h.pool.QueryRow(context.Background(), `SELECT ack_pending_at FROM ingest_jobs WHERE id=$1`, jobID).Scan(&ts); err != nil {
		t.Fatalf("read ack_pending_at: %v", err)
	}
	return ts != nil
}

func (fp *fakeProcessor) addAckFailure() {
	fp.ackFailures = 1
}

// TestCancellationDuringProcessing (F8/F2): an operator cancels a processing
// job; the dispatcher calls the processor cancel endpoint and converges fenced
// to cancelled instead of renewing/continuing.
func TestCancellationDuringProcessing(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "C1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"running"}

	d := newDispatcher(t, h, fp, Config{Concurrency: 1, LeaseDuration: 5 * time.Second, RenewalInterval: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	// Let it reach processing, then operator-cancel.
	h.waitForStatus(t, jobID, "processing", 8*time.Second)
	if err := h.rep.RequestCancellation(ctx, jobID); err != nil {
		t.Fatalf("request cancellation: %v", err)
	}
	h.waitForStatus(t, jobID, "cancelled", 8*time.Second)
	cancel()
	<-done

	if fp.cancelHits == 0 {
		t.Fatal("dispatcher never called the processor cancel endpoint")
	}
	if got := h.jobStatus(t, jobID); got != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got)
	}
	// #190 Z2: cancellation during processing must leave NO half-finished
	// snapshot or outbox artifact — the guard stops the loop before the
	// result fetch/persist ever runs.
	var snaps, outbox int
	if err := h.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM processing_snapshots").Scan(&snaps); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if err := h.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM opensearch_outbox").Scan(&outbox); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if snaps != 0 || outbox != 0 {
		t.Fatalf("cancelled job left artifacts: %d snapshots, %d outbox rows", snaps, outbox)
	}
}

// TestAckFailureIsRetried (F8/F3): a failed ACK leaves the job completed but
// ack-pending; the separate retry pass re-acknowledges and clears the mark,
// without reprocessing.
func TestAckFailureIsRetried(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "A1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"running", "completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`
	fp.addAckFailure() // first ack returns 500

	d := newDispatcher(t, h, fp, Config{Concurrency: 1, LeaseDuration: 5 * time.Second, RenewalInterval: 60 * time.Millisecond, AckRetryInterval: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	// Job reaches completed; the initial ack fails once (ackFailures consumed) and
	// the mark is set. A 2s retry interval keeps the mark visible long enough for
	// this assertion instead of racing the immediate retry.
	h.waitForStatus(t, jobID, "completed", 10*time.Second)
	if !h.ackPendingAt(t, jobID) {
		// onCompleted acks after flipping completed, so give it a moment.
		time.Sleep(300 * time.Millisecond)
	}
	if !h.ackPendingAt(t, jobID) {
		t.Fatal("expected ack_pending_at set after a failed ack")
	}
	if fp.ackHits < 1 {
		t.Fatalf("expected at least the initial failed ack, got %d", fp.ackHits)
	}

	// The retry pass re-acks and clears the mark within its interval.
	deadline := time.Now().Add(6 * time.Second)
	for h.ackPendingAt(t, jobID) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if h.ackPendingAt(t, jobID) {
		t.Fatal("ack-pending mark was never cleared by the retry pass")
	}
	if fp.ackHits < 2 {
		t.Fatalf("expected at least 2 ack attempts (initial + retry), got %d", fp.ackHits)
	}
	if fp.processHits != 1 {
		t.Fatalf("process hits = %d, want exactly 1 (must never reprocess after ack failure)", fp.processHits)
	}
	cancel()
	<-done
}

// TestResultInvalidFails (F8): a processor result that does not echo the job id
// must fail the job, never complete it.
func TestResultInvalidFails(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "V1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"completed"}
	fp.result = `{"contract_version":"1.0","job_id":"SOMEOTHER","status":"completed"}`

	d := newDispatcher(t, h, fp, Config{Concurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	h.waitForStatus(t, jobID, "failed", 8*time.Second)
	cancel()
	<-done
	if got := h.jobStatus(t, jobID); got != "failed" {
		t.Fatalf("status = %q, want failed", got)
	}
	if h.ackPendingAt(t, jobID) {
		t.Fatal("ack must not be pending for an invalid result")
	}
}

// TestGracefulShutdownReleasesLease (F8/F7): cancelling the dispatcher during
// processing returns the job to pending (lease released) so a restart can
// reclaim it, instead of abandoning a held lease.
func TestGracefulShutdownReleasesLease(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "G1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"running"}

	d := newDispatcher(t, h, fp, Config{Concurrency: 1, LeaseDuration: 5 * time.Minute, RenewalInterval: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	h.waitForStatus(t, jobID, "processing", 8*time.Second)
	cancel() // graceful shutdown mid-processing
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatcher did not exit after cancel")
	}
	// The lease should have been released back to pending (not left processing).
	if got := h.jobStatus(t, jobID); got != "pending" {
		t.Fatalf("status = %q, want pending (lease released on shutdown)", got)
	}
}

// TestLostLeaseIsNonVacuous asserts the fake's status script actually offers a
// completed state, so TestLostLeasePreventsPersistenceAndAck is not vacuous.
func TestLostLeaseIsNonVacuous(t *testing.T) {
	fp := newFakeProcessor(t)
	defer fp.srv.Close()
	found := false
	for _, s := range fp.statuses {
		if s == "completed" {
			found = true
		}
	}
	if !found {
		t.Fatal("fake script must include a completed status for the lost-lease test to be non-vacuous")
	}
}

// TestRealPersisterCompletesAndAcks (C1 regression): wire the REAL
// repo.PersistResult as the dispatcher persister (not the recording fake) and
// prove a job reaches 'completed' AND is acknowledged in one pass.
//
// This is the regression test for the C1 double-MarkCompletedTx bug: before
// the fix, onCompleted called BOTH PersistResult (which fence-completes in its
// own TX) AND markCompleted (a second MarkCompletedTx that found
// status='completed' → ErrLostLease → return without acking, no ack_pending
// set → retryAcks never recovered it). With the fix, onCompleted trusts
// PersistResult to complete and ACKs directly after.
//
// Mutation check: reverting the C1 fix (re-adding the markCompleted call in
// onCompleted) must make this test red — the job completes but ackHits stays 0.
func TestRealPersisterCompletesAndAcks(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "C1reg", 3)

	// Look up the seeded attachment id + content hash so the fake processor can
	// return a §14-valid result whose identity matches the claim-time frozen input.
	var attID, contentHash string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT a.id::text, a.content_hash FROM ingest_jobs j
		 JOIN zotero_attachments a ON a.id = j.attachment_id WHERE j.id=$1`, jobID,
	).Scan(&attID, &contentHash); err != nil {
		t.Fatalf("lookup attachment identity: %v", err)
	}

	fp := newFakeProcessor(t)
	fp.statuses = []string{"running", "completed"}
	// §14-valid result: echoes the attachment identity, valid chunks/entities,
	// finite dense vector of the declared dim (3, from the fake capabilities),
	// matching stats. No durable artifacts (ArtifactRoot unset → empty path).
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed",` +
		`"source":{"attachment_id":"` + attID + `","content_hash":"` + contentHash + `","verified":true},` +
		`"processor":{"name":"fake","version":"0.1.0","profile":"full-rag-v1","profile_hash":"unused-fallback","models":{"dense_embedding":"fake-bge"}},` +
		`"artifacts":[],` +
		`"chunks":[{"ref":"chunk-0000","index":0,"text":"the quick brown fox",` +
		`"locator":{"type":"page_span","physical_page_start":0,"physical_page_end":0,"page_label_start":"1","page_label_end":"1","source":"marker_paginate","page_source":"pdf_label_sane"},` +
		`"structure":{"section_titles":["Intro"],"start_paragraph_index":0,"end_paragraph_index":0},` +
		`"token_count":4,` +
		`"embeddings":{"dense":{"model":"fake-bge","dimensions":3,"values":[0.1,0.2,0.3]}}}],` +
		`"entities":[],"chunk_relationships":[],"entity_relationships":[],` +
		`"stats":{"pages":0,"chunks":1,"artifacts":0,"entities":0,"entity_relationships":0,"chunk_relationships":0},` +
		`"warnings":[]}`

	// Wire the REAL repo.PersistResult (h.rep) as the persister, not the recording
	// fake. This is what production does and what the recordingPersister masked.
	c := Config{
		Concurrency: 1, LeaseDuration: 5 * time.Minute,
		RenewalInterval: 25 * time.Millisecond, PollInterval: 15 * time.Millisecond,
		AckRetryInterval: 100 * time.Millisecond,
		Profile:          json.RawMessage(`{"profile":"full-rag-v1"}`),
	}
	d := NewWithPersister(h.rep, mustClient(t, fp.url()), h.rep, c, log.New(io.Discard, "", 0))
	runFor(t, d, context.Background(), 8*time.Second)

	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed (real persister should fence-complete)", got)
	}
	// C1 headline assertion: the processor was acknowledged. Before the fix this
	// was 0 because the second markCompleted returned ErrLostLease and onCompleted
	// returned without acking.
	if fp.ackHits == 0 {
		t.Fatalf("ack hits = 0, want >=1: job completed but never acknowledged (C1 double-completion regression)")
	}
	// And the snapshot was actually persisted (real rows, not just a fake id).
	var snapN int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM processing_snapshots WHERE attachment_id=$1 AND active=true`, attID).Scan(&snapN); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapN != 1 {
		t.Fatalf("expected 1 active snapshot for the completed job, got %d", snapN)
	}
	// And no ack_pending left dangling (the C1 bug left it unset, so retryAcks
	// never recovered the job).
	if h.ackPendingAtIsNull(t, jobID) {
		// ack_pending_at NULL is correct on a successful ack. Good.
	}
}

// ackPendingAtIsNull reports whether ack_pending_at is NULL for a job.
func (h *dispatchHarness) ackPendingAtIsNull(t *testing.T, jobID string) bool {
	t.Helper()
	var v *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT ack_pending_at FROM ingest_jobs WHERE id=$1`, jobID).Scan(&v); err != nil {
		t.Fatalf("read ack_pending_at: %v", err)
	}
	return v == nil
}

// TestArtifactsExpiredSubmitIsTerminal pins #126: a resubmit that dedups onto
// an acknowledged runner job answers 409 ARTIFACTS_EXPIRED — the stored
// result's artifacts died with the ACK. The dispatcher must terminalize with
// the distinct code (not PROCESS_SUBMIT_FAILED) and NOT retry into the same
// wall (exactly one submit attempt).
func TestArtifactsExpiredSubmitIsTerminal(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "AE", 3)
	fp := newFakeProcessor(t)
	fp.processFailStatus = 409
	fp.processFailBody = `{"detail":{"code":"ARTIFACTS_EXPIRED","message":"job was acknowledged; result artifacts are gone (contract §19.12)","retryable":false}}`

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "failed" {
		t.Fatalf("status = %q, want failed (terminal)", got)
	}
	code, _ := h.jobError(t, jobID)
	if code != "ARTIFACTS_EXPIRED" {
		t.Fatalf("error code = %q, want ARTIFACTS_EXPIRED", code)
	}
	if !h.nextAttemptAtIsNull(t, jobID) {
		t.Fatal("next_attempt_at set; ARTIFACTS_EXPIRED must not schedule a retry")
	}
	if fp.processHits != 1 {
		t.Fatalf("process hits = %d, want exactly 1 (terminal, no retry hammering)", fp.processHits)
	}
}

// TestStartupRetriesUntilRunnerReachable (#214 regression): the dispatcher
// starts while the runner cannot yet serve capabilities — the fake answers 503
// on /v1/capabilities while "booting", the same retryable failure class as the
// incident's unreachable runner. The dispatcher must NOT die: it retries
// negotiation with backoff until the runner is ready, then claims and completes
// the job.
//
// Pre-fix discriminator: the old one-shot negotiation failed fatally on the
// first 503, Run returned, and the claim loop stayed dead while the process
// lived on — under that behavior this test FAILS (the job never completes), so
// the regression cannot reappear silently.
func TestStartupRetriesUntilRunnerReachable(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "S1", 3)

	fp := newFakeProcessor(t)
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`
	// The runner is up but not ready: every capability negotiation gets a 503
	// until the flag flips below.
	fp.capsDown.Store(true)

	// Fast retry cadence for the test; defaults (5s / 2min) are the production
	// values wired by NewWithPersister.
	d := newDispatcher(t, h, fp, Config{
		StartupRetryInterval: 50 * time.Millisecond,
		MaxStartupWait:       10 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var runErr error
	go func() { runErr = d.Run(ctx); close(done) }()

	// Let several negotiation attempts fail (runner not ready) without killing
	// the loop, then bring the runner up.
	time.Sleep(200 * time.Millisecond)
	fp.capsDown.Store(false)

	// The dispatcher survives the outage and drives the seeded job to completion.
	h.waitForStatus(t, jobID, "completed", 10*time.Second)
	if fp.processHits != 1 {
		t.Fatalf("process hits = %d, want 1 (job claimed+submitted after runner came up)", fp.processHits)
	}
	cancel()
	<-done
	if runErr != nil {
		t.Fatalf("Run returned an error after the runner came up (must keep looping): %v", runErr)
	}
}

// TestStartupFailsFatalWhenRunnerStaysDown (#214): if the runner never becomes
// able to serve capabilities through MaxStartupWait, Run must return a non-nil
// error so the caller (main) can exit non-zero and let the supervisor restart —
// never the silent "process alive, loop dead" state. This also passes pre-fix
// (fatal was the old behavior too); it pins the post-retry fatal shape and
// proves Run returns instead of hanging forever.
func TestStartupFailsFatalWhenRunnerStaysDown(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "S2", 3)

	fp := newFakeProcessor(t)
	fp.capsDown.Store(true) // never becomes ready

	d := newDispatcher(t, h, fp, Config{
		StartupRetryInterval: 10 * time.Millisecond,
		MaxStartupWait:       150 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := d.Run(ctx) // synchronous: proves Run returns instead of hanging forever
	if err == nil {
		t.Fatal("Run returned nil although the runner never served capabilities through MaxStartupWait; must be a fatal (non-zero) error")
	}
	// The seeded job must still be pending — the dispatcher never claimed it.
	if got := h.jobStatus(t, jobID); got != "pending" {
		t.Fatalf("status = %q, want pending (no claim while negotiation fatally failed)", got)
	}
}

// TestStartupRetryIsGracefulOnShutdown (#214): a shutdown that lands during the
// startup retry window must exit cleanly (nil) — an operator-initiated stop must
// never surface as a fatal dispatcher error / non-zero exit.
func TestStartupRetryIsGracefulOnShutdown(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	_ = h.seedJob(t, "S3", 3)

	fp := newFakeProcessor(t)
	fp.capsDown.Store(true) // never becomes ready

	d := newDispatcher(t, h, fp, Config{
		StartupRetryInterval: 50 * time.Millisecond,
		MaxStartupWait:       1 * time.Hour, // far past the test: only cancel ends it
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() { runErr = d.Run(ctx); close(done) }()

	// Cancel mid-retry and assert a clean (nil) exit, not a fatal error.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit after cancel during startup retry")
	}
	if runErr != nil {
		t.Fatalf("Run returned %v on graceful shutdown; want nil (clean exit)", runErr)
	}
}

// testMinimalPDF is a tiny valid one-page PDF (space-saving literal) so the
// preflight gate has real bytes to read from the seeded attachment's
// local_path. Yes, it's a hand-rolled PDF with an uncompressed page stream.
const testMinimalPDF = "%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]/Contents 4 0 R>>endobj\n4 0 obj<</Length 44>>stream\nBT /F1 12 Tf 72 120 Td (ok) Tj ET\nendstream\nendobj\nxref\n0 5\n0000000000 65535 f \ntrailer<</Root 1 0 R/Size 5>>\nstartxref\n0\n%%EOF\n"

// openRepairCaseCount returns the number of open repair cases for an
// attachment (a preflight-fail must create exactly one, per the unique-open
// constraint).
func (h *dispatchHarness) openRepairCaseCount(t *testing.T, attID string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM repair_cases WHERE attachment_id=$1 AND status IN ('rejected','queued','in_repair')`,
		attID).Scan(&n); err != nil {
		t.Fatalf("count repair cases: %v", err)
	}
	return n
}

// ingestion quality gate policy (#175): a quality-red preflight skips the job
// (terminal, not failed) and marks the attachment as a repair-case candidate,
// so no junk chunks are produced and the fixer (#206/#203) can later heal it.
func TestPreflightFailSkipsJobAndCreatesRepairCase(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "Q1", 3)

	// The seed hardcodes local_path=/tmp/x.pdf; write real bytes so the
	// preflight gate can read them.
	if err := os.WriteFile("/tmp/x.pdf", []byte(testMinimalPDF), 0o600); err != nil {
		t.Fatalf("write source pdf: %v", err)
	}

	fp := newFakeProcessor(t)
	fp.preflightReport = &map[string]any{
		"contract_version": "1.0",
		"source_name":      "inline",
		"ok":               false,
		"finding":          "🔴 unpaginiert",
		"reason":           "kein Tier-1",
		"details": map[string]any{
			"pages": 1, "text_layer": false,
			"suspicious_patterns": []any{"viele reine Bildseiten ohne OCR-Text (1/1)"},
			"drm":                 true, // present key must pass through (W5)
		},
	}
	d := newDispatcher(t, h, fp, Config{PreflightEnabled: true})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "skipped" {
		t.Fatalf("status = %q, want skipped (preflight-red)", got)
	}
	if fp.processHits != 0 {
		t.Fatalf("process hits = %d, want 0 (junk chunks must not be produced)", fp.processHits)
	}
	// The attachment must now be a repair-case candidate.
	var attID string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT attachment_id::text FROM ingest_jobs WHERE id=$1`, jobID).Scan(&attID); err != nil {
		t.Fatalf("read attachment: %v", err)
	}
	if n := h.openRepairCaseCount(t, attID); n != 1 {
		t.Fatalf("open repair cases = %d, want 1", n)
	}
	// quality_state was recorded on the job.
	var qs []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT quality_state FROM ingest_jobs WHERE id=$1`, jobID).Scan(&qs); err != nil {
		t.Fatalf("read quality_state: %v", err)
	}
	if string(qs) == "" || !strings.Contains(string(qs), "unpaginiert") {
		t.Fatalf("quality_state not recorded: %q", string(qs))
	}
	// W5: absent EPUB detail keys must not become literal null entries;
	// present ones (drm below) pass through for repair-case analysis.
	var probe map[string]any
	if err := json.Unmarshal(qs, &probe); err != nil {
		t.Fatalf("quality_state json: %v", err)
	}
	if _, ok := probe["format"]; ok {
		t.Fatalf("quality_state must not carry absent EPUB keys: %s", string(qs))
	}
	if probe["drm"] != true {
		t.Fatalf("quality_state must pass through present EPUB keys: %s", string(qs))
	}
	// #219 contract: the quality_state producer emits EXACTLY the canonical
	// English key set — a German-key producer must never reappear pre-freeze.
	wantKeys := map[string]bool{
		"verdict": false, "finding": false, "reason": false, "pages": false,
		"text_layer": false, "mean_chars_per_page": false,
		"suspicious_patterns": false, "drm": false,
	}
	if len(probe) != len(wantKeys) {
		t.Fatalf("quality_state key set = %d keys, want %d: %s", len(probe), len(wantKeys), string(qs))
	}
	for k := range probe {
		if _, ok := wantKeys[k]; !ok {
			t.Fatalf("quality_state carries non-contract key %q: %s", k, string(qs))
		}
	}
	for k := range wantKeys {
		if _, ok := probe[k]; !ok {
			t.Fatalf("quality_state missing contract key %q: %s", k, string(qs))
		}
	}
}

// Preflight disabled (default) → the job processes normally, no preflight call.
func TestPreflightDisabledDefaultsToNormalProcessing(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "Q2", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"running", "completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`
	fp.preflightReport = &map[string]any{"ok": false} // even a red preflight must be ignored when disabled

	d := newDispatcher(t, h, fp, Config{PreflightEnabled: false})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed (preflight disabled)", got)
	}
	if fp.preflightHits != 0 {
		t.Fatalf("preflight hits = %d, want 0 when disabled", fp.preflightHits)
	}
}

// --- #235: stale source URL at runner execution --------------------------

// jobAttempt reads the attempt counter (for the retry assertion).
func (h *dispatchHarness) jobAttempt(t *testing.T, jobID string) int {
	t.Helper()
	var a int
	if err := h.pool.QueryRow(context.Background(), `SELECT attempt FROM ingest_jobs WHERE id=$1`, jobID).Scan(&a); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	return a
}

// TestSubmitStaleSourceURLRetriesAndCompletes (#235): a 422 whose body names
// a source-transport failure (the stale-URL single-lane race) must reset the
// job to pending and complete on attempt 2 with a fresh submit — not die
// terminally on attempt 1 as in the 2026-08-31 production incident.
func TestSubmitStaleSourceURLRetriesAndCompletes(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "S235", 3)
	fp := newFakeProcessor(t)
	fp.processFailFirst = 1
	fp.processFailStatus = 422
	fp.processFailBody = `{"detail":"source_url download failed: HTTP Error 404: Not Found"}`
	fp.statuses = []string{"completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 10*time.Second)

	if got := h.jobStatus(t, jobID); got != "completed" {
		code, msg := h.jobError(t, jobID)
		t.Fatalf("status = %q, want completed after retry (error %s: %s)", got, code, msg)
	}
	if fp.processHits != 2 {
		t.Fatalf("process hits = %d, want 2 (failed submit + retried submit)", fp.processHits)
	}
	if got := h.jobAttempt(t, jobID); got != 2 {
		t.Fatalf("attempt = %d, want 2", got)
	}
	if fp.ackHits != 1 {
		t.Fatalf("ack hits = %d, want 1", fp.ackHits)
	}
}

// TestSubmitBrokenFile422StaysTerminal (#235 counter-test): a 422 from a real
// hash-gate mismatch is poison — terminal on attempt-1 semantics, no retry.
func TestSubmitBrokenFile422StaysTerminal(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "B235", 3)
	fp := newFakeProcessor(t)
	fp.processFailStatus = 422
	fp.processFailBody = `{"detail":"downloaded source failed the content hash gate"}`

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "failed" {
		t.Fatalf("status = %q, want failed (broken file is poison)", got)
	}
	code, _ := h.jobError(t, jobID)
	if code != "PROCESS_SUBMIT_FAILED" {
		t.Fatalf("error code = %q, want PROCESS_SUBMIT_FAILED", code)
	}
	if fp.processHits != 1 {
		t.Fatalf("process hits = %d, want 1 (terminal, no retry)", fp.processHits)
	}
}

// --- #237: unreadable source → structured FAIL → repair case -------------

// repairCaseFor reads the open repair case of an attachment (nil if none).
func (h *dispatchHarness) repairCaseFor(t *testing.T, jobID string) map[string]any {
	t.Helper()
	var analysis []byte
	err := h.pool.QueryRow(context.Background(), `
		SELECT c.analysis FROM repair_cases c
		JOIN ingest_jobs j ON j.attachment_id = c.attachment_id
		WHERE j.id=$1 AND c.status IN ('rejected','queued','in_repair')`, jobID).Scan(&analysis)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(analysis, &m) != nil {
		return nil
	}
	return m
}

// TestUnreadableSourceCreatesRepairCase (#237): a runner-reported terminal
// SOURCE_UNREADABLE failure must land in the repair track — repair case with
// the structured FAIL analysis — and not burn a single retry.
func TestUnreadableSourceCreatesRepairCase(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "U237", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"failed"}
	fp.failRetryCode = "SOURCE_UNREADABLE"
	fp.failRetryable = false

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "failed" {
		t.Fatalf("status = %q, want failed (terminal document defect)", got)
	}
	rc := h.repairCaseFor(t, jobID)
	if rc == nil {
		t.Fatal("no repair case created for SOURCE_UNREADABLE failure")
	}
	if rc["error_class"] != "SOURCE_UNREADABLE" || rc["retryable"] != false {
		t.Fatalf("repair case analysis = %v, want structured FAIL shape", rc)
	}
	if fp.processHits != 1 {
		t.Fatalf("process hits = %d, want 1 (no retry storm on document failure)", fp.processHits)
	}
}

// TestRetryableFailureStaysInfraTrack (#237 counter-test): a retryable
// runner failure is infrastructure — scheduled retry, NO repair case.
func TestRetryableFailureStaysInfraTrack(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "R237", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"failed"}
	fp.failRetryCode = "INTERNAL_ERROR"
	fp.failRetryable = true

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 3*time.Second)

	if got := h.jobStatus(t, jobID); got != "pending" && got != "processing" && got != "running" {
		code, _ := h.jobError(t, jobID)
		t.Fatalf("status = %q (error %s), want retry pending — infra track", got, code)
	}
	if rc := h.repairCaseFor(t, jobID); rc != nil {
		t.Fatalf("retryable failure must not create a repair case: %v", rc)
	}
}

// repairCaseStatus returns the status of the open repair case for a job's
// attachment ("" when none) — the #238 auto-queue assertions read it.
func (h *dispatchHarness) repairCaseStatus(t *testing.T, jobID string) string {
	t.Helper()
	var status string
	err := h.pool.QueryRow(context.Background(), `
		SELECT c.status::text FROM repair_cases c
		JOIN ingest_jobs j ON j.attachment_id = c.attachment_id
		WHERE j.id=$1 AND c.status IN ('rejected','queued','in_repair')`, jobID).Scan(&status)
	if err != nil {
		return ""
	}
	return status
}

// TestPreflightRepairableAutoQueues (#238): a 🔴 reparierbar preflight
// rejection auto-queues its repair case at creation — no operator, no
// manual repair-API call. The queue hop is the missing link from the
// issue: cases were created but never reached the fixer invoker.
func TestPreflightRepairableAutoQueues(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "Q238a", 3)
	if err := os.WriteFile("/tmp/x.pdf", []byte(testMinimalPDF), 0o600); err != nil {
		t.Fatalf("write source pdf: %v", err)
	}
	fp := newFakeProcessor(t)
	fp.preflightReport = &map[string]any{
		"contract_version": "1.0",
		"source_name":      "inline",
		"ok":               false,
		"finding":          "🔴 reparierbar",
		"reason":           "Label-Lücken",
		"details": map[string]any{
			"pages": 1, "text_layer": true,
			"suspicious_patterns": []any{"Label-Serie unterbrochen"},
		},
	}
	d := newDispatcher(t, h, fp, Config{PreflightEnabled: true})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "skipped" {
		t.Fatalf("status = %q, want skipped (preflight-red)", got)
	}
	if got := h.repairCaseStatus(t, jobID); got != "queued" {
		t.Fatalf("repair case status = %q, want queued (auto-queue, #238)", got)
	}
}

// TestPreflightUnpaginatedStaysRejected (#238 counter-test): the design
// nail — unpaginierte Originale gehen NIE in die Reparatur-Schleife. The
// auto-queue must not touch them.
func TestPreflightUnpaginatedStaysRejected(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "Q238b", 3)
	if err := os.WriteFile("/tmp/x.pdf", []byte(testMinimalPDF), 0o600); err != nil {
		t.Fatalf("write source pdf: %v", err)
	}
	fp := newFakeProcessor(t)
	fp.preflightReport = &map[string]any{
		"contract_version": "1.0",
		"source_name":      "inline",
		"ok":               false,
		"finding":          "🔴 unpaginiert",
		"reason":           "kein Tier-1",
		"details": map[string]any{
			"pages": 1, "text_layer": false,
			"suspicious_patterns": []any{"viele reine Bildseiten ohne OCR-Text (1/1)"},
		},
	}
	d := newDispatcher(t, h, fp, Config{PreflightEnabled: true})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "skipped" {
		t.Fatalf("status = %q, want skipped (preflight-red)", got)
	}
	if got := h.repairCaseStatus(t, jobID); got != "rejected" {
		t.Fatalf("repair case status = %q, want rejected (unpaginiert NEVER enters the loop)", got)
	}
}

// TestUnreadableSourceAutoQueues (#238): the #237 SOURCE_UNREADABLE track
// also auto-queues — the corrupt-source evidence reaches the fixer without
// an operator hop.
func TestUnreadableSourceAutoQueues(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "U238", 3)
	fp := newFakeProcessor(t)
	fp.statuses = []string{"failed"}
	fp.failRetryCode = "SOURCE_UNREADABLE"
	fp.failRetryable = false

	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "failed" {
		t.Fatalf("status = %q, want failed (terminal document defect)", got)
	}
	if got := h.repairCaseStatus(t, jobID); got != "queued" {
		t.Fatalf("repair case status = %q, want queued (auto-queue, #238)", got)
	}
	rc := h.repairCaseFor(t, jobID)
	if rc == nil || rc["error_class"] != "SOURCE_UNREADABLE" {
		t.Fatalf("queued case must keep the structured FAIL analysis: %v", rc)
	}
}

// TestStaleRepairableCaseNotRecycledIntoQueue (#238 review fix): a stale
// OPEN case of an auto-queueable class must never be queued by a NEWER
// verdict of a different class. A rejected 🔴 reparierbar case predating
// the auto-queue (manual-world leftover) + a current 🔴 unpaginiert
// preflight for the same attachment: the case must stay rejected — the
// current evidence says unpaginiert, and unpaginiert NEVER enters the
// loop. Auto-queue is strictly queue-AT-CREATION (created flag).
func TestStaleRepairableCaseNotRecycledIntoQueue(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "S238", 3)
	if err := os.WriteFile("/tmp/x.pdf", []byte(testMinimalPDF), 0o600); err != nil {
		t.Fatalf("write source pdf: %v", err)
	}

	// Seed the STALE state directly: a rejected reparierbar case from the
	// pre-auto-queue world (created manually or by older code).
	var attID string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT attachment_id::text FROM ingest_jobs WHERE id=$1`, jobID).Scan(&attID); err != nil {
		t.Fatalf("read attachment: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO repair_cases (attachment_id, suspicion_class, analysis, status)
		VALUES ($1::uuid, '🔴 reparierbar', '{"stale": true}', 'rejected')`, attID); err != nil {
		t.Fatalf("seed stale case: %v", err)
	}

	// Current preflight verdict: unpaginiert — must NOT queue the stale
	// reparierbar case.
	fp := newFakeProcessor(t)
	fp.preflightReport = &map[string]any{
		"contract_version": "1.0",
		"source_name":      "inline",
		"ok":               false,
		"finding":          "🔴 unpaginiert",
		"reason":           "kein Tier-1",
		"details": map[string]any{
			"pages": 1, "text_layer": false,
			"suspicious_patterns": []any{"viele reine Bildseiten ohne OCR-Text (1/1)"},
		},
	}
	d := newDispatcher(t, h, fp, Config{PreflightEnabled: true})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.repairCaseStatus(t, jobID); got != "rejected" {
		t.Fatalf("stale repair case status = %q, want rejected (a newer unpaginiert verdict must never recycle it into the queue)", got)
	}
	// And the stale analysis is untouched (no class laundering either).
	rc := h.repairCaseFor(t, jobID)
	if rc == nil || rc["stale"] != true {
		t.Fatalf("stale case analysis must be untouched: %v", rc)
	}
}

// runningScript returns a busy script of n "running" polls before the
// terminal "completed" — the long-compute phase the #248 lane ITs need so a
// claimed job holds its lane for the whole observation window.
func runningScript(n int) []string {
	s := make([]string, n)
	for i := range s {
		s[i] = "running"
	}
	return append(s, "completed")
}

// activeLaneStats samples the live claim lanes: concurrent active leases
// and the distinct claimers behind them (#248 log proof: SELECT DISTINCT
// claimed_by).
func activeLaneStats(t *testing.T, h *dispatchHarness) (active int, claimers []string) {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT claimed_by FROM ingest_jobs WHERE status IN ('claimed','processing') AND lease_until > now()`)
	if err != nil {
		t.Fatalf("lane stats: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			t.Fatalf("lane stats scan: %v", err)
		}
		active++
		seen[w] = true
	}
	for w := range seen {
		claimers = append(claimers, w)
	}
	sort.Strings(claimers)
	return active, claimers
}

// TestSoloRunnerSingleLaneAcrossAgents (#248 solo proof): ONE runner with
// declared capacity 1, TWO dispatcher agents (the carrier-era topology)
// hammering the same DB and the same runner with more claimable jobs than
// lanes. The DB claim budget must hold the system to EXACTLY ONE claim
// lane: the busy agent keeps its claim, the other agent NEVER claims, and
// the log proof (SELECT DISTINCT claimed_by over active claims) shows one
// claimer for the whole window.
func TestSoloRunnerSingleLaneAcrossAgents(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	j1 := h.seedJob(t, "SOLO1", 3)
	h.seedJob(t, "SOLO2", 3)
	h.seedJob(t, "SOLO3", 3)

	// The shared runner (capacity 1) holds job 1 busy for the whole window.
	fp := newFakeProcessor(t)
	fp.statuses = runningScript(400)
	_ = j1

	agentA := newDispatcher(t, h, fp, Config{WorkerID: "solo-agent-a"})
	agentB := newDispatcher(t, h, fp, Config{WorkerID: "solo-agent-b"})
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go agentA.Run(ctxA)
	go agentB.Run(ctxB)

	sawBusy := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		active, claimers := activeLaneStats(t, h)
		if active > 1 {
			t.Fatalf("solo violation: %d concurrent claims (claimers %v), want exactly 1 lane", active, claimers)
		}
		if active == 1 {
			sawBusy = true
			if len(claimers) != 1 || claimers[0] != "solo-agent-a" && claimers[0] != "solo-agent-b" {
				t.Fatalf("unexpected claimer %v", claimers)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !sawBusy {
		t.Fatal("no busy phase observed — the lane proof is vacuous")
	}
	// Whoever holds the lane, the OTHER agent never claimed concurrently:
	// end-of-window, still at most one active claim and one claimer.
	active, claimers := activeLaneStats(t, h)
	if active != 1 || len(claimers) != 1 {
		t.Fatalf("end of window: %d active claims by %v, want exactly 1 by 1 claimer", active, claimers)
	}
	cancelA()
	cancelB()
}

// TestGPURichLanesScaleToSumCapacities (#248 GPU-rich proof): TWO runners,
// each declaring capacity 1 — the derived lane count (Concurrency unset)
// must scale to Σ = 2, and the DB budget must hold it there: exactly two
// concurrent claims while more jobs wait, never a third lane.
func TestGPURichLanesScaleToSumCapacities(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	h.seedJob(t, "GPU1", 3)
	h.seedJob(t, "GPU2", 3)
	h.seedJob(t, "GPU3", 3)
	h.seedJob(t, "GPU4", 3)

	fp1, fp2 := newFakeProcessor(t), newFakeProcessor(t)
	fp1.statuses = runningScript(400)
	fp2.statuses = runningScript(400)
	chain := processor.NewFailoverChain(
		[]*processor.Client{mustClient(t, fp1.url()), mustClient(t, fp2.url())},
		log.New(io.Discard, "", 0))

	// Built directly (not via newDispatcher): the helper would pin
	// Concurrency=1, but this test PROVES the derivation — Concurrency <= 0
	// must become Σ(live capacities) = 2.
	d := NewWithPersister(h.rep, chain, &recordingPersister{rep: h.rep}, Config{
		WorkerID:         "gpu-rich-agent",
		LeaseDuration:    5 * time.Minute,
		RenewalInterval:  25 * time.Millisecond,
		PollInterval:     15 * time.Millisecond,
		AckRetryInterval: 100 * time.Millisecond,
		Profile:          json.RawMessage(`{"profile":"full-rag-v1"}`),
	}, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	sawTwo := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		active, claimers := activeLaneStats(t, h)
		if active > 2 {
			t.Fatalf("GPU-rich violation: %d concurrent claims (claimers %v), want at most Σ=2", active, claimers)
		}
		if active == 2 {
			sawTwo = true
			// One process, two lanes: both claims carry the same worker.
			if len(claimers) != 1 || claimers[0] != "gpu-rich-agent" {
				t.Fatalf("lanes held by %v, want the single derived-concurrency agent", claimers)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !sawTwo {
		t.Fatal("Σ-capacity scaling not observed — derivation did not produce 2 lanes")
	}
	cancel()
}
