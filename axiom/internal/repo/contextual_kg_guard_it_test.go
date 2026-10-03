package repo

import (
	"context"
	"encoding/json"
	"testing"
)

// TestPersistContextualKGGuardIT moved back from the mirror extraction: it
// drives repo.PersistResult (the STORE persist path) against a contextual
// citation class — the dual-read the transition documents (#255 persist
// gate reads the class the mirror projection owns until F12/DM06).
func TestPersistContextualKGGuardIT(t *testing.T) {
	h := newPersistHarness(t, "ctxguard")
	ctx := context.Background()
	const dims = 3

	// The harness document is a LECTURE: contextual. Even a result that
	// (wrongly) carries entities must persist ZERO of them — the graph stays
	// book-truth — while chunks + embeddings persist untouched (equal rank).
	if _, err := h.pool.Exec(ctx,
		`UPDATE zotero_documents SET citation_class='contextual' WHERE zotero_key='DOCctxguard'`); err != nil {
		t.Fatal(err)
	}
	raw := h.validResultRaw(dims)
	if len(raw.Entities) == 0 || len(raw.EntityRelationships) == 0 {
		t.Fatal("fixture must carry entities/relationships to prove the guard")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	snapID, err := h.persist(t, b, dims, markdownArtifact())
	if err != nil {
		t.Fatalf("persist: %v", err)
	}

	var n int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE snapshot_id=$1`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("contextual document must contribute ZERO entities, got %d", n)
	}
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entity_relationships WHERE snapshot_id=$1`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("contextual document must contribute ZERO relationships, got %d", n)
	}
	// Equal-rank substrate: chunks + dense embeddings persist like any book.
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_chunks WHERE snapshot_id=$1`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(raw.Chunks) {
		t.Fatalf("contextual chunks must persist, got %d want %d", n, len(raw.Chunks))
	}
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_chunk_dense_embeddings e
		JOIN processing_chunks c ON c.id=e.chunk_id
		WHERE c.snapshot_id=$1 AND model='reference-bge-m3'`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(raw.Chunks) {
		t.Fatalf("contextual dense embeddings must persist, got %d want %d", n, len(raw.Chunks))
	}

	// Passenger: a citable document keeps its entities.
	h2 := newPersistHarness(t, "ctxpass")
	b2 := h2.validResultBytes(t, dims)
	snapID2, err := h2.persist(t, b2, dims, markdownArtifact())
	if err != nil {
		t.Fatalf("persist passenger: %v", err)
	}
	if err := h2.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE snapshot_id=$1`, snapID2).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("citable passenger must keep entities, got %d", n)
	}
}
