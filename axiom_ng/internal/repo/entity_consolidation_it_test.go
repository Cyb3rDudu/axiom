package repo

// #193 epilogue: generation-time entity consolidation. Two documents each
// extract "deutschland" as their OWN entity (per-document extraction never
// merges cross-document); after the wave the standard epilogue merges the
// same-canonical-form entities into one survivor with summed mentions and
// re-pointed relations — idempotently (a re-run finds no pairs).
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../scratch_test?sslmode=disable \
//   go test ./internal/repo/ -run TestIT_ConsolidateEntities -v

import (
	"context"
	"testing"
)

func TestIT_ConsolidateEntities(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	_, snapA := kgSeedSnapshot(t, lr, "Dokument A", "KGCONS_A")
	_, snapB := kgSeedSnapshot(t, lr, "Dokument B", "KGCONS_B")

	// Same canonical form in both documents; doc B's entity has MORE
	// mentions (5 vs 3) and must survive the deterministic ranking.
	entA, chA := kgSeedEntity(t, lr, snapA, "deutschland", 3)
	entB, chB := kgSeedEntity(t, lr, snapB, "deutschland", 5)
	// Distinct form stays untouched.
	entC, chC := kgSeedEntity(t, lr, snapA, "nachhaltigkeit", 2)
	_ = entC
	// Relations: A's deutschland -> nachhaltigkeit; B's deutschland ->
	// nachhaltigkeit (corroboration shape); plus one A-internal relation.
	kgSeedRelation(t, lr, snapA, entA, entC, "facet_of", chA[:1])
	kgSeedRelation(t, lr, snapB, entB, entC, "facet_of", chB[:1])
	kgSeedRelation(t, lr, snapA, entC, entA, "subclass_of", chC[:1])

	merged, err := lr.rep.ConsolidateEntities(ctx)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if merged != 1 {
		t.Fatalf("want exactly 1 merged entity (the smaller same-form twin), got %d", merged)
	}

	// Survivor: doc B's entity (5 chunks beat 3), still in ITS snapshot.
	var n int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE canonical_form='deutschland'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("one deutschland entity must remain, got %d", n)
	}
	var survivor string
	if err := lr.pool.QueryRow(ctx,
		`SELECT id::text FROM processing_entities WHERE canonical_form='deutschland'`).Scan(&survivor); err != nil {
		t.Fatal(err)
	}
	if survivor != entB {
		t.Fatalf("survivor must be the most-mentioned entity %s, got %s", entB, survivor)
	}
	// Mentions summed: 3 + 5 = 8 distinct chunks now hang off the survivor.
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(DISTINCT chunk_id) FROM processing_entity_mentions WHERE entity_id=$1::uuid`,
		survivor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("mentions must sum to 8 distinct chunks, got %d", n)
	}
	// Evidence chunks preserved: all 8 chunk rows still exist.
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_chunks c
		JOIN processing_entity_mentions m ON m.chunk_id = c.id
		WHERE m.entity_id=$1::uuid`, survivor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("all 8 evidence chunks must resolve, got %d", n)
	}
	// Relations re-pointed on BOTH endpoints: the two facet_of relations
	// (one per document) now both start at the survivor — the corroboration
	// shape across documents is exactly what the merge must preserve.
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships
		WHERE source_entity_id=$1::uuid AND type='facet_of'`, survivor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("both facet_of relations must hang off the survivor, got %d", n)
	}
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships
		WHERE target_entity_id=$1::uuid AND type='subclass_of'`, survivor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the subclass_of relation must re-point to the survivor, got %d", n)
	}
	// The loser is gone.
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE id=$1::uuid`, entA).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the loser entity row must be deleted")
	}

	// Idempotency: a re-run finds no same-form pairs among actives.
	merged2, err := lr.rep.ConsolidateEntities(ctx)
	if err != nil {
		t.Fatalf("consolidate re-run: %v", err)
	}
	if merged2 != 0 {
		t.Fatalf("re-run must be a no-op, merged %d", merged2)
	}
}
