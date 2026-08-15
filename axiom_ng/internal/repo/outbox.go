package repo

// L5: OpenSearch outbox drain support. The claim is an atomic
// UPDATE..FOR UPDATE SKIP LOCKED so multiple axiom-ng instances never
// double-drain a row (work order §10.3: OpenSearch is never on the
// snapshot-transaction path; all methods here run OUTSIDE any snapshot TX).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// OutboxRow is a claimed opensearch_outbox item.
type OutboxRow struct {
	ID         string
	SnapshotID string
	Operation  string
	Payload    map[string]any
	Attempts   int
}

// OutboxDoc is one indexable chunk document for a snapshot.
type OutboxDoc struct {
	ChunkID   string
	ChunkRef  string
	Index     int
	Text      string
	Locator   map[string]any
	Sections  []string
	Tokens    int
	Embedding []float64 // nil when the snapshot has no dense embedding
}

// ClaimOutboxBatch atomically claims up to limit due pending rows
// (FOR UPDATE SKIP LOCKED). Claiming bumps next_attempt_at by visibility so
// a crashed worker's rows reappear for retry instead of being locked out.
func (r *Repo) ClaimOutboxBatch(ctx context.Context, limit int, visibility time.Duration) ([]OutboxRow, error) {
	if limit <= 0 {
		limit = 64 // mirrors dispatcher outboxBatchSize (default claim size)
	}
	rows, err := r.pool.Query(ctx, `
		UPDATE opensearch_outbox o
		SET next_attempt_at = now() + $2::interval
		WHERE o.id IN (
			SELECT id FROM opensearch_outbox
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at, id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING o.id::text, o.snapshot_id::text, o.operation, o.payload, o.attempts
	`, limit, visibility.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var it OutboxRow
		if err := rows.Scan(&it.ID, &it.SnapshotID, &it.Operation, &it.Payload, &it.Attempts); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// OutboxDocs loads the indexable chunk documents for a snapshot: chunk text +
// locator + section titles + dense embedding (LEFT JOIN — a snapshot without
// dense embeddings still indexes, embedding stays nil).
func (r *Repo) OutboxDocs(ctx context.Context, snapshotID string) ([]OutboxDoc, error) {
	rows, err := r.pool.Query(ctx, `
	SELECT c.id::text, c.chunk_index, c.text, c.locator,
		       c.section_titles, c.token_count,
		       CASE WHEN e.vector IS NULL THEN NULL ELSE e.vector::text END
	FROM processing_chunks c
	LEFT JOIN processing_chunk_dense_embeddings e ON e.chunk_id = c.id
	WHERE c.snapshot_id = $1
	ORDER BY c.chunk_index
	`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxDoc
	for rows.Next() {
		var d OutboxDoc
		var vec *string
		if err := rows.Scan(&d.ChunkID, &d.Index, &d.Text, &d.Locator,
			&d.Sections, &d.Tokens, &vec); err != nil {
			return nil, err
		}
		// Contract ref is not persisted (durable identity is id+index); derive
		// it with the same 0-based scheme the processor emits.
		d.ChunkRef = fmt.Sprintf("chunk-%04d", d.Index)
		if vec != nil {
			emb, err := parseVector(*vec)
			if err != nil {
				return nil, fmt.Errorf("snapshot %s chunk %s vector: %w", snapshotID, d.ChunkID, err)
			}
			d.Embedding = emb
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// OutboxDocumentID reads the document_id from a row's payload.
func (r OutboxRow) OutboxDocumentID() string {
	if v, ok := r.Payload["document_id"].(string); ok {
		return v
	}
	return ""
}

// OutboxAttachmentID reads the attachment_id from a row's payload.
func (r OutboxRow) OutboxAttachmentID() string {
	if v, ok := r.Payload["attachment_id"].(string); ok {
		return v
	}
	return ""
}

// MarkOutboxDone marks a drained row terminal-successful.
func (r *Repo) MarkOutboxDone(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE opensearch_outbox SET status='done', last_error=NULL WHERE id=$1`, id)
	return err
}

// FailOutboxAttempt records a failed drain: attempts+1, last_error, and
// either terminal 'failed' (attempts exhausted) or pending with
// next_attempt_at = now()+backoff.
//
// The status='pending' guard makes the update a no-op (ErrNoRows) when the
// row is already terminal or was finished by a faster worker — a stale
// worker whose visibility window expired must never flip a done row back
// to pending.
//
// Operator recovery — requeue a terminal 'failed' row (e.g. after fixing
// an OpenSearch-side mapping problem):
//
//	UPDATE opensearch_outbox
//	SET status='pending', attempts=0, next_attempt_at=now(), last_error=NULL
//	WHERE id='...';
func (r *Repo) FailOutboxAttempt(ctx context.Context, id, errMsg string, backoff time.Duration, maxAttempts int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE opensearch_outbox
		SET attempts = attempts + 1,
		    last_error = $2,
		    next_attempt_at = now() + $3::interval,
		    status = CASE WHEN attempts + 1 >= $4 THEN 'failed' ELSE 'pending' END
		WHERE id = $1 AND status = 'pending'
	`, id, errMsg, backoff.String(), maxAttempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// OutboxFirstEmbeddingDim returns the dense dimension of ANY chunk among
// the given snapshots (0 when none have embeddings) so the index can be
// created with the right knn_vector mapping BEFORE the first doc lands —
// otherwise OpenSearch auto-creates on a no-embedding doc and the field
// maps to plain float (no k-NN).
func (r *Repo) OutboxFirstEmbeddingDim(ctx context.Context, snapshotIDs []string) (int, error) {
	if len(snapshotIDs) == 0 {
		return 0, nil
	}
	var dim int
	err := r.pool.QueryRow(ctx, `
		SELECT e.dimensions
		FROM processing_chunk_dense_embeddings e
		JOIN processing_chunks c ON c.id = e.chunk_id
		WHERE c.snapshot_id = ANY($1::uuid[])
		LIMIT 1
	`, snapshotIDs).Scan(&dim)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return dim, nil
}

// parseVector turns a pgvector text form "[0.1,0.2,...]" into []float64.
func parseVector(s string) ([]float64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// SnapshotActive reports whether the snapshot is currently the active
// generation for its attachment. The outbox drainer uses it to skip obsolete
// operations (#127): a delete for a since-reactivated snapshot, or an index
// for a since-superseded one, must not materialize stale state.
func (r *Repo) SnapshotActive(ctx context.Context, snapshotID string) (bool, error) {
	var active bool
	err := r.pool.QueryRow(ctx,
		`SELECT active FROM processing_snapshots WHERE id=$1`, snapshotID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return active, nil
}

// OutboxChunkIDs returns the durable chunk ids of a snapshot — the doc ids a
// tombstone (#127) must delete from OpenSearch.
func (r *Repo) OutboxChunkIDs(ctx context.Context, snapshotID string) ([]string, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id::text FROM processing_chunks WHERE snapshot_id=$1 ORDER BY chunk_index`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
