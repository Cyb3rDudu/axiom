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
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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
	createdIndex bool
	createBody   []byte
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
