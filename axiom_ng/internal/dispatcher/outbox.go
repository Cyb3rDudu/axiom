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
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5"
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
	logger   *log.Logger
	ensured  bool // index existence verified once per client
}

func newOpenSearchClient(baseURL, username, password string, logger *log.Logger) *openSearchClient {
	if logger == nil {
		logger = log.Default()
	}
	return &openSearchClient{
		baseURL:  baseURL,
		username: username,
		password: password,
		hc:       &http.Client{Timeout: 30 * time.Second},
		index:    outboxIndexName,
		logger:   logger,
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
		// R5: an index created before sparse existed lacks the rank_features
		// mapping; the first sparse-carrying doc would then hit dynamic mapping
		// (wrong type, silent). Add the field additively — idempotent PUT.
		// ensured flips only on success so a failed mapping PUT retries.
		if err := c.ensureSparseMapping(ctx); err != nil {
			return err
		}
		if err := c.ensureCaptionMapping(ctx); err != nil {
			return err
		}
		c.ensured = true
		c.warnIfStrandedKnn(ctx)
		return nil
	}
	mapping := map[string]any{
		"settings": map[string]any{"index": map[string]any{"knn": true}},
		"mappings": map[string]any{
			"properties": map[string]any{
				"embedding": map[string]any{"type": "knn_vector", "dimension": dim},
				"text":      map[string]any{"type": "text"},
				// Learned lexical weights {token: weight} (R5 #135).
				"sparse": map[string]any{"type": "rank_features"},
				// Machine image captions, BM25 arm (#230) — in the CREATE
				// mapping too: a fresh index must never rely on dynamic
				// mapping for it (round-2 review; the additive
				// ensureCaptionMapping covers pre-#230 indexes).
				"caption_text": map[string]any{"type": "text"},
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
	// Lost a create race or index already exists. The winner may predate
	// sparse (R5): the additive rank_features mapping is required here too,
	// and ensured flips only on success so a failed mapping PUT retries.
	if code == http.StatusBadRequest && bytes.Contains(respBody, []byte("already exists")) {
		if err := c.ensureSparseMapping(ctx); err != nil {
			return err
		}
		if err := c.ensureCaptionMapping(ctx); err != nil {
			return err
		}
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

// deleteDoc removes a chunk document. 404 counts as success (idempotent
// tombstone: the doc may never have been indexed).
func (c *openSearchClient) deleteDoc(ctx context.Context, id string) error {
	code, _, err := c.do(ctx, http.MethodDelete, "/"+outboxIndexName+"/_doc/"+id, nil)
	if err != nil {
		return err
	}
	if code != 200 && code != 404 {
		return fmt.Errorf("delete %s: status %d", id, code)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// warnIfStrandedKnn runs once per client (ensureIndex short-circuits on the
// `ensured` flag after the HEAD-200 branch). An index that already exists
// with a plain-float embedding mapping — auto-created by a doc that landed
// before any knn-first ensure — silently degrades every embedding search to
// exact scoring. The only fix is delete + reindex with the right mapping, so
// make the condition loud instead of leaving the operator guessing why k-NN
// returns nothing. Non-2xx or malformed mapping responses are not fatal
// (ensure stays satisfied); no warning means knn or no embedding field yet.
func (c *openSearchClient) warnIfStrandedKnn(ctx context.Context) {
	code, body, err := c.do(ctx, http.MethodGet, "/"+c.index+"/_mapping", nil)
	if err != nil || code < 200 || code >= 300 {
		return
	}
	// Real response shape: {"<index>":{"mappings":{"properties":{...}}}}.
	var idx map[string]struct {
		Mappings struct {
			Properties struct {
				Embedding struct {
					Type string `json:"type"`
				} `json:"embedding"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if json.Unmarshal(body, &idx) != nil {
		return
	}
	for _, m := range idx {
		if t := m.Mappings.Properties.Embedding.Type; t != "" && t != "knn_vector" {
			c.logger.Printf("WARNING: OpenSearch index %s maps embedding as %q, not knn_vector — k-NN search is silently degraded; delete the index so it re-creates knn-first, then requeue the affected outbox rows", c.index, t)
		}
		return // single index queried
	}
}

// outboxWorker polls the outbox until ctx is cancelled. Disabled (empty URL)
// never starts — see Run.
func outboxWorker(ctx context.Context, d *Dispatcher, osc *openSearchClient) {
	ticker := time.NewTicker(outboxPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := drainOutboxOnce(ctx, d, osc); err != nil && ctx.Err() == nil {
				d.logger.Printf("outbox drain: %v", err)
			}
		}
	}
}

// drainOutboxOnce claims one batch and indexes it. Per-row outcome:
// success → done; error → attempts+1 + backoff (terminal failed at max).
// A row's failure never stops the rest of the batch.
func drainOutboxOnce(ctx context.Context, d *Dispatcher, osc *openSearchClient) error {
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
		if err := osc.ensureIndex(ctx, dim); err != nil {
			// Not fatal for rows without embeddings; rows with embeddings will
			// surface the error per-row below via their own ensureIndex call.
			d.logger.Printf("outbox ensure index (dim=%d): %v", dim, err)
		}
	}
	for _, row := range rows {
		if err := drainOutboxRow(ctx, d, osc, row); err != nil && ctx.Err() == nil {
			d.logger.Printf("outbox row %s: %v", row.ID, err)
		}
	}
	// #167: observe a drained batch; observer-only noise on the quiet path.
	d.publish(events.OutboxDrained{Count: len(rows)})
	return nil
}

func drainOutboxRow(ctx context.Context, d *Dispatcher, osc *openSearchClient, row repo.OutboxRow) error {
	// #127 obsolete-op guards: only CURRENT state materializes. A delete for
	// a since-reactivated snapshot, or an index for a since-superseded one,
	// is marked done without touching OpenSearch — this makes draining
	// order-insensitive (backoff reordering can no longer wipe an active
	// generation or resurrect a stale one).
	active, err := d.rep.SnapshotActive(ctx, row.SnapshotID)
	if err != nil {
		return d.failOutboxRow(ctx, row, fmt.Errorf("snapshot active check: %w", err))
	}
	if row.Operation == repo.OutboxOpDelete && active {
		// #212: an active snapshot's DELETE tombstone is normally obsolete
		// (#127). This must NOT apply to force-replace tombstones carrying
		// frozen chunk ids: those chunks are the superseded generation's —
		// force-replace CASCADE-deletes their ROWS and re-inserts brand-new
		// fresh UUIDs in the same tx, so a frozen id can never belong to the
		// live generation (the canonical invariant this bypass leans on).
		// Deleting them unconditionally cannot wipe an active generation.
		if _, frozen := row.Payload["chunk_ids"]; frozen {
			return drainOutboxDelete(ctx, d, osc, row)
		}
		return d.rep.MarkOutboxDone(ctx, row.ID)
	}
	if row.Operation == repo.OutboxOpIndex && !active {
		return d.rep.MarkOutboxDone(ctx, row.ID)
	}
	if row.Operation == repo.OutboxOpDelete {
		return drainOutboxDelete(ctx, d, osc, row)
	}

	docs, err := d.rep.OutboxDocs(ctx, row.SnapshotID)
	if err != nil {
		return d.failOutboxRow(ctx, row, fmt.Errorf("load docs: %w", err))
	}
	// Row-level ensure: the backstop for a failed (or embedding-less) batch
	// dim-peek above. The two ensure layers cover different failures — the
	// batch peek prevents the no-embedding-first auto-create, this loop
	// guarantees every snapshot carrying embeddings still ensures the index
	// (with its own dimension) before its first doc lands.
	for _, doc := range docs {
		if doc.Embedding != nil {
			if err := osc.ensureIndex(ctx, len(doc.Embedding)); err != nil {
				return d.failOutboxRow(ctx, row, fmt.Errorf("ensure index: %w", err))
			}
			break
		}
	}
	for _, doc := range docs {
		if err := osc.indexDoc(ctx, doc.ChunkID, outboxDocument(row, doc)); err != nil {
			return d.failOutboxRow(ctx, row, err)
		}
	}
	return d.rep.MarkOutboxDone(ctx, row.ID)
}

// drainOutboxDelete materializes a tombstone (#127/#212): every chunk doc of
// the (deactivated, or force-replaced) snapshot leaves the index. Deleting an
// absent doc is fine.
func drainOutboxDelete(ctx context.Context, d *Dispatcher, osc *openSearchClient, row repo.OutboxRow) error {
	// Frozen chunk ids (force-replace tombstones, #212): deleted
	// unconditionally — the payload-freshness invariant is documented at the
	// guard bypass and the writer. Regular deactivate tombstones carry no
	// payload and fall through to the current-row read.
	var ids []string
	if raw, ok := row.Payload["chunk_ids"]; ok {
		// pgx decodes a nested JSONB array into []any; chunk_ids was frozen as a
		// []string, so each element is a string.
		if arr, ok := raw.([]any); ok {
			for _, e := range arr {
				if sv, ok := e.(string); ok {
					ids = append(ids, sv)
				}
			}
		}
	} else {
		var err error
		ids, err = d.rep.OutboxChunkIDs(ctx, row.SnapshotID)
		if err != nil {
			return d.failOutboxRow(ctx, row, fmt.Errorf("load chunk ids: %w", err))
		}
	}
	for _, id := range ids {
		if err := osc.deleteDoc(ctx, id); err != nil {
			return d.failOutboxRow(ctx, row, err)
		}
	}
	// TOCTOU self-heal (#127 review): a reactivation may have committed while
	// this tombstone materialized (the guard-read above is long past). The
	// reactivation tx enqueued its own index op, but ours already deleted
	// docs — re-enqueue ours as index so convergence to the active generation
	// is guaranteed instead of depending on drain order.
	active, err := d.rep.SnapshotActive(ctx, row.SnapshotID)
	if err != nil {
		return d.failOutboxRow(ctx, row, fmt.Errorf("snapshot active recheck: %w", err))
	}
	if active {
		return d.rep.HealOutboxRowToIndex(ctx, row.ID)
	}
	return d.rep.MarkOutboxDone(ctx, row.ID)
}

// outboxDocument builds the indexable chunk body. The document id is the
// durable chunk UUID (passed separately — OpenSearch metadata field, never
// in the body), so re-indexing overwrites instead of duplicating.
func outboxDocument(row repo.OutboxRow, doc repo.OutboxDoc) map[string]any {
	out := map[string]any{
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
	// Sparse joins the doc only when present: a null would fight the
	// rank_features mapping on doc parse (OS rejects null rank_feature values).
	if doc.Sparse != nil {
		out["sparse"] = doc.Sparse
	}
	// #230: machine captions as BM25-only text — absent (not null) when
	// nothing was captioned; never in _source, never citable.
	if doc.CaptionText != "" {
		out["caption_text"] = doc.CaptionText
	}
	return out
}

// failOutboxRow records the failure with capped exponential backoff.
func (d *Dispatcher) failOutboxRow(ctx context.Context, row repo.OutboxRow, cause error) error {
	err := d.rep.FailOutboxAttempt(ctx, row.ID, cause.Error(), outboxBackoff(row.Attempts+1), outboxMaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		// Row already terminal or finished by a faster worker (status='pending'
		// guard in FailOutboxAttempt) — a stale attempt must not flip it back.
		return cause
	}
	if err != nil {
		return fmt.Errorf("record failure: %v (cause: %w)", err, cause)
	}
	return cause
}

// ensureSparseMapping adds the sparse rank_features field to an existing
// index (idempotent additive PUT; R5 #135). Errors surface — a silently
// missing mapping would degrade every sparse query to nothing.
func (c *openSearchClient) ensureSparseMapping(ctx context.Context) error {
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{
			"sparse": map[string]any{"type": "rank_features"},
		},
	})
	if err != nil {
		return err
	}
	return c.putMapping(ctx, "sparse", body)
}

// ensureCaptionMapping adds the #230 caption_text field (plain text —
// BM25 arm only) to an existing index, same additive pattern as sparse R5.
func (c *openSearchClient) ensureCaptionMapping(ctx context.Context) error {
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{
			"caption_text": map[string]any{"type": "text"},
		},
	})
	if err != nil {
		return err
	}
	return c.putMapping(ctx, "caption_text", body)
}

func (c *openSearchClient) putMapping(ctx context.Context, what string, body []byte) error {
	code, respBody, err := c.do(ctx, http.MethodPut, "/"+c.index+"/_mapping", body)
	if err != nil {
		return err
	}
	if code >= 200 && code < 300 {
		return nil
	}
	return fmt.Errorf("put %s mapping: HTTP %d: %s", what, code, truncate(respBody, 200))
}
