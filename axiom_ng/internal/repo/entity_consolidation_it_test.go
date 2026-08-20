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

// seedEntityWithID: kgSeedEntity with a CONTROLLED id — the survivor
// ranking pin must not be a coin flip on gen_random_uuid (review V-W2).
func seedEntityWithID(t *testing.T, lr *leaseRepo, snapID, form, id string, nChunks int) (string, []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO processing_entities (id, snapshot_id, ref, text, canonical_form)
		VALUES ($1::uuid, $2::uuid, $3, $3, $3)`, id, snapID, form); err != nil {
		t.Fatalf("seed entity %s: %v", form, err)
	}
	chunks := make([]string, 0, nChunks)
	for i := 0; i < nChunks; i++ {
		var cID string
		if err := lr.pool.QueryRow(ctx, `
			INSERT INTO processing_chunks (snapshot_id, chunk_index, text, token_count)
			VALUES ($1::uuid,
			        (SELECT coalesce(max(chunk_index), -1) + 1 FROM processing_chunks WHERE snapshot_id = $1::uuid),
			        $2, 10) RETURNING id::text`,
			snapID, form+" inhalt").Scan(&cID); err != nil {
			t.Fatalf("seed chunk for %s: %v", form, err)
		}
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
			VALUES ($1::uuid, $2::uuid, 0, 1)`, id, cID); err != nil {
			t.Fatalf("seed mention %s: %v", form, err)
		}
		chunks = append(chunks, cID)
	}
	return id, chunks
}

func docA_ID(t *testing.T, lr *leaseRepo, attKey string) string {
	t.Helper()
	var docID string
	if err := lr.pool.QueryRow(context.Background(),
		`SELECT document_id::text FROM processing_snapshots
		 WHERE attachment_id = (SELECT id FROM zotero_attachments WHERE zotero_key=$1)
		 ORDER BY created_at DESC LIMIT 1`, attKey).Scan(&docID); err != nil {
		t.Fatalf("docA id: %v", err)
	}
	return docID
}

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
	// Controlled ids: gen_random_uuid would make the ranking pin a coin
	// flip (review V-W2) — deterministic ids pin survivor + tie-break.
	entA, chA := seedEntityWithID(t, lr, snapA, "deutschland", "11111111-1111-1111-1111-111111111111", 3)
	entB, chB := seedEntityWithID(t, lr, snapB, "deutschland", "22222222-2222-2222-2222-222222222222", 5)
	// Distinct form stays untouched.
	entC, chC := kgSeedEntity(t, lr, snapA, "nachhaltigkeit", 2)

	// C1 fixture (review round): same-SNAPSHOT same-form duplicate with a
	// VERBATIM overlapping span on the same chunk — the pre-hardening move
	// violated the (entity, chunk, span) unique key here and aborted the
	// whole epilogue (3,461 live collisions of this shape). The merge must
	// skip the redundant copy, not die.
	var entDup string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form)
		VALUES ($1::uuid, 'de-dup', 'deutschland', 'deutschland') RETURNING id::text`,
		snapA).Scan(&entDup); err != nil {
		t.Fatalf("seed same-snapshot duplicate: %v", err)
	}
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
		SELECT $1::uuid, chunk_id, start_char, end_char
		FROM processing_entity_mentions WHERE entity_id = $2::uuid LIMIT 1`,
		entDup, entA); err != nil {
		t.Fatalf("seed verbatim duplicate span: %v", err)
	}
	// Active-scope fixture: a same-form entity in an INACTIVE snapshot must
	// NOT merge (the wave consolidates the active generation only).
	snapInactive := kgSeedInactiveSnapshot(t, lr, "KGCONS_A", docA_ID(t, lr, "KGCONS_A"))
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form)
		VALUES ($1::uuid, 'de', 'deutschland', 'deutschland')`, snapInactive); err != nil {
		t.Fatalf("seed inactive same-form: %v", err)
	}
	// Relations: A's deutschland -> nachhaltigkeit; B's deutschland ->
	// nachhaltigkeit (corroboration shape); plus one A-internal relation.
	kgSeedRelation(t, lr, snapA, entA, entC, "facet_of", chA[:1])
	kgSeedRelation(t, lr, snapB, entB, entC, "facet_of", chB[:1])
	kgSeedRelation(t, lr, snapA, entC, entA, "subclass_of", chC[:1])

	merged, err := lr.rep.ConsolidateEntities(ctx)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if merged != 2 {
		t.Fatalf("want exactly 2 merged entities (the cross-doc twin + the same-snapshot duplicate), got %d", merged)
	}

	// Survivor: doc B's entity (5 chunks beat 3), still in ITS snapshot.
	var n int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE canonical_form='deutschland'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("2 deutschland entities must remain (active survivor + untouched inactive-snapshot one), got %d", n)
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
	// Active scope: exactly TWO deutschland entities consumed (entA + the
	// same-snapshot duplicate); the inactive-snapshot one survives untouched.
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id
		WHERE e.canonical_form='deutschland' AND s.active`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("exactly ONE active deutschland entity must remain, got %d", n)
	}
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id
		WHERE e.canonical_form='deutschland' AND NOT s.active`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the INACTIVE-snapshot same-form entity must be untouched, got %d", n)
	}
	// Read-path pin (review S1): the corroboration shape survives the merge
	// THROUGH the KG read API — both cross-document facet_of relations count.
	rels, err := lr.rep.KGRelations(ctx, "", survivor, "", 2, 10)
	if err != nil {
		t.Fatalf("KGRelations post-merge: %v", err)
	}
	var facet *int
	for i := range rels {
		if rels[i].Type == "facet_of" && rels[i].TargetForm == "nachhaltigkeit" {
			facet = &rels[i].Documents
		}
	}
	if facet == nil || *facet != 2 {
		t.Fatalf("survivor's facet_of triple must corroborate across 2 documents, got %v", facet)
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

// Batched-resume (owner decision A): per-form atomicity + loud skip +
// resume. A batch that fails leaves its form UN-merged and consistent;
// a re-run picks it up.
func TestIT_ConsolidateEntitiesBatchedResume(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Dok A", "KGCONS_A")
	_, snapB := kgSeedSnapshot(t, lr, "Dok B", "KGCONS_B")
	entA, _ := seedEntityWithID(t, lr, snapA, "form_one", "aaaaaaaa-1111-1111-1111-111111111111", 3)
	entB, _ := seedEntityWithID(t, lr, snapB, "form_one", "aaaaaaaa-2222-2222-2222-222222222222", 5)
	_, _ = kgSeedEntity(t, lr, snapA, "form_two_a", 2)
	entT2, _ := kgSeedEntity(t, lr, snapB, "form_two_a", 3)
	_ = entT2
	_ = entA
	_ = entB

	var failedForms int
	merged, err := lr.rep.ConsolidateEntitiesProgress(ctx,
		func(done, total, m int, form string, berr error) {
			if berr != nil {
				failedForms++
			}
		})
	if err != nil {
		t.Fatalf("batched consolidate: %v", err)
	}
	if merged != 2 {
		t.Fatalf("want 2 merged, got %d (failedForms=%d)", merged, failedForms)
	}
	// both forms fully merged: exactly one entity each remains
	var n1, n2 int
	_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entities WHERE canonical_form='form_one'`).Scan(&n1)
	_ = lr.pool.QueryRow(ctx, `SELECT count(*) FROM processing_entities WHERE canonical_form='form_two_a'`).Scan(&n2)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("per-form atomicity broken: form_one=%d form_two=%d", n1, n2)
	}
	// resume: a re-run is a no-op
	merged2, err := lr.rep.ConsolidateEntities(ctx)
	if err != nil || merged2 != 0 {
		t.Fatalf("resume must be no-op, got %d err=%v", merged2, err)
	}
}
