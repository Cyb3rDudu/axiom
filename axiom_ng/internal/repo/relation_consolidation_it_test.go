package repo

// #198-2 IT: relations consolidation — per (source,target) pair ONE
// aggregated edge among ACTIVE snapshots. The pinned scenario mirrors the
// external regression (facet_of ×N + main_subject + a reverse-direction
// edge on the same pair):
//
//   entNa --facet_of-->    entCsr   evidence [chA1]        (doc A)
//   entNa --facet_of-->    entCsr   evidence [chA2]        (doc A, dup type)
//   entNa --main_subject--> entCsr  evidence [chA1]        (doc A, loser type)
//   entNa --facet_of-->    entCsr   evidence [chB1]        (doc B — cross-doc)
//   entCsr --related_to--> entNa    evidence [chB2]        (doc B — REVERSED)
//
// After ConsolidateRelationsReport the pair collapses to ONE edge:
//   - direction forward (3 forward edges vs 1 reversed — corroboration),
//   - type facet_of (3 edges vs 1 main_subject),
//   - evidence = union of the winning type's forward edges {chA1,chA2,chB1}
//     (corpus-wide, cross-snapshot),
//   - loser types archived in metadata.superseded_types with their own
//     evidence trail (main_subject as_is, related_to reversed) — never a
//     silent delete,
//   - KGRelations reports documents=2 (docs of the evidence chunks).
// Re-run must be a NO-OP. Unrelated single-edge pairs and INACTIVE
// snapshot edges stay untouched.
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../axiom_ng_test_kgrel?sslmode=disable \
//   go test ./internal/repo/ -run TestIT_RelationConsolidation -v

import (
	"context"
	"encoding/json"
	"testing"
)

func relConsEdges(t *testing.T, lr *leaseRepo, pairA, pairB string) (edges int) {
	t.Helper()
	var n int
	if err := lr.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM processing_entity_relationships r
		JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
		WHERE (r.source_entity_id = $1::uuid AND r.target_entity_id = $2::uuid)
		   OR (r.source_entity_id = $2::uuid AND r.target_entity_id = $1::uuid)`,
		pairA, pairB).Scan(&n); err != nil {
		t.Fatalf("count pair edges: %v", err)
	}
	return n
}

func TestIT_RelationConsolidationCollapsesPairToOneAggregatedEdge(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate kg fixtures: %v", err)
	}

	docA, snapA := kgSeedSnapshot(t, lr, "CSR Buch", "RCATT_A")
	_, snapB := kgSeedSnapshot(t, lr, "CSR Buch 2", "RCATT_B")

	// Entities live once (post-#193 shape: cross-snapshot survivors).
	entNa, chA := kgSeedEntity(t, lr, snapA, "nachhaltigkeit", 2)
	entCsr, _ := kgSeedEntity(t, lr, snapA, "csr", 2)
	// A doc-B chunk to cite as evidence from snapB (cross-doc corroboration).
	entSeedB, chB := kgSeedEntity(t, lr, snapB, "nachhaltigkeit", 2) // same form, other row — only its chunks are used
	_ = entSeedB

	kgSeedRelation(t, lr, snapA, entNa, entCsr, "facet_of", chA[:1])     // e1
	kgSeedRelation(t, lr, snapA, entNa, entCsr, "facet_of", chA[1:2])    // e2 (dup type, other evidence)
	kgSeedRelation(t, lr, snapA, entNa, entCsr, "main_subject", chA[:1]) // e3 (loser type)
	kgSeedRelation(t, lr, snapB, entNa, entCsr, "facet_of", chB[:1])     // e4 (cross-doc)
	kgSeedRelation(t, lr, snapB, entCsr, entNa, "related_to", chB[1:2])  // e5 (REVERSED direction)

	// An unrelated single-edge pair must survive byte-identical.
	entX, chX := kgSeedEntity(t, lr, snapA, "reporting_standard", 2)
	entY, _ := kgSeedEntity(t, lr, snapA, "gri", 2)
	soloID := kgSeedRelation(t, lr, snapA, entX, entY, "part_of", chX[:1])

	// An INACTIVE-snapshot edge on ANOTHER pair: consolidation scope is
	// active snapshots only.
	snapInact := kgSeedInactiveSnapshot(t, lr, "RCATT_A", docA)
	entP, _ := kgSeedEntity(t, lr, snapInact, "photonik", 2)
	entQ, _ := kgSeedEntity(t, lr, snapInact, "laser", 2)
	kgSeedRelation(t, lr, snapInact, entP, entQ, "facet_of", nil)
	kgSeedRelation(t, lr, snapInact, entP, entQ, "main_subject", nil)

	if got := relConsEdges(t, lr, entNa, entCsr); got != 5 {
		t.Fatalf("seed sanity: want 5 edges on the pair, got %d", got)
	}

	rep, err := lr.rep.ConsolidateRelationsReport(ctx)
	if err != nil {
		t.Fatalf("ConsolidateRelationsReport: %v", err)
	}
	if rep.MultiEdgePairs != 1 {
		t.Fatalf("report: want multi_edge_pairs=1, got %+v", rep)
	}
	if rep.CollapsedEdges != 4 {
		t.Fatalf("report: want collapsed_edges=4 (5->1), got %+v", rep)
	}
	if rep.DirectionFlips != 1 {
		t.Fatalf("report: want direction_flips=1 (reversed loser archived), got %+v", rep)
	}
	if rep.SupersededTypeEntries != 2 {
		t.Fatalf("report: want superseded_type_entries=2 (main_subject + related_to), got %+v", rep)
	}

	if got := relConsEdges(t, lr, entNa, entCsr); got != 1 {
		t.Fatalf("after consolidation: want 1 edge on the pair, got %d", got)
	}

	// The surviving edge: facet_of, forward, unioned evidence, archived losers.
	var typ, src, tgt, evJSON, metaJSON string
	var strength *float64
	if err := lr.pool.QueryRow(ctx, `
		SELECT r.type, r.source_entity_id::text, r.target_entity_id::text,
		       r.evidence_chunk_ids::text, r.metadata::text, r.strength::float8
		FROM processing_entity_relationships r
		JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
		WHERE (r.source_entity_id = $1::uuid AND r.target_entity_id = $2::uuid)
		   OR (r.source_entity_id = $2::uuid AND r.target_entity_id = $1::uuid)`,
		entNa, entCsr).Scan(&typ, &src, &tgt, &evJSON, &metaJSON, &strength); err != nil {
		t.Fatalf("read surviving edge: %v", err)
	}
	if typ != "facet_of" {
		t.Fatalf("winning type: want facet_of, got %q", typ)
	}
	if src != entNa || tgt != entCsr {
		t.Fatalf("direction: want forward entNa->entCsr, got %s->%s", src, tgt)
	}
	var ev []string
	if err := json.Unmarshal([]byte(evJSON), &ev); err != nil {
		t.Fatalf("evidence json: %v", err)
	}
	if len(ev) != 3 {
		t.Fatalf("evidence union: want 3 chunks {chA1,chA2,chB1}, got %v", ev)
	}
	want := map[string]bool{chA[0]: true, chA[1]: true, chB[0]: true}
	for _, c := range ev {
		if !want[c] {
			t.Fatalf("evidence union: unexpected chunk %s (want exactly chA1,chA2,chB1)", c)
		}
	}
	var meta struct {
		SupersededTypes []struct {
			Type      string   `json:"type"`
			Direction string   `json:"direction"`
			Evidence  []string `json:"evidence_chunk_ids"`
			Edges     int      `json:"edges"`
		} `json:"superseded_types"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatalf("metadata json: %v", err)
	}
	if len(meta.SupersededTypes) != 2 {
		t.Fatalf("superseded_types: want 2 entries, got %s", metaJSON)
	}
	byType := map[string]json.RawMessage{}
	for _, st := range meta.SupersededTypes {
		byType[st.Type], _ = json.Marshal(st)
	}
	var ms struct {
		Type      string   `json:"type"`
		Direction string   `json:"direction"`
		Evidence  []string `json:"evidence_chunk_ids"`
		Edges     int      `json:"edges"`
	}
	if err := json.Unmarshal(byType["main_subject"], &ms); err != nil {
		t.Fatalf("main_subject archive: %v", err)
	}
	if ms.Direction != "as_is" || ms.Edges != 1 || len(ms.Evidence) != 1 || ms.Evidence[0] != chA[0] {
		t.Fatalf("main_subject archive wrong: %+v", ms)
	}
	var rt struct {
		Type      string   `json:"type"`
		Direction string   `json:"direction"`
		Evidence  []string `json:"evidence_chunk_ids"`
		Edges     int      `json:"edges"`
	}
	if err := json.Unmarshal(byType["related_to"], &rt); err != nil {
		t.Fatalf("related_to archive: %v", err)
	}
	if rt.Direction != "reversed" || rt.Edges != 1 || len(rt.Evidence) != 1 || rt.Evidence[0] != chB[1] {
		t.Fatalf("related_to archive wrong: %+v", rt)
	}

	// Read layer: documents = corpus-wide triple support from EVIDENCE
	// chunks (docA + docB = 2), not snapshot membership.
	rels, err := lr.rep.KGRelations(ctx, "", entNa, "", 2, 50)
	if err != nil {
		t.Fatalf("KGRelations: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("KGRelations: want the single aggregated edge, got %d", len(rels))
	}
	if rels[0].Documents != 2 {
		t.Fatalf("documents: want 2 (evidence spans docA+docB), got %d", rels[0].Documents)
	}
	if rels[0].Type != "facet_of" {
		t.Fatalf("KGRelations type: want facet_of, got %q", rels[0].Type)
	}

	// The loser type is no longer an edge anywhere.
	if rels2, _ := lr.rep.KGRelations(ctx, "main_subject", entNa, "", 2, 50); len(rels2) != 0 {
		t.Fatalf("main_subject must be archived, not queryable; got %d edges", len(rels2))
	}

	// Unrelated pair untouched (same row id + evidence).
	var soloEv string
	if err := lr.pool.QueryRow(ctx,
		`SELECT evidence_chunk_ids::text FROM processing_entity_relationships WHERE id = $1::uuid`,
		soloID).Scan(&soloEv); err != nil {
		t.Fatalf("read solo edge: %v", err)
	}
	if soloEv != `["`+chX[0]+`"]` {
		t.Fatalf("solo edge evidence changed: %s", soloEv)
	}

	// Inactive-snapshot pair untouched (both edges remain).
	var nInact int
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships r
		JOIN processing_snapshots s ON s.id = r.snapshot_id AND NOT s.active
		WHERE r.source_entity_id = $1::uuid`, entP).Scan(&nInact); err != nil {
		t.Fatalf("count inactive edges: %v", err)
	}
	if nInact != 2 {
		t.Fatalf("inactive edges must stay untouched, got %d", nInact)
	}

	// Re-run: NO-OP (idempotency pin).
	rep2, err := lr.rep.ConsolidateRelationsReport(ctx)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep2.MultiEdgePairs != 0 || rep2.CollapsedEdges != 0 || rep2.SupersededTypeEntries != 0 {
		t.Fatalf("re-run must be a no-op, got %+v", rep2)
	}
}
