package repo

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

func TestIT_KGReadModelRefreshDirectionArbitration(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE kg_relation_evidence_docs, kg_relation_triples, kg_entity_roots,
		         processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "RM Direction", "RMDIR_A")
	eA, chA := kgSeedEntity(t, lr, snap, "alpha", 2)
	eB, chB := kgSeedEntity(t, lr, snap, "beta", 2)
	kgSeedRelation(t, lr, snap, eA, eB, "related_to", chA[:1])
	kgSeedRelation(t, lr, snap, eB, eA, "related_to", chB[:1])
	kgSeedRelation(t, lr, snap, eB, eA, "related_to", chB[1:2])

	if err := lr.rep.RefreshKGReadModel(ctx); err != nil {
		t.Fatalf("RefreshKGReadModel: %v", err)
	}
	var n int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM kg_relation_triples WHERE type='related_to'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("direction arbitration must materialize one triple, got %d", n)
	}
	var src, tgt string
	var evCount, docs int
	if err := lr.pool.QueryRow(ctx, `
		SELECT source_root_id::text, target_root_id::text, evidence_count, corroborating_documents
		FROM kg_relation_triples WHERE type='related_to'`).Scan(&src, &tgt, &evCount, &docs); err != nil {
		t.Fatal(err)
	}
	if src != eB || tgt != eA {
		t.Fatalf("majority direction must win: want %s->%s, got %s->%s", eB, eA, src, tgt)
	}
	if evCount != 3 || docs != 1 {
		t.Fatalf("support must be unioned across both directions, evidence=%d docs=%d", evCount, docs)
	}
}

func TestIT_KGReadModelRefreshDropsIntraFamilyEdges(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE kg_relation_evidence_docs, kg_relation_triples, kg_entity_roots,
		         processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "RM Family", "RMFAM_A")
	eRoot, chRoot := kgSeedEntity(t, lr, snap, "nachhaltigkeit", 3)
	eVar, _ := kgSeedEntity(t, lr, snap, "nachhaltigkeiten", 2)
	if _, err := lr.pool.Exec(ctx, `UPDATE processing_entities SET alias_of=$1::uuid WHERE id=$2::uuid`, eRoot, eVar); err != nil {
		t.Fatal(err)
	}
	kgSeedRelation(t, lr, snap, eVar, eRoot, "facet_of", chRoot[:1])

	if err := lr.rep.RefreshKGReadModel(ctx); err != nil {
		t.Fatalf("RefreshKGReadModel: %v", err)
	}
	var triples int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM kg_relation_triples`).Scan(&triples); err != nil {
		t.Fatal(err)
	}
	if triples != 0 {
		t.Fatalf("family-internal raw edges must not materialize, got %d triples", triples)
	}
	var roots int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM kg_entity_roots WHERE root_entity_id=$1::uuid AND member_count=2`, eRoot).Scan(&roots); err != nil {
		t.Fatal(err)
	}
	if roots != 1 {
		t.Fatalf("family root must still materialize with both members, got %d", roots)
	}
}

func TestIT_KGReadModelAPIsReadMaterializedTables(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE kg_relation_evidence_docs, kg_relation_triples, kg_entity_roots,
		         processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "RM API", "RMAPI_A")
	eN, chN := kgSeedEntity(t, lr, snap, "nachhaltigkeitsbericht", 3)
	eC, _ := kgSeedEntity(t, lr, snap, "csr", 2)
	kgSeedRelation(t, lr, snap, eN, eC, "facet_of", chN[:1])
	if err := lr.rep.RefreshKGReadModel(ctx); err != nil {
		t.Fatalf("RefreshKGReadModel: %v", err)
	}

	// Remove raw relations after refresh: API must still serve from the read model.
	if _, err := lr.pool.Exec(ctx, `DELETE FROM processing_entity_relationships`); err != nil {
		t.Fatal(err)
	}
	ents, err := lr.rep.SearchKGEntities(ctx, "Nachhaltigkeitsberichte", 1, 10)
	if err != nil || len(ents) == 0 || ents[0].ID != eN {
		t.Fatalf("SearchKGEntities read-model hit = %+v err=%v", ents, err)
	}
	rels, err := lr.rep.KGRelations(ctx, "facet_of", eN, "", 1, 10)
	if err != nil || len(rels) != 1 || rels[0].SourceID != eN || rels[0].TargetID != eC {
		t.Fatalf("KGRelations read-model hit = %+v err=%v", rels, err)
	}
	neigh, err := lr.rep.KGNeighbors(ctx, eN, 1, 10)
	if err != nil || len(neigh) != 1 || neigh[0].OtherID != eC {
		t.Fatalf("KGNeighbors read-model hit = %+v err=%v", neigh, err)
	}
}

func TestIT_KGReadModelRelationsBrowseP95Under300msOn10xSeed(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE kg_relation_evidence_docs, kg_relation_triples, kg_entity_roots,
		         processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "RM Perf", "RMPERF_A")
	const roots = 1200
	const triples = 12000
	for i := 0; i < roots; i++ {
		id := testUUID(i + 1)
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO processing_entities (id, snapshot_id, ref, text, canonical_form)
			VALUES ($1::uuid, $2::uuid, $3, $3, $3)
			ON CONFLICT DO NOTHING`, id, snap, "entity-"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO kg_entity_roots (root_entity_id, primary_form, primary_text, forms, type_votes, mention_count, member_count, normalized_form, normalized_form_nofam)
			VALUES ($1::uuid, $2, $2, $3::jsonb, '{}'::jsonb, $4, 1, $5, $5)`, id, "entity-"+id, `["entity"]`, 10+i%50, normalizeKGTerm("entity-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < triples; i++ {
		srcIdx := i % roots
		tgtIdx := (srcIdx + 1 + (i / roots)) % roots
		src := testUUID(srcIdx + 1)
		tgt := testUUID(tgtIdx + 1)
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO kg_relation_triples
			  (source_root_id, target_root_id, type, source_form, target_form,
			   source_mentions, target_mentions, evidence_chunk_ids, evidence_count,
			   triple_row_count, corroborating_documents, section_quality, confidence)
			VALUES ($1::uuid,$2::uuid,'facet_of',$3,$4,20,18,'[]'::jsonb,0,1,$5,1,0.5)`,
			src, tgt, "entity-"+src, "entity-"+tgt, 1+i%8); err != nil {
			t.Fatal(err)
		}
	}
	var seeded int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM kg_relation_triples`).Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	if seeded != triples {
		t.Fatalf("performance fixture inserted %d triples, want %d", seeded, triples)
	}
	lat := make([]time.Duration, 40)
	for i := range lat {
		start := time.Now()
		if _, err := lr.rep.KGRelations(ctx, "", "", "", 1, 50); err != nil {
			t.Fatal(err)
		}
		lat[i] = time.Since(start)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p95 := lat[int(float64(len(lat))*0.95)-1]
	if p95 >= 300*time.Millisecond {
		t.Fatalf("read-model relation browse p95 = %s, want <300ms", p95)
	}
}

func testUUID(n int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
}

// Family-forms tier ranking witness: a root whose primary_form is
// "stakeholders" but whose forms list contains "stakeholder" must rank
// the query "stakeholder" at Tier 1 (exact form match on a family
// member), not Tier 4 (substring on the primary). The query
// "stakeholders" (primary exact) stays Tier 1 too.
func TestIT_KGFormsTierRanking(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE kg_relation_evidence_docs, kg_relation_triples, kg_entity_roots,
		         processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, snap := kgSeedSnapshot(t, lr, "Tier Buch", "TIER_A")

	// Family root: "stakeholders" (3 chunks) + variant "stakeholder" (2).
	ePlural, _ := kgSeedEntity(t, lr, snap, "stakeholders", 3)
	eSingular, _ := kgSeedEntity(t, lr, snap, "stakeholder", 2)
	_ = eSingular
	// Substring competitor: "Stakeholder-Theorie" (20 chunks — far more
	// mentions than the family root (5) to prove tier beats mention_count).
	_, _ = kgSeedEntity(t, lr, snap, "Stakeholder-Theorie", 20)

	// Bind the family FIRST (so the read model has one root with both forms).
	if _, err := lr.rep.BindFlexionAliases(ctx); err != nil {
		t.Fatalf("BindFlexionAliases: %v", err)
	}

	if err := lr.rep.RefreshKGReadModel(ctx); err != nil {
		t.Fatalf("RefreshKGReadModel: %v", err)
	}

	// Query "stakeholder" (singular): must find the family root at rank 1.
	ents, err := lr.rep.SearchKGEntities(ctx, "stakeholder", 1, 10)
	if err != nil {
		t.Fatalf("search stakeholder: %v", err)
	}
	if len(ents) == 0 {
		t.Fatal("no results for stakeholder")
	}
	if ents[0].ID != ePlural {
		t.Fatalf("query 'stakeholder': family root must rank 1, got %s (forms=%v)", ents[0].ID, ents[0].Forms)
	}

	// Query "stakeholders" (plural = primary): also rank 1.
	ents2, err := lr.rep.SearchKGEntities(ctx, "stakeholders", 1, 10)
	if err != nil {
		t.Fatalf("search stakeholders: %v", err)
	}
	if len(ents2) == 0 || ents2[0].ID != ePlural {
		t.Fatalf("query 'stakeholders': family root must rank 1, got %+v", ents2)
	}
}
