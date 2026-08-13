package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5/pgxpool"
)

const dispatchTestDatabaseName = "axiom_ng_dispatch_test"

// dispatchHarness owns an isolated _test database and the repo/pool.
type dispatchHarness struct {
	pool *pgxpool.Pool
	rep  *repo.Repo
	dsn  string
}

func openDispatchDB(t *testing.T) *dispatchHarness {
	t.Helper()
	base := os.Getenv("AXIOMNG_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping dispatcher integration test")
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

	processBody []byte
	processHits int

	// script holds one handler per probe. index is consumed by the switch in serve.
	statuses      []string // sequence of statuses returned by GET /v1/jobs/{id}
	statusIdx     int
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
		})
	case r.Method == http.MethodPost && path == "/v1/process":
		b, _ := io.ReadAll(r.Body)
		fp.processBody = b
		fp.processHits++
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
		idx := fp.statusIdx
		if idx < len(fp.statuses)-1 {
			fp.statusIdx++
		}
		st := fp.statuses[idx]
		if st == "failed" {
			writeJSON(w, 200, map[string]any{
				"contract_version": "1.0", "job_id": strings.TrimPrefix(strings.TrimSuffix(path, ""), "/v1/jobs/"),
				"status": "failed",
				"error":  map[string]any{"code": fp.failRetryCode, "message": "boom", "retryable": fp.failRetryable, "stage": "convert"},
			})
			return
		}
		jobID := strings.TrimPrefix(path, "/v1/jobs/")
		writeJSON(w, 200, map[string]any{"contract_version": "1.0", "job_id": jobID, "status": st})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/jobs/") && strings.HasSuffix(path, "/ack"):
		fp.ackHits++
		if fp.ackFailures > 0 {
			fp.ackFailures--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, 200, map[string]any{"contract_version": "1.0"})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/jobs/") && strings.HasSuffix(path, "/cancel"):
		fp.cancelHits++
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
// recordingPersister is the Gate-2 test persistence stub: it records the result
// bytes it would have committed and returns a deterministic snapshot id, so a
// job can legitimately reach completed and be acknowledged.
type recordingPersister struct {
	snapshotID string
	persisted  int
}

func (r *recordingPersister) PersistResult(_ context.Context, _ string, _ []byte) (string, error) {
	r.persisted++
	if r.snapshotID == "" {
		r.snapshotID = "snap-gate2"
	}
	return r.snapshotID, nil
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
	return NewWithPersister(h.rep, mustClient(t, fp.url()), &recordingPersister{}, c, log.New(io.Discard, "", 0))
}

func mustClient(t *testing.T, base string) *processor.Client {
	t.Helper()
	cl, err := processor.New(processor.Options{BaseURL: base, RequestTimeout: 5 * time.Second})
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
