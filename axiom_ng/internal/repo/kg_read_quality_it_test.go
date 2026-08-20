package repo

// #198 items 4+5+6 (read layer):
//   4 — confidence: a computed read-side field (persisted strength stays
//       untouched): documents support + repetition + section quality of
//       evidence. RED-FIRST: a well-corroborated (3-doc) edge must carry
//       HIGHER confidence than a single-doc high-repetition edge; an edge
//       whose evidence is frontmatter-class must score below the same-shape
//       body-evidence edge (the item-1 defense: gated classes are absent in
//       production, the term must still bite if they leak back).
//   5 — German query normalization: lowercase, hyphen/space-stripped,
//       plural-stemmed, bilingual families (theory↔theorie). RED-FIRST:
//       the external regression queries must hit — "Stakeholdertheorie"
//       against BOTH "stakeholder-theorie" and "stakeholder theory" (the
//       180-chunk EN entity — plain ILIKE misses both today),
//       "doppelte Wesentlichkeit(en)" against "wesentlichkeit"
//       (reverse containment = de-compounding), "ESG-Managementsystem"
//       against "esg-managementsystemen" (plural stem).
//   6 — corroborating_documents: the corroboration count under its honest
//       name (documents stays as deprecated alias).
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../axiom_consol_test?sslmode=disable \
//   go test ./internal/repo/ -run TestIT_KGReadQuality -v

import (
	"context"
	"math"
	"testing"
)

func kgqSeedSnapshot(t *testing.T, lr *leaseRepo, attKey string) string {
	_, snap := kgSeedSnapshot(t, lr, "KGQ Buch "+attKey, attKey)
	return snap
}

func TestIT_KGConfidence(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	snapA := kgqSeedSnapshot(t, lr, "KGQA")
	snapB := kgqSeedSnapshot(t, lr, "KGQB")
	snapC := kgqSeedSnapshot(t, lr, "KGQC")

	// Endpoints for the edges.
	nachh := "nachhaltigkeit"
	csr := "csr"
	ctrl := "controlling"
	ents := map[string]string{}
	for _, snap := range []string{snapA, snapB, snapC} {
		for _, form := range []string{nachh, csr, ctrl} {
			id, _ := kgSeedEntity(t, lr, snap, form, 2)
			ents[snap+form] = id
		}
	}

	// Edge A: 3-doc corroboration, 1 evidence each (the #185 shape).
	relIDs := []string{}
	for _, snap := range []string{snapA, snapB, snapC} {
		_, ch := kgSeedEntity(t, lr, snap, "tracer-"+snap, 1)
		relIDs = append(relIDs, kgSeedRelation(t, lr, snap, ents[snap+nachh], ents[snap+csr], "facet_of", []string{ch[0]}))
	}
	_ = relIDs
	// Edge B: single doc, 4 evidence chunks (repetition without corroboration).
	_, chB := kgSeedEntity(t, lr, snapA, "tracer-b", 1)
	_, chB2 := kgSeedEntity(t, lr, snapA, "tracer-b2", 1)
	_, chB3 := kgSeedEntity(t, lr, snapA, "tracer-b3", 1)
	_, chB4 := kgSeedEntity(t, lr, snapA, "tracer-b4", 1)
	kgSeedRelation(t, lr, snapA, ents[snapA+csr], ents[snapA+ctrl], "related_to", []string{chB[0], chB2[0], chB3[0], chB4[0]})

	// Section-quality pair: identical shape, one with frontmatter evidence.
	tocText := "| 3 | Titel des Kapitels<br>Max Mustermann | 23 |\n| 4 | Zweites Kapitel | 45 |\n| 5 | Drittes Kapitel | 67 |\n| 6 | Viertes Kapitel | 89 |"
	var tocChunk, bodyChunk string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_chunks (snapshot_id, chunk_index, text, token_count)
		VALUES ($1::uuid, 900, $2, 10) RETURNING id::text`, snapA, tocText).Scan(&tocChunk); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_chunks (snapshot_id, chunk_index, text, token_count)
		VALUES ($1::uuid, 901, 'Der CSR-Bericht beschreibt die Steuerung des Controllings im Unternehmen nachvollziehbar.', 10)
		RETURNING id::text`, snapA).Scan(&bodyChunk); err != nil {
		t.Fatal(err)
	}
	kgSeedRelation(t, lr, snapA, ents[snapA+nachh], ents[snapA+ctrl], "supports", []string{tocChunk})
	kgSeedRelation(t, lr, snapA, ents[snapA+nachh], ents[snapA+ctrl], "contrasts", []string{bodyChunk})

	rels, err := lr.rep.KGRelations(ctx, "", "", "", 2, 50)
	if err != nil {
		t.Fatalf("KGRelations: %v", err)
	}
	byType := map[string]float32{}
	docsByType := map[string]int{}
	for _, r := range rels {
		byType[r.Type] = r.Confidence
		docsByType[r.Type] = r.Documents
	}
	// Well-corroborated must outrank single-doc high-repetition.
	if byType["facet_of"] <= byType["related_to"] {
		t.Fatalf("3-doc edge confidence (%.3f) must exceed single-doc high-rep edge (%.3f)",
			byType["facet_of"], byType["related_to"])
	}
	if docsByType["facet_of"] != 3 || docsByType["related_to"] != 1 {
		t.Fatalf("documents: facet_of want 3 got %d; related_to want 1 got %d",
			docsByType["facet_of"], docsByType["related_to"])
	}
	// Documented formula: conf = 0.6*(1-1/(1+docs)) + 0.3*(1-1/rep) + 0.1*sec
	// facet_of: docs=3 rep=3 sec=1 -> 0.6*0.75+0.3*(2/3)+0.1 = 0.75
	if d := math.Abs(float64(byType["facet_of"]) - 0.75); d > 1e-6 {
		t.Fatalf("facet_of confidence: want 0.75, got %.6f", byType["facet_of"])
	}
	// related_to: docs=1 rep=4 sec=1 -> 0.6*0.5+0.3*0.75+0.1 = 0.625
	if d := math.Abs(float64(byType["related_to"]) - 0.625); d > 1e-6 {
		t.Fatalf("related_to confidence: want 0.625, got %.6f", byType["related_to"])
	}
	// Section quality: TOC-evidence edge must score 0.1 below the body twin.
	if d := float64(byType["contrasts"]) - float64(byType["supports"]); math.Abs(d-0.1) > 1e-6 {
		t.Fatalf("frontmatter evidence must cost exactly the section term (0.1), got delta %.6f", d)
	}
	// The browse ranks the corroborated triple first.
	if rels[0].Type != "facet_of" {
		t.Fatalf("top-ranked relation must be the 3-doc corroboration, got %s", rels[0].Type)
	}

	// Item 6: corroborating_documents mirrors documents under its honest name.
	for _, r := range rels {
		if r.CorroboratingDocuments != r.Documents {
			t.Fatalf("corroborating_documents must mirror documents on %s", r.Type)
		}
	}

	// Neighbors carry confidence too (1-hop surface, same formula).
	nb, err := lr.rep.KGNeighbors(ctx, ents[snapA+nachh], 2, 50)
	if err != nil {
		t.Fatalf("KGNeighbors: %v", err)
	}
	var sawConf bool
	for _, n := range nb {
		if n.Confidence > 0 {
			sawConf = true
		}
	}
	if !sawConf {
		t.Fatal("KGNeighbor edges must carry confidence > 0")
	}
}

func TestIT_KGGermanQueryNormalization(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	snap := kgqSeedSnapshot(t, lr, "KGQD")

	// The production forms the external test failed to hit.
	forms := []string{"stakeholder theory", "stakeholder-theorie", "wesentlichkeit", "esg-managementsystemen"}
	seeded := map[string]bool{}
	for _, f := range forms {
		id, _ := kgSeedEntity(t, lr, snap, f, 2)
		if id != "" {
			seeded[f] = true
		}
	}

	search := func(q string) []KGEntity {
		t.Helper()
		res, err := lr.rep.SearchKGEntities(ctx, q, 2, 50)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		return res
	}
	formsOf := func(res []KGEntity) map[string]bool {
		out := map[string]bool{}
		for _, e := range res {
			out[e.CanonicalForm] = true
		}
		return out
	}

	// "Stakeholdertheorie" (one word, DE) must hit BOTH the hyphenated DE
	// entity AND the 180-chunk EN class exemplar "stakeholder theory"
	// (bilingual family theory<->theorie after hyphen/space strip).
	hit := formsOf(search("Stakeholdertheorie"))
	if !hit["stakeholder-theorie"] || !hit["stakeholder theory"] {
		t.Fatalf("'Stakeholdertheorie' must hit both DE hyphenated and EN forms, got %v", hit)
	}
	// Reverse direction: EN query finds the DE hyphenated entity.
	hit = formsOf(search("stakeholder theory"))
	if !hit["stakeholder-theorie"] {
		t.Fatalf("'stakeholder theory' must hit 'stakeholder-theorie' (bilingual), got %v", hit)
	}
	// Compound query over a shorter stored concept: de-compounding via
	// reverse containment.
	hit = formsOf(search("doppelte Wesentlichkeit"))
	if !hit["wesentlichkeit"] {
		t.Fatalf("'doppelte Wesentlichkeit' must hit 'wesentlichkeit' (reverse containment), got %v", hit)
	}
	// Plural query still hits (stemming both sides).
	hit = formsOf(search("Doppelte Wesentlichkeiten"))
	if !hit["wesentlichkeit"] {
		t.Fatalf("'Doppelte Wesentlichkeiten' must hit 'wesentlichkeit' (plural stem), got %v", hit)
	}
	// Plural STORED form: 'esg-managementsystemen' found by the singular query.
	hit = formsOf(search("ESG-Managementsystem"))
	if !hit["esg-managementsystemen"] {
		t.Fatalf("'ESG-Managementsystem' must hit 'esg-managementsystemen' (stored plural), got %v", hit)
	}
	// Case/quote/hyphen tolerance of the same query.
	hit = formsOf(search("esg managementsystem"))
	if !hit["esg-managementsystemen"] {
		t.Fatalf("'esg managementsystem' must hit 'esg-managementsystemen', got %v", hit)
	}
	// No false positives on an unrelated term.
	if res := search("xyzzyq"); len(res) != 0 {
		t.Fatalf("unrelated query must return nothing, got %v", formsOf(res))
	}
}
