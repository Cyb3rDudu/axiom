package ingest_test

import (
	"context"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/testutil"
)

// TestEntitiesRepoRoundTrip covers repo.Entities against real Postgres
// so we catch SQL syntax + conflict-upsert drift the stubs can't.
func TestEntitiesRepoRoundTrip(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	uid := seedUser(t, pg, "entities-repo")
	ids := seedPending(t, pg, uid, 1)
	docID := ids[0]

	chunkID := "d_chunk_0000"
	if err := pg.DB.Exec(`
		INSERT INTO document_chunks (id, doc_id, chunk_id, chunk_index, chunk_text,
		                             sparse_embedding, chunk_metadata, created_at)
		VALUES (gen_random_uuid(), ?, ?, 0, 'hello world',
		        '{}'::jsonb, '{}'::jsonb, NOW())`,
		docID, chunkID).Error; err != nil {
		t.Fatalf("seed chunk: %v", err)
	}

	entities := repo.NewEntities(pg.DB)
	id1, err := entities.UpsertEntity(context.Background(), repo.EntityUpsert{
		Text: "Acme Corp", Type: "ORGANIZATION", CanonicalForm: "acme corp",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	id2, err := entities.UpsertEntity(context.Background(), repo.EntityUpsert{
		Text: "ACME Corp", Type: "ORGANIZATION", CanonicalForm: "acme corp",
	})
	if err != nil {
		t.Fatalf("upsert dup: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("upsert should dedupe: %v vs %v", id1, id2)
	}

	for i := 0; i < 2; i++ {
		if err := entities.LinkChunk(context.Background(), repo.OccurrenceLink{
			EntityID:        id1,
			ChunkID:         chunkID,
			DocID:           docID,
			PositionInChunk: 17,
			RelevanceScore:  0.9,
		}); err != nil {
			t.Fatalf("link[%d]: %v", i, err)
		}
	}
	var count int32
	if err := pg.DB.Raw(
		`SELECT occurrence_count FROM entity_chunk_occurrences WHERE entity_id = ? AND chunk_id = ?`,
		id1, chunkID,
	).Scan(&count).Error; err != nil {
		t.Fatalf("read count: %v", err)
	}
	if count != 2 {
		t.Errorf("occurrence_count: got %d want 2", count)
	}

	n, err := entities.CountEntitiesForDoc(context.Background(), docID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("distinct entities: got %d want 1", n)
	}

	if err := entities.DeleteForDoc(context.Background(), docID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var occCount int64
	_ = pg.DB.Raw(`SELECT COUNT(*) FROM entity_chunk_occurrences WHERE doc_id = ?`, docID).Scan(&occCount).Error
	if occCount != 0 {
		t.Errorf("occurrences not cleared: %d", occCount)
	}
}
