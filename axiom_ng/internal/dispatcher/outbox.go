package dispatcher

// L5: OpenSearch outbox drainer. A completely separate path from snapshot
// persistence — no OpenSearch call ever runs inside a snapshot transaction
// (work order §10.3). An OpenSearch outage only ever touches outbox rows:
// attempts/backoff on the row, terminal 'failed' after max attempts; the
// snapshot and the ingest job stay untouched.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

const (
	outboxIndexName    = "axiom-ng-chunks-v1"
	outboxBatchSize    = 64
	outboxPollInterval = 2 * time.Second
	outboxMaxAttempts  = 10
	outboxBackoffBase  = 5 * time.Second
	outboxBackoffCap   = 1 * time.Hour
	// claimVisibility pushes next_attempt_at forward while a row is being
	// worked so a crashed drainer's rows reappear for retry.
	outboxClaimVisibility = 10 * time.Minute
)

// outboxBackoff returns the capped exponential retry delay for attempt n
// (1-based): 5s, 10s, 20s ... capped at 1h.
func outboxBackoff(attempts int) time.Duration {
	d := outboxBackoffBase
	for i := 1; i < attempts && d < outboxBackoffCap; i++ {
		d *= 2
	}
	if d > outboxBackoffCap {
		d = outboxBackoffCap
	}
	return d
}

// openSearchClient is a minimal indexing client: PUT /{index}/_doc/{id} per
// chunk (idempotent — doc id = chunk id). Auth is optional basic; the local
// mothership runs anonymous.
type openSearchClient struct {
	baseURL  string
	username string
	password string
	hc       *http.Client
	index    string
	ensured  bool // index existence verified once per client
}

func newOpenSearchClient(baseURL, username, password string) *openSearchClient {
	return &openSearchClient{
		baseURL:  baseURL,
		username: username,
		password: password,
		hc:       &http.Client{Timeout: 30 * time.Second},
		index:    outboxIndexName,
	}
}

func (c *openSearchClient) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// ensureIndex creates the index with a knn_vector mapping on first use.
// Dimension comes from the first embedding seen (1024 real / 3 reference).
// A concurrent create racing another instance only loses to a 400
// "already exists" — treated as success.
func (c *openSearchClient) ensureIndex(ctx context.Context, dim int) error {
	if c.ensured {
		return nil
	}
	if code, _, err := c.do(ctx, http.MethodHead, "/"+c.index, nil); err != nil {
		return err
	} else if code == http.StatusOK {
		c.ensured = true
		return nil
	}
	mapping := map[string]any{
		"settings": map[string]any{"index": map[string]any{"knn": true}},
		"mappings": map[string]any{
			"properties": map[string]any{
				"embedding": map[string]any{"type": "knn_vector", "dimension": dim},
				"text":      map[string]any{"type": "text"},
			},
		},
	}
	body, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	code, respBody, err := c.do(ctx, http.MethodPut, "/"+c.index, body)
	if err != nil {
		return err
	}
	if code >= 200 && code < 300 {
		c.ensured = true
		return nil
	}
	// Lost a create race or index already exists.
	if code == http.StatusBadRequest && bytes.Contains(respBody, []byte("already exists")) {
		c.ensured = true
		return nil
	}
	return fmt.Errorf("create index %s: HTTP %d: %s", c.index, code, truncate(respBody, 200))
}

// indexDoc PUTs one chunk document. The id goes in the URL only — OpenSearch
// metadata fields must never appear in the body (mapper_parsing_exception).
func (c *openSearchClient) indexDoc(ctx context.Context, id string, doc map[string]any) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	code, respBody, err := c.do(ctx, http.MethodPut, "/"+c.index+"/_doc/"+id, body)
	if err != nil {
		return err
	}
	if code >= 200 && code < 300 {
		return nil
	}
	return fmt.Errorf("index doc %s: HTTP %d: %s", id, code, truncate(respBody, 200))
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// outboxIndexer abstracts the OpenSearch side so tests can inject a fake.
type outboxIndexer interface {
	ensureIndex(ctx context.Context, dim int) error
	indexDoc(ctx context.Context, id string, doc map[string]any) error
}

// outboxWorker polls the outbox until ctx is cancelled. Disabled (empty URL)
// never starts — see Run.
func outboxWorker(ctx context.Context, d *Dispatcher, ix outboxIndexer) {
	ticker := time.NewTicker(outboxPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := drainOutboxOnce(ctx, d, ix); err != nil && ctx.Err() == nil {
				d.logger.Printf("outbox drain: %v", err)
			}
		}
	}
}

// drainOutboxOnce claims one batch and indexes it. Per-row outcome:
// success → done; error → attempts+1 + backoff (terminal failed at max).
// A row's failure never stops the rest of the batch.
func drainOutboxOnce(ctx context.Context, d *Dispatcher, ix outboxIndexer) error {
	rows, err := d.rep.ClaimOutboxBatch(ctx, outboxBatchSize, outboxClaimVisibility)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	// Ensure the index with the right knn dimension BEFORE any doc lands:
	// the first indexed doc must not auto-create a plain-float mapping. The
	// dimension comes from the DB (dimensions column) across the claimed
	// snapshots; batches without embeddings skip this (no knn field needed
	// yet — a later ensure on an embedding row upgrades nothing, but exact
	// search still works and a reindex command fixes the mapping if that
	// ordering ever matters).
	snapIDs := make([]string, len(rows))
	for i, r := range rows {
		snapIDs[i] = r.SnapshotID
	}
	if dim, derr := d.rep.OutboxFirstEmbeddingDim(ctx, snapIDs); derr != nil {
		d.logger.Printf("outbox dim peek: %v", derr)
	} else if dim > 0 {
		if err := ix.ensureIndex(ctx, dim); err != nil {
			// Not fatal for rows without embeddings; rows with embeddings will
			// surface the error per-row below via their own ensureIndex call.
			d.logger.Printf("outbox ensure index (dim=%d): %v", dim, err)
		}
	}
	for _, row := range rows {
		if err := drainOutboxRow(ctx, d, ix, row); err != nil && ctx.Err() == nil {
			d.logger.Printf("outbox row %s: %v", row.ID, err)
		}
	}
	return nil
}

func drainOutboxRow(ctx context.Context, d *Dispatcher, ix outboxIndexer, row repo.OutboxRow) error {
	docs, err := d.rep.OutboxDocs(ctx, row.SnapshotID)
	if err != nil {
		return d.failOutboxRow(ctx, row, fmt.Errorf("load docs: %w", err))
	}
	// Ensure the index (with knn dimension from the first embedding seen)
	// before the first write; memoized on the client after that.
	for _, doc := range docs {
		if doc.Embedding != nil {
			if err := ix.ensureIndex(ctx, len(doc.Embedding)); err != nil {
				return d.failOutboxRow(ctx, row, fmt.Errorf("ensure index: %w", err))
			}
			break
		}
	}
	for _, doc := range docs {
		if err := ix.indexDoc(ctx, doc.ChunkID, outboxDocument(row, doc)); err != nil {
			return d.failOutboxRow(ctx, row, err)
		}
	}
	return d.rep.MarkOutboxDone(ctx, row.ID)
}

// outboxDocument builds the indexable chunk body. The document id is the
// durable chunk UUID (passed separately — OpenSearch metadata field, never
// in the body), so re-indexing overwrites instead of duplicating.
func outboxDocument(row repo.OutboxRow, doc repo.OutboxDoc) map[string]any {
	return map[string]any{
		"chunk_id":       doc.ChunkID,
		"chunk_ref":      doc.ChunkRef,
		"snapshot_id":    row.SnapshotID,
		"document_id":    row.OutboxDocumentID(),
		"attachment_id":  row.OutboxAttachmentID(),
		"chunk_index":    doc.Index,
		"text":           doc.Text,
		"locator":        doc.Locator,
		"section_titles": doc.Sections,
		"token_count":    doc.Tokens,
		"embedding":      doc.Embedding, // nil → JSON null when absent
	}
}

// failOutboxRow records the failure with capped exponential backoff.
func (d *Dispatcher) failOutboxRow(ctx context.Context, row repo.OutboxRow, cause error) error {
	err := d.rep.FailOutboxAttempt(ctx, row.ID, cause.Error(), outboxBackoff(row.Attempts+1), outboxMaxAttempts)
	if err != nil {
		return fmt.Errorf("record failure: %v (cause: %w)", err, cause)
	}
	return cause
}
