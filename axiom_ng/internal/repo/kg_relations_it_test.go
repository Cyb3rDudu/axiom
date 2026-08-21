package repo

// #185 KG relations noise: cross-document corroboration as the ranking
// signal, plus the optional document_id scope. All fixtures are seeded —
// no dependency on live corpus data.
//
// Scenario pinned here (the agent-observed noise shape):
//
//	doc A "CSR Buch":  nachhaltigkeit --facet_of--> csr            (thematic)
//	                  weleda --owned_by--> nachhaltigkeit          (junk type)
//	doc B "CSR Buch 2": nachhaltigkeit --facet_of--> csr           (SAME triple)
//
// The junk endpoints have HIGHER combined popularity than the thematic
// pair (weleda in 8 chunks), so the OLD ranking (src.chunks + tgt.chunks
// DESC) served the junk first. The corroboration ranking must serve the
// cross-document triple first with documents=2 and the single-document
// junk last with documents=1. The document_id scope restricts WHICH
// relations are returned while the corroboration count stays global.
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../scratch_test?sslmode=disable \
//   go test ./internal/repo/ -run TestIT_KGRelations -v

import (
	"context"
	"testing"
)

func kgSeedSnapshot(t *testing.T, lr *leaseRepo, title, attKey string) (docID, snapID string) {
	t.Helper()
	ctx := context.Background()
	// Distinct library per book: seed() inserts a fresh zotero_source row,
	// and two identical sources could trip the source unique key.
	_, _ = lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-" + attKey,
		docKey: "DOC" + attKey, attKey: attKey, contentHash: &attKey}, "completed", 1)
	var attID string
	if err := lr.pool.QueryRow(ctx,
		`SELECT id FROM zotero_attachments WHERE zotero_key = $1`, attKey).Scan(&attID); err != nil {
		t.Fatalf("seed attachment %s: %v", attKey, err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		SELECT $1::uuid, $2, 'test', '1', 'p1', a.document_id, '{}', true
		FROM zotero_attachments a WHERE a.id = $1::uuid
		RETURNING id::text`, attID, attKey).Scan(&snapID); err != nil {
		t.Fatalf("seed snapshot for %s: %v", title, err)
	}
	if err := lr.pool.QueryRow(ctx,
		`SELECT document_id::text FROM processing_snapshots WHERE id = $1::uuid`, snapID).Scan(&docID); err != nil {
		t.Fatalf("doc id for %s: %v", title, err)
	}
	return docID, snapID
}

// kgSeedEntity inserts an entity mentioned in nChunks chunks of the
// snapshot; the chunks double as evidence targets.
func kgSeedEntity(t *testing.T, lr *leaseRepo, snapID, form string, nChunks int) (string, []string) {
	t.Helper()
	ctx := context.Background()
	var entID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form)
		VALUES ($1::uuid, $2, $2, $2) RETURNING id::text`, snapID, form).Scan(&entID); err != nil {
		t.Fatalf("seed entity %s: %v", form, err)
	}
	chunks := make([]string, 0, nChunks)
	for i := 0; i < nChunks; i++ {
		var cID string
		if err := lr.pool.QueryRow(ctx, `
			INSERT INTO processing_chunks (snapshot_id, chunk_index, text, token_count)
			VALUES ($1::uuid,
			        (SELECT coalesce(max(chunk_index), -1) + 1 FROM processing_chunks WHERE snapshot_id = $1::uuid),
			        $2, 10)
			RETURNING id::text`,
			snapID, form+" inhalt").Scan(&cID); err != nil {
			t.Fatalf("seed chunk for %s: %v", form, err)
		}
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
			VALUES ($1::uuid, $2::uuid, 0, 1)`, entID, cID); err != nil {
			t.Fatalf("seed mention %s: %v", form, err)
		}
		chunks = append(chunks, cID)
	}
	return entID, chunks
}

func kgSeedRelation(t *testing.T, lr *leaseRepo, snapID, srcID, tgtID, relType string, evidence []string) string {
	t.Helper()
	ctx := context.Background()
	ev := "["
	for i, c := range evidence {
		if i > 0 {
			ev += ","
		}
		ev += `"` + c + `"`
	}
	ev += "]"
	var relID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_entity_relationships
			(snapshot_id, source_entity_id, target_entity_id, type, strength, evidence_chunk_ids)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 0.7, $5::jsonb)
		RETURNING id::text`, snapID, srcID, tgtID, relType, ev).Scan(&relID); err != nil {
		t.Fatalf("seed relation %s: %v", relType, err)
	}
	return relID
}

func kgSeedInactiveSnapshot(t *testing.T, lr *leaseRepo, attKey, docID string) string {
	t.Helper()
	ctx := context.Background()
	var attID, snapID string
	if err := lr.pool.QueryRow(ctx,
		`SELECT id FROM zotero_attachments WHERE zotero_key = $1`, attKey).Scan(&attID); err != nil {
		t.Fatalf("lookup attachment %s: %v", attKey, err)
	}
	// Same (attachment, hash, processor, version, profile) identity would
	// collide with the active snapshot — bump the processor version.
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1::uuid, 'superseded', 'test', '2', 'p1', $2::uuid, '{}', false)
		RETURNING id::text`, attID, docID).Scan(&snapID); err != nil {
		t.Fatalf("seed inactive snapshot: %v", err)
	}
	return snapID
}

func TestIT_KGRelationsCorroborationRankingAndScope(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate kg fixtures: %v", err)
	}

	docA, snapA := kgSeedSnapshot(t, lr, "CSR Buch", "KGATT_A")
	_, snapB := kgSeedSnapshot(t, lr, "CSR Buch 2", "KGATT_B")

	// Doc A: thematic pair (2 chunks each) + junk with a POPULAR endpoint.
	entNaA, chNaA := kgSeedEntity(t, lr, snapA, "nachhaltigkeit", 2)
	entCsrA, _ := kgSeedEntity(t, lr, snapA, "csr", 2)
	entWeledaA, chWeA := kgSeedEntity(t, lr, snapA, "weleda", 8)
	kgSeedRelation(t, lr, snapA, entNaA, entCsrA, "facet_of", chNaA)
	kgSeedRelation(t, lr, snapA, entWeledaA, entNaA, "owned_by", chWeA[:1])

	// Doc B: the SAME (nachhaltigkeit, facet_of, csr) triple — corroboration.
	entNaB, chNaB := kgSeedEntity(t, lr, snapB, "nachhaltigkeit", 2)
	entCsrB, _ := kgSeedEntity(t, lr, snapB, "csr", 2)
	kgSeedRelation(t, lr, snapB, entNaB, entCsrB, "facet_of", chNaB)

	// Inactive-snapshot evidence: a SUPERSEDED snapshot of doc A carries a
	// chunk that an active doc-A relation cites as evidence. The scope must
	// NOT count evidence from inactive snapshots (mutation c in review:
	// dropping esn.active stayed green before this fixture).
	snapA2Inactive := kgSeedInactiveSnapshot(t, lr, "KGATT_A", docA)
	_, chGri := kgSeedEntity(t, lr, snapA2Inactive, "gri", 2)
	entStdA, _ := kgSeedEntity(t, lr, snapA, "reporting_standard", 2)
	kgSeedRelation(t, lr, snapA, entStdA, entStdA, "same_as", chGri[:1])

	if _, err := lr.rep.ConsolidateEntities(ctx); err != nil {
		t.Fatalf("ConsolidateEntities: %v", err)
	}
	var entNa string
	if err := lr.pool.QueryRow(ctx, `
		SELECT e.id::text FROM processing_entities e
		JOIN processing_snapshots s ON s.id=e.snapshot_id AND s.active
		WHERE e.canonical_form='nachhaltigkeit'`).Scan(&entNa); err != nil {
		t.Fatalf("read nachhaltigkeit survivor: %v", err)
	}

	// 1. Ranking: corroborated triple FIRST despite lower endpoint
	// popularity (weleda 8 + nachhaltigkeit 2 > nachhaltigkeit 2 + csr 2 —
	// the old ranking served the junk first).
	rels, err := lr.rep.KGRelations(ctx, "", entNa, "", 2, 50)
	if err != nil {
		t.Fatalf("KGRelations: %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("want 2 relations for the doc-A nachhaltigkeit entity, got %d", len(rels))
	}
	if rels[0].Type != "facet_of" || rels[0].TargetForm != "csr" || rels[0].Documents != 2 {
		t.Fatalf("corroborated triple must rank first with documents=2, got %+v", rels[0])
	}
	if rels[1].Type != "owned_by" || rels[1].SourceForm != "weleda" || rels[1].Documents != 1 {
		t.Fatalf("single-document junk must rank last with documents=1, got %+v", rels[1])
	}

	// 2. document_id scope: only relations with evidence in the scoped
	// document's active snapshot; corroboration stays GLOBAL.
	scopedA, err := lr.rep.KGRelations(ctx, "", "", docA, 2, 50)
	if err != nil {
		t.Fatalf("scope A: %v", err)
	}
	if len(scopedA) != 2 {
		t.Fatalf("doc A scope must carry both doc-A relations, got %d", len(scopedA))
	}
	for _, r := range scopedA {
		if r.SourceForm == "nachhaltigkeit" && r.Type == "facet_of" && r.Documents != 2 {
			t.Fatalf("scoped relation must keep global corroboration (documents=2), got %+v", r)
		}
	}

	// 3. Entity + scope agree (both doc-A relations qualify).
	entScoped, err := lr.rep.KGRelations(ctx, "", entNa, docA, 2, 50)
	if err != nil || len(entScoped) != 2 {
		t.Fatalf("entity-from-A + scope A must return its 2 relations, got %d err=%v", len(entScoped), err)
	}

	// 4. Type filter still narrows within the ranking.
	typed, err := lr.rep.KGRelations(ctx, "owned_by", entNa, "", 2, 50)
	if err != nil || len(typed) != 1 || typed[0].SourceForm != "weleda" {
		t.Fatalf("type filter broken: %d err=%v", len(typed), err)
	}

	// 5. Evidence resolving only into an INACTIVE snapshot of the scoped
	// document does not satisfy the scope (len(scopedA) stays 2 — the
	// reporting_standard relation would leak in as a third without the
	// esn.active check).
	if len(scopedA) != 2 { // re-assert after the extra fixture rows
		t.Fatalf("inactive-snapshot evidence leaked into the doc-A scope: %d", len(scopedA))
	}
}

// #193 P1 repo-level pin: the entity filter scopes to exactly that entity's
// relations — a NONEXISTENT uuid returns EMPTY (pre-fix the HTTP layer never
// sent the filter, so any id returned the unfiltered list).
func TestIT_KGRelationsNonexistentEntityIsEmpty(t *testing.T) {
	lr := openLeaseDB(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snapA := kgSeedSnapshot(t, lr, "Klein", "KGATT_P1")
	entX, chX := kgSeedEntity(t, lr, snapA, "konzept_x", 2)
	entY, _ := kgSeedEntity(t, lr, snapA, "konzept_y", 2)
	kgSeedRelation(t, lr, snapA, entX, entY, "same_as", chX[:1])
	if err := lr.rep.RefreshKGReadModel(ctx); err != nil {
		t.Fatalf("RefreshKGReadModel: %v", err)
	}

	// a real id returns exactly its relations (scoped-only).
	rels, err := lr.rep.KGRelations(ctx, "", entX, "", 2, 50)
	if err != nil {
		t.Fatalf("KGRelations: %v", err)
	}
	if len(rels) != 1 || rels[0].SourceForm != "konzept_x" {
		t.Fatalf("real entity must scope to its own relations, got %+v", rels)
	}

	// a nonexistent id returns the EMPTY slice ([] on the wire, not null).
	rels2, err := lr.rep.KGRelations(ctx, "", "00000000-0000-0000-0000-00000000dead", "", 2, 50)
	if err != nil {
		t.Fatalf("nonexistent entity: %v", err)
	}
	if rels2 == nil || len(rels2) != 0 {
		t.Fatalf("nonexistent entity must return an EMPTY non-nil slice, got %#v", rels2)
	}
}
