package dispatcher

// L5 outbox drainer tests. DB-backed (dispatchHarness) with httptest fake
// OpenSearch servers exercising the REAL openSearchClient HTTP path.
//
// Covers the work-package gate:
//   - row done after successful indexing
//   - no double-drain: concurrent claims over one row, exactly one wins
//   - OS error → attempts+1, stays pending, next_attempt_at in the future
//   - failed after max attempts, snapshot rows untouched
//   - disabled (empty URL) → no error, nothing happens

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedOutboxSnapshot inserts a snapshot + n chunks (with 3-dim embeddings) +
// one pending outbox row; returns (outboxRowID, snapshotID).
func (h *dispatchHarness) seedOutboxSnapshot(t *testing.T, key string, nChunks, attempts int) (string, string) {
	t.Helper()
	ctx := context.Background()
	srcID := h.insertSource(t, key)
	docID := h.insertDocument(t, srcID, key)
	attID := h.insertAttachment(t, srcID, docID, key)

	var snapID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots
			(attachment_id, content_hash, processor_name, processor_version,
			 profile_hash, document_id, profile, active)
		VALUES ($1, $2, 'test-proc', '1.0.0', 'ph1', $3, 'full-rag-v1', true)
		RETURNING id::text`, attID, "sha256:"+key, docID).Scan(&snapID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	for i := 0; i < nChunks; i++ {
		var chunkID string
		if err := h.pool.QueryRow(ctx, `
			INSERT INTO processing_chunks (snapshot_id, chunk_index, text, locator, token_count)
			VALUES ($1, $2, $3, '{"type":"page_span"}'::jsonb, 10)
			RETURNING id::text`, snapID, i, "chunk text "+key).Scan(&chunkID); err != nil {
			t.Fatalf("seed chunk %d: %v", i, err)
		}
		if _, err := h.pool.Exec(ctx, `
			INSERT INTO processing_chunk_dense_embeddings (chunk_id, model, dimensions, vector)
			VALUES ($1, 'test-bge', 3, $2)`, chunkID, "[0.1,0.2,0.3]"); err != nil {
			t.Fatalf("seed embedding %d: %v", i, err)
		}
	}
	var rowID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO opensearch_outbox (snapshot_id, operation, payload, attempts)
		VALUES ($1, 'index', $2::jsonb, $3) RETURNING id::text`,
		snapID, `{"document_id":"`+docID+`","attachment_id":"`+attID+`"}`, attempts).Scan(&rowID); err != nil {
		t.Fatalf("seed outbox row: %v", err)
	}
	return rowID, snapID
}

func outboxRowStatus(t *testing.T, h *dispatchHarness, rowID string) (status string, attempts int, next time.Time, lastErr *string) {
	t.Helper()
	err := h.pool.QueryRow(context.Background(),
		`SELECT status, attempts, next_attempt_at, last_error FROM opensearch_outbox WHERE id=$1`,
		rowID).Scan(&status, &attempts, &next, &lastErr)
	if err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	return
}

func newOutboxDispatcher(h *dispatchHarness) *Dispatcher {
	return New(h.rep, nil, Config{}, log.New(&strings.Builder{}, "test: ", 0))
}

// fakeOS returns an httptest server recording indexed docs. failAll makes
// every non-HEAD request return 500.
func fakeOS(t *testing.T, failAll bool) (*httptest.Server, *osRecorder) {
	t.Helper()
	rec := &osRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if failAll {
				w.WriteHeader(500)
				return
			}
			w.WriteHeader(404) // index not yet created
			return
		}
		if failAll {
			w.WriteHeader(500)
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/axiom-ng-chunks-v1/_doc/") {
			var doc map[string]any
			_ = json.NewDecoder(r.Body).Decode(&doc)
			// Mirror the REAL OpenSearch rule that bit us in the reality test:
			// metadata fields in the body are a 400 mapper_parsing_exception.
			if _, ok := doc["_id"]; ok {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"type":"mapper_parsing_exception","reason":"Field [_id] is a metadata field"}}`))
				return
			}
			rec.mu.Lock()
			rec.docs = append(rec.docs, doc)
			rec.paths = append(rec.paths, r.URL.Path)
			rec.mu.Unlock()
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"result":"created"}`))
			return
		}
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/axiom-ng-chunks-v1/_doc/") {
			rec.mu.Lock()
			rec.deletes = append(rec.deletes, strings.TrimPrefix(r.URL.Path, "/axiom-ng-chunks-v1/_doc/"))
			rec.mu.Unlock()
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"result":"deleted"}`))
			return
		}
		if r.Method == http.MethodPut { // index create
			b, _ := io.ReadAll(r.Body)
			rec.mu.Lock()
			rec.createdIndex = true
			rec.createBody = b
			rec.mu.Unlock()
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
			return
		}
		w.WriteHeader(400)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

type osRecorder struct {
	mu           sync.Mutex
	docs         []map[string]any
	paths        []string
	deletes      []string
	createdIndex bool
	createBody   []byte
}

func (r *osRecorder) deletedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.deletes))
	copy(out, r.deletes)
	return out
}

func (r *osRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.docs)
}

func (r *osRecorder) indexCreateBody() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createBody
}

func TestOutboxBackoffShape(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{20, time.Hour},
	}
	for _, c := range cases {
		if got := outboxBackoff(c.attempts); got != c.want {
			t.Errorf("outboxBackoff(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
	for n := 1; n <= 100; n++ {
		if got := outboxBackoff(n); got > time.Hour {
			t.Fatalf("outboxBackoff(%d) = %v exceeds the 1h cap", n, got)
		}
	}
}

func TestOutboxDrainedToDone(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	rowID, snapID := h.seedOutboxSnapshot(t, "done-key", 3, 0)

	srv, rec := fakeOS(t, false)
	d := newOutboxDispatcher(h)
	ix := newOpenSearchClient(srv.URL, "", "", nil)
	if err := drainOutboxOnce(context.Background(), d, ix); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// All 3 chunks indexed with chunk-id doc ids (in the URL path) and
	// 3-dim embeddings.
	if got := rec.count(); got != 3 {
		t.Fatalf("indexed docs = %d, want 3", got)
	}
	first := rec.docs[0]
	if first["snapshot_id"] != snapID {
		t.Fatalf("doc snapshot_id = %v, want %s", first["snapshot_id"], snapID)
	}
	// Doc id travels in the URL (metadata field), body carries chunk_id.
	if !strings.HasSuffix(rec.paths[0], first["chunk_id"].(string)) {
		t.Fatalf("doc URL %s does not end with chunk id %v", rec.paths[0], first["chunk_id"])
	}
	if _, hasID := first["_id"]; hasID {
		t.Fatal("_id must not appear in the document body (metadata field)")
	}
	emb, ok := first["embedding"].([]any)
	if !ok || len(emb) != 3 {
		t.Fatalf("doc embedding = %v, want 3-dim", first["embedding"])
	}
	if !rec.createdIndex {
		t.Fatal("index was not ensured")
	}
	// The index was CREATED knn-first with the seeded 3-dim dimension — the
	// create body must pin both, or the first doc auto-creates plain float.
	var create struct {
		Settings struct {
			Index struct {
				Knn bool `json:"knn"`
			} `json:"index"`
		} `json:"settings"`
		Mappings struct {
			Properties struct {
				Embedding struct {
					Type      string `json:"type"`
					Dimension int    `json:"dimension"`
				} `json:"embedding"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(rec.indexCreateBody(), &create); err != nil {
		t.Fatalf("index create body: %v", err)
	}
	if !create.Settings.Index.Knn {
		t.Fatal("index create body must set settings.index.knn = true")
	}
	if create.Mappings.Properties.Embedding.Type != "knn_vector" {
		t.Fatalf("embedding mapping type = %q, want knn_vector", create.Mappings.Properties.Embedding.Type)
	}
	if create.Mappings.Properties.Embedding.Dimension != 3 {
		t.Fatalf("embedding mapping dimension = %d, want 3 (from the batch dim-peek)", create.Mappings.Properties.Embedding.Dimension)
	}

	status, attempts, _, _ := outboxRowStatus(t, h, rowID)
	if status != "done" || attempts != 0 {
		t.Fatalf("row = %s/%d, want done/0", status, attempts)
	}

	// Second drain: nothing pending left, no duplicate docs.
	if err := drainOutboxOnce(context.Background(), d, ix); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if got := rec.count(); got != 3 {
		t.Fatalf("docs after second drain = %d, want 3 (no duplicates)", got)
	}
}

func TestOutboxClaimExclusivity(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	const total = 8
	rowIDs := make([]string, total)
	for i := 0; i < total; i++ {
		rowIDs[i], _ = h.seedOutboxSnapshot(t, fmt.Sprintf("excl-key-%04d", i), 1, 0)
	}

	// 4 concurrent claimers over the same rows; SKIP LOCKED must hand each
	// row to exactly one.
	const workers = 4
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := h.rep.ClaimOutboxBatch(context.Background(), 64, outboxClaimVisibility)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			for _, r := range rows {
				claimed[r.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(claimed) != total {
		t.Fatalf("distinct claimed rows = %d, want %d", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("row %s claimed %d times, want exactly 1 (double-drain)", id, n)
		}
	}
}

func TestOutboxRetryBackoffOnError(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	rowID, _ := h.seedOutboxSnapshot(t, "backoff-key", 2, 0)

	srv, _ := fakeOS(t, true) // every request 500
	d := newOutboxDispatcher(h)
	ix := newOpenSearchClient(srv.URL, "", "", nil)
	if err := drainOutboxOnce(context.Background(), d, ix); err != nil {
		t.Fatalf("drain: %v", err)
	}

	status, attempts, next, lastErr := outboxRowStatus(t, h, rowID)
	if status != "pending" {
		t.Fatalf("status = %s, want pending", status)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if next.Before(time.Now()) {
		t.Fatalf("next_attempt_at %v is not in the future", next)
	}
	if lastErr == nil || !strings.Contains(*lastErr, "HTTP 500") {
		t.Fatalf("last_error = %v, want HTTP 500 detail", lastErr)
	}
}

func TestOutboxFailedAfterMaxAttempts(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	// attempts already at max-1 → this failure tips it terminal.
	rowID, snapID := h.seedOutboxSnapshot(t, "max-key", 2, outboxMaxAttempts-1)

	var chunksBefore int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM processing_chunks WHERE snapshot_id=$1`, snapID).Scan(&chunksBefore); err != nil {
		t.Fatalf("count before: %v", err)
	}

	srv, _ := fakeOS(t, true)
	d := newOutboxDispatcher(h)
	ix := newOpenSearchClient(srv.URL, "", "", nil)
	if err := drainOutboxOnce(context.Background(), d, ix); err != nil {
		t.Fatalf("drain: %v", err)
	}

	status, attempts, _, _ := outboxRowStatus(t, h, rowID)
	if status != "failed" {
		t.Fatalf("status = %s, want failed after %d attempts", status, outboxMaxAttempts)
	}
	if attempts != outboxMaxAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, outboxMaxAttempts)
	}

	// Snapshot rows untouched by the failure.
	var chunksAfter int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM processing_chunks WHERE snapshot_id=$1`, snapID).Scan(&chunksAfter); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if chunksAfter != chunksBefore {
		t.Fatalf("snapshot chunks changed: %d -> %d (outbox failure must not touch snapshots)", chunksBefore, chunksAfter)
	}
}

func TestOutboxStaleFailDoesNotFlipDone(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	rowID, _ := h.seedOutboxSnapshot(t, "doneflip-key", 1, 0)

	// Worker A claims and finishes the row.
	rows, err := h.rep.ClaimOutboxBatch(context.Background(), 1, outboxClaimVisibility)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(rows))
	}
	if err := h.rep.MarkOutboxDone(context.Background(), rows[0].ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// Worker B (stale — its claim's visibility window has since become
	// irrelevant) drives the failure path against a failing OpenSearch.
	// The status='pending' guard in FailOutboxAttempt must make this a
	// no-op: the done row stays done with attempts unchanged.
	srv, _ := fakeOS(t, true)
	d := newOutboxDispatcher(h)
	stale := rows[0]
	if err := drainOutboxRow(context.Background(), d, newOpenSearchClient(srv.URL, "", "", nil), stale); err == nil {
		t.Fatal("expected the stale drain attempt to report its error")
	}

	status, attempts, _, _ := outboxRowStatus(t, h, rowID)
	if status != "done" || attempts != 0 {
		t.Fatalf("row = %s/%d, want done/0 (stale failure must not flip a done row)", status, attempts)
	}
}

func TestOutboxWarnsWhenExistingIndexStrandsKnn(t *testing.T) {
	// Index already exists (HEAD 200) with a plain-float embedding mapping —
	// the auto-create trap. ensureIndex must log a loud warning naming the
	// fix, and only once per client.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(200)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_mapping"):
			// Real OpenSearch response shape: wrapped under the index name.
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"axiom-ng-chunks-v1":{"mappings":{"properties":{"embedding":{"type":"float"}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/_mapping"):
			// R5: additive sparse rank_features mapping (idempotent).
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(srv.Close)

	buf := &strings.Builder{}
	osc := newOpenSearchClient(srv.URL, "", "", log.New(buf, "", 0))
	if err := osc.ensureIndex(context.Background(), 3); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !strings.Contains(buf.String(), "knn_vector") {
		t.Fatalf("expected stranded-knn WARNING in log, got %q", buf.String())
	}

	// Memoized: the second ensure (same client) must not re-check.
	buf.Reset()
	if err := osc.ensureIndex(context.Background(), 3); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("mapping check must run once per client, second run logged %q", buf.String())
	}
}

func TestOutboxWorkerDisabledLeavesRowsPending(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	rowID, _ := h.seedOutboxSnapshot(t, "disabled-key", 1, 0)

	// Real Run() startup path with a live (fake) processor and an EMPTY
	// OpenSearch URL: the outbox worker must never start, and a pending row
	// must survive multiple outbox poll ticks untouched.
	fp := newFakeProcessor(t)
	c := Config{
		Concurrency: 1, LeaseDuration: 5 * time.Minute,
		RenewalInterval: 25 * time.Millisecond, PollInterval: 15 * time.Millisecond,
		AckRetryInterval: 100 * time.Millisecond,
		OpenSearchURL:    "", // disabled
	}
	d := New(h.rep, mustClient(t, fp.url()), c, log.New(io.Discard, "", 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() { errCh <- d.Run(ctx); close(done) }()
	// ~2 outbox poll ticks (poll interval is 2s): enough for a wrongly
	// started worker to have claimed and drained the row twice over.
	select {
	case err := <-errCh:
		t.Fatalf("Run errored with the outbox worker disabled: %v", err)
	case <-time.After(5 * time.Second):
	}
	cancel()
	<-done

	status, attempts, _, _ := outboxRowStatus(t, h, rowID)
	if status != "pending" || attempts != 0 {
		t.Fatalf("row = %s/%d, want pending/0 (disabled worker must leave rows untouched)", status, attempts)
	}
}

// TestOutboxTombstoneRoundtrip pins #127 (Option A): persisting generation B
// over active A plans a tombstone for A; draining leaves ONLY B's chunks in
// the index; a later replay-REACTIVATION of A re-indexes A and tombstones B.
// The obsolete-op guards make stale operations no-ops.
func TestOutboxTombstoneRoundtrip(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	ctx := context.Background()
	srv, rec := fakeOS(t, false)
	osc := newOpenSearchClient(srv.URL, "", "", nil)
	d := newOutboxDispatcher(h)

	// Helper: drive one generation end-to-end through the REAL persist path
	// (claim → processing → PersistResult). force=true seeds a second job so
	// the claim freezes a different profile hash (the #125/#127 trigger).
	// attRefs: the SHARED attachment identity both generations process.
	var attID, srcID, docID, chRef string
	drive := func(key string, nChunks int, force bool) (jobID, snapID string) {
		t.Helper()
		if force {
			// B re-processes A's attachment via a force job (different profile
			// hash): the sibling/deactivation path only exists on a SHARED
			// attachment.
			if _, err := h.pool.Exec(ctx, `
				INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, force_rebuild)
				VALUES ($1,$2,$3,$4,'pending',true)`, srcID, docID, attID, chRef); err != nil {
				t.Fatalf("force job: %v", err)
			}
			jobID = h.seededPendingForceID(t, attID)
		} else {
			jobID = h.seedJob(t, key, 3)
			if err := h.pool.QueryRow(ctx,
				`SELECT attachment_id::text, source_id::text, document_id::text, content_hash FROM ingest_jobs WHERE id=$1`, jobID,
			).Scan(&attID, &srcID, &docID, &chRef); err != nil {
				t.Fatalf("refs: %v", err)
			}
		}
		cj, err := h.rep.ClaimNextJob(ctx, repo.ClaimOptions{
			WorkerID: "w-rt-" + key, LeaseDuration: time.Minute,
			Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
		})
		if err != nil || cj == nil {
			t.Fatalf("claim %s: %v %v", key, cj, err)
		}
		if err := h.rep.MarkProcessing(ctx, cj.LeaseRef); err != nil {
			t.Fatalf("mark processing: %v", err)
		}
		snapID, err = h.rep.PersistResult(ctx, cj.LeaseRef.JobID, []byte(rtResult(cj.LeaseRef.JobID, attID, chRef, nChunks)), repo.PersistOptions{CapDim: 3})
		if err != nil {
			t.Fatalf("persist %s: %v", key, err)
		}
		return jobID, snapID
	}

	chunkIDs := func(snapID string) []string {
		rows, err := h.pool.Query(ctx, `SELECT id::text FROM processing_chunks WHERE snapshot_id=$1 ORDER BY chunk_index`, snapID)
		if err != nil {
			t.Fatalf("chunk ids: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			out = append(out, id)
		}
		return out
	}

	// A: 2 chunks, active, indexed.
	_, snapA := drive("rtA", 2, false)
	if err := drainOutboxOnce(ctx, d, osc); err != nil {
		t.Fatalf("drain A: %v", err)
	}
	if got := rec.count(); got != 2 {
		t.Fatalf("after A: indexed = %d, want 2", got)
	}

	// B (force, different profile): 3 chunks; A must be tombstoned.
	_, snapB := drive("rtB", 3, true)
	if len(chunkIDs(snapB)) != 3 {
		t.Fatalf("B chunks = %d, want 3", len(chunkIDs(snapB)))
	}
	if err := drainOutboxOnce(ctx, d, osc); err != nil {
		t.Fatalf("drain B: %v", err)
	}
	del := rec.deletedIDs()
	want := chunkIDs(snapA)
	asSet := func(in []string) string {
		out := slices.Clone(in)
		slices.Sort(out)
		return strings.Join(out, ",")
	}
	if asSet(del) != asSet(want) {
		t.Fatalf("after B: deletes = %v, want exactly A's chunk ids %v", del, want)
	}

	// Reactivation of A: replay-persist identity A once more (duplicate-persist
	// convention; the replay branch never touches the job row).
	var attA, chA, jobA string
	if err := h.pool.QueryRow(ctx,
		`SELECT j.attachment_id::text, j.content_hash, j.id::text FROM ingest_jobs j
		 JOIN processing_snapshots s ON s.ingest_job_id = j.id
		 WHERE s.id=$1`, snapA).Scan(&attA, &chA, &jobA); err != nil {
		t.Fatalf("A job refs: %v", err)
	}
	reactivated, err := h.rep.PersistResult(ctx, jobA, []byte(rtResult(jobA, attA, chA, 2)), repo.PersistOptions{CapDim: 3})
	if err != nil {
		t.Fatalf("replay A: %v", err)
	}
	if reactivated != snapA {
		t.Fatalf("replay must return A's snapshot %s, got %s", snapA, reactivated)
	}
	if err := drainOutboxOnce(ctx, d, osc); err != nil {
		t.Fatalf("drain reactivate: %v", err)
	}

	// Net index state: A's 2 docs live; A's and B's docs each deleted exactly once.
	netDocs := rec.count() - len(rec.deletedIDs())
	if netDocs != 2 {
		t.Fatalf("net docs = %d, want 2 (only A's chunks)", netDocs)
	}
	var activeSnap string
	if err := h.pool.QueryRow(ctx,
		`SELECT id::text FROM processing_snapshots WHERE attachment_id=(SELECT attachment_id FROM processing_snapshots WHERE id=$1) AND active`,
		snapA).Scan(&activeSnap); err != nil {
		t.Fatalf("active: %v", err)
	}
	if activeSnap != snapA {
		t.Fatalf("active snapshot = %s, want reactivated A", activeSnap)
	}
}

// seededPendingForceID returns the force job's id for an attachment.
func (h *dispatchHarness) seededPendingForceID(t *testing.T, attID string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id::text FROM ingest_jobs WHERE attachment_id=$1 AND force_rebuild ORDER BY enqueued_at DESC LIMIT 1`, attID).Scan(&id); err != nil {
		t.Fatalf("force id: %v", err)
	}
	return id
}

// rtResult builds a §14-valid processor result with n chunks (dim 3).
func rtResult(jobID, attID, contentHash string, n int) string {
	chunks := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			chunks += ","
		}
		chunks += `{"ref":"chunk-000` + string(rune('0'+i)) + `","index":` + string(rune('0'+i)) +
			`,"text":"roundtrip chunk ` + string(rune('0'+i)) + `",` +
			`"locator":{"type":"page_span","physical_page_start":0,"physical_page_end":0,"page_label_start":"1","page_label_end":"1","source":"marker_paginate"},` +
			`"structure":{"section_titles":["RT"],"start_paragraph_index":0,"end_paragraph_index":0},"token_count":3,` +
			`"embeddings":{"dense":{"model":"fake-bge","dimensions":3,"values":[0.1,0.2,0.3]}}}`
	}
	return `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed",` +
		`"source":{"attachment_id":"` + attID + `","content_hash":"` + contentHash + `","verified":true},` +
		`"processor":{"name":"fake","version":"0.1.0","profile":"full-rag-v1","profile_hash":"unused-fallback","models":{"dense_embedding":"fake-bge"}},` +
		`"artifacts":[],"chunks":[` + chunks + `],"entities":[],"chunk_relationships":[],"entity_relationships":[],` +
		`"stats":{"pages":0,"chunks":` + string(rune('0'+n)) + `,"artifacts":0,"entities":0,"entity_relationships":0,"chunk_relationships":0},"warnings":[]}`
}

// TestOutboxObsoleteDeleteSkipped pins the #127 order-insensitivity guard: a
// tombstone whose snapshot is ACTIVE again (reactivated after the tombstone
// was planned, before it drained — e.g. under backoff reordering) must be
// marked done WITHOUT deleting the live generation's docs.
func TestOutboxObsoleteDeleteSkipped(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	_, snapID := h.seedOutboxSnapshot(t, "obs-del", 2, 0)
	// Seed an out-of-band delete row for the (still active) snapshot.
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO opensearch_outbox (snapshot_id, operation, payload)
		VALUES ($1, 'delete', '{"operation":"delete"}'::jsonb)`, snapID); err != nil {
		t.Fatalf("seed delete row: %v", err)
	}

	srv, rec := fakeOS(t, false)
	d := newOutboxDispatcher(h)
	osc := newOpenSearchClient(srv.URL, "", "", nil)
	if err := drainOutboxOnce(context.Background(), d, osc); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if got := len(rec.deletedIDs()); got != 0 {
		t.Fatalf("deletes = %v, want none (snapshot is active — obsolete op)", rec.deletedIDs())
	}
	// The obsolete row is done, the live index row indexed its 2 chunks.
	var done, pending int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE status='done'), count(*) FILTER (WHERE status='pending')
		 FROM opensearch_outbox WHERE operation='delete'`).Scan(&done, &pending); err != nil {
		t.Fatalf("row states: %v", err)
	}
	if done != 1 || pending != 0 {
		t.Fatalf("delete row done=%d pending=%d, want 1/0", done, pending)
	}
}

// TestOutboxObsoleteIndexSkipped pins the OTHER #127 order-insensitivity
// guard: an index op whose snapshot was superseded before it drained (e.g.
// backoff reordering left it behind a newer generation's tombstone) must be
// marked done WITHOUT resurrecting the stale generation's docs. Together
// with TestOutboxObsoleteDeleteSkipped this makes draining order-insensitive
// in both directions.
func TestOutboxObsoleteIndexSkipped(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	ctx := context.Background()
	_, snapID := h.seedOutboxSnapshot(t, "obs-idx", 2, 0)
	// Supersede it before the drain: the pending index row is now obsolete.
	if _, err := h.pool.Exec(ctx,
		`UPDATE processing_snapshots SET active=false WHERE id=$1`, snapID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	srv, rec := fakeOS(t, false)
	d := newOutboxDispatcher(h)
	osc := newOpenSearchClient(srv.URL, "", "", nil)
	if err := drainOutboxOnce(ctx, d, osc); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if got := rec.count(); got != 0 {
		t.Fatalf("indexed %d docs, want 0 (snapshot superseded — obsolete op must not resurrect it)", got)
	}
	var done, pending int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status='done'), count(*) FILTER (WHERE status='pending')
		 FROM opensearch_outbox WHERE operation='index' AND snapshot_id=$1`, snapID).Scan(&done, &pending); err != nil {
		t.Fatalf("row states: %v", err)
	}
	if done != 1 || pending != 0 {
		t.Fatalf("index row done=%d pending=%d, want 1/0", done, pending)
	}
}

// R5 review pins: the sparse field joins a doc only when present, and a
// failed additive mapping PUT leaves ensured=false so the next ensureIndex
// retries it (a mixed-version create race must not strand the index without
// rank_features).
func TestOutboxDocumentSparseOnlyWhenPresent(t *testing.T) {
	row := repo.OutboxRow{SnapshotID: "s", Payload: map[string]any{"document_id": "d"}}
	doc := repo.OutboxDoc{ChunkID: "c", Text: "t"}
	if d := outboxDocument(row, doc); d["sparse"] != nil {
		t.Fatalf("nil sparse must stay absent (null fights rank_features): %v", d["sparse"])
	}
	doc.Sparse = map[string]float64{"12": 0.5}
	d := outboxDocument(row, doc)
	got, ok := d["sparse"].(map[string]float64)
	if !ok || got["12"] != 0.5 {
		t.Fatalf("present sparse must map through exactly: %v", d["sparse"])
	}
}

func TestOutboxEnsureSparseMappingRetriedAfterFailure(t *testing.T) {
	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(200) // index already exists
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_mapping"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"axiom-ng-chunks-v1":{"mappings":{"properties":{"embedding":{"type":"knn_vector"}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/_mapping"):
			puts++
			if puts == 1 {
				w.WriteHeader(500) // transient mapping failure
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(srv.Close)

	osc := newOpenSearchClient(srv.URL, "", "", log.New(io.Discard, "", 0))
	if err := osc.ensureIndex(context.Background(), 3); err == nil {
		t.Fatal("first ensure must fail (mapping PUT 500)")
	}
	if osc.ensured {
		t.Fatal("ensured must stay false after a failed mapping PUT")
	}
	if err := osc.ensureIndex(context.Background(), 3); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !osc.ensured {
		t.Fatal("ensured must flip after the retried mapping PUT")
	}
	if puts != 2 {
		t.Fatalf("mapping PUT must retry exactly once more, got %d", puts)
	}
}
