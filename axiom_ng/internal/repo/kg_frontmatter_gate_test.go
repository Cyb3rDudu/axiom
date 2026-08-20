package repo

// #198 item 1: the persist-time frontmatter gate. White-box test of the
// pure filter (no DB) + end-to-end proof through PersistResult (the real
// harness): gated evidence never reaches the KG tables, body evidence
// persists untouched.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

const (
	gateBodyText   = "Die Studie zeigt, dass nachhaltige Unternehmensführung im Zeitraum 2010 bis 2020 deutlich an Bedeutung gewonnen hat. Kapitalmärkte belohnen Transparenz."
	gateTOCText    = "| 3 | Titel des Kapitels<br>Max Mustermann | 23 |\n| 4 | Zweites Kapitel | 45 |\n| 5 | Drittes Kapitel<br>Noch ein Autor | 67 |\n| 6 | Viertes Kapitel | 89 |"
	gateBylineText = "### <span id=\"page-108-0\"></span>**Nachhaltigkeit und Digitalisierung als Chance für Unternehmen**\n\n**4**\n\nAnabel Ternès"
)

func gateResult() *processor.Result {
	return &processor.Result{
		Chunks: []processor.Chunk{
			{Ref: "body-1", Index: 0, Text: gateBodyText, TokenCount: 20},
			{Ref: "toc-1", Index: 1, Text: gateTOCText, TokenCount: 20},
			{Ref: "byline-1", Index: 2, Text: gateBylineText, TokenCount: 10},
		},
		Entities: []processor.Entity{
			{Ref: "e-body", Text: "Nachhaltigkeit", CanonicalForm: "nachhaltigkeit", Type: "CONCEPT",
				Mentions: []processor.EntityMention{{ChunkRef: "body-1", StartChar: 10, EndChar: 24, Confidence: 0.9}}},
			{Ref: "e-mixed", Text: "Kapitalmarkt", CanonicalForm: "kapitalmarkt", Type: "CONCEPT",
				Mentions: []processor.EntityMention{
					{ChunkRef: "body-1", StartChar: 0, EndChar: 12, Confidence: 0.9},
					{ChunkRef: "toc-1", StartChar: 0, EndChar: 12, Confidence: 0.9},
				}},
			{Ref: "e-toc-only", Text: "Zweites Kapitel", CanonicalForm: "zweites kapitel", Type: "CONCEPT",
				Mentions: []processor.EntityMention{{ChunkRef: "toc-1", StartChar: 0, EndChar: 14, Confidence: 0.9}}},
			{Ref: "e-byline", Text: "Anabel Ternès", CanonicalForm: "anabel ternès", Type: "PERSON",
				Mentions: []processor.EntityMention{{ChunkRef: "byline-1", StartChar: 0, EndChar: 13, Confidence: 0.9}}},
		},
		EntityRelationships: []processor.EntityRelationship{
			// keep: body evidence, live endpoints
			{SourceEntityRef: "e-body", TargetEntityRef: "e-mixed", Type: "related_to", EvidenceChunkRefs: []string{"body-1"}},
			// drop: endpoint e-toc-only died
			{SourceEntityRef: "e-body", TargetEntityRef: "e-toc-only", Type: "facet_of", EvidenceChunkRefs: []string{"body-1", "toc-1"}},
			// drop: all evidence gated (the Fifka/Weber class)
			{SourceEntityRef: "e-byline", TargetEntityRef: "e-body", Type: "named_after", EvidenceChunkRefs: []string{"byline-1"}},
			// keep: mixed evidence survives with gated refs STRIPPED
			{SourceEntityRef: "e-mixed", TargetEntityRef: "e-body", Type: "main_subject", EvidenceChunkRefs: []string{"toc-1", "body-1"}},
		},
	}
}

func TestGateKgFrontmatterFilter(t *testing.T) {
	res := gateResult()
	st := gateKgFrontmatter(res)

	if st.EntitiesDropped != 2 || st.EntityMentionsDropped != 3 || st.RelationshipsDropped != 2 {
		t.Fatalf("gate stats wrong: %+v", st)
	}
	if len(res.Entities) != 2 {
		t.Fatalf("2 entities must survive (body + mixed), got %d", len(res.Entities))
	}
	for _, e := range res.Entities {
		for _, m := range e.Mentions {
			if m.ChunkRef == "toc-1" || m.ChunkRef == "byline-1" {
				t.Fatalf("gated mention survived on %s", e.Ref)
			}
		}
	}
	if len(res.EntityRelationships) != 2 {
		t.Fatalf("2 relationships must survive, got %d", len(res.EntityRelationships))
	}
	for _, rel := range res.EntityRelationships {
		for _, ref := range rel.EvidenceChunkRefs {
			if ref == "toc-1" || ref == "byline-1" {
				t.Fatalf("gated evidence ref survived on %s->%s", rel.SourceEntityRef, rel.TargetEntityRef)
			}
		}
	}
	// Idempotent: a second pass drops nothing.
	st2 := gateKgFrontmatter(res)
	if st2 != (FrontmatterGateStats{}) {
		t.Fatalf("second gate pass must be a no-op, got %+v", st2)
	}
	// Ungated result: gate is a no-op (fast path, no allocation of drops).
	clean := &processor.Result{
		Chunks:   []processor.Chunk{{Ref: "c1", Index: 0, Text: "Plain body text about companies.", TokenCount: 6}},
		Entities: []processor.Entity{{Ref: "e1", Text: "Company", Mentions: []processor.EntityMention{{ChunkRef: "c1"}}}},
	}
	if st := gateKgFrontmatter(clean); st != (FrontmatterGateStats{}) || len(clean.Entities) != 1 {
		t.Fatalf("clean result must pass unchanged: %+v", st)
	}
}

// End-to-end: PersistResult with gated evidence — the KG tables must never
// see it, while body evidence persists and verifyCounts stays consistent.
func TestPersistFrontmatterGateEndToEnd(t *testing.T) {
	h := newPersistHarness(t, "fmgate")
	const dims = 3
	raw := h.validResultRaw(dims)
	raw.Chunks = append(raw.Chunks,
		processor.Chunk{Ref: "toc-1", Index: 1, Text: gateTOCText, TokenCount: 20,
			Locator:   &processor.Locator{Type: "page_span", PhysicalPageStart: ptrInt(2), PhysicalPageEnd: ptrInt(2), PageLabelStart: "3", PageLabelEnd: "3", Source: "marker_paginate", PageSource: "pdf_label_sane"},
			Structure: processor.ChunkStructure{SectionTitles: []string{"Inhaltsverzeichnis"}}},
		processor.Chunk{Ref: "byline-1", Index: 2, Text: gateBylineText, TokenCount: 10,
			Locator: &processor.Locator{Type: "page_span", PhysicalPageStart: ptrInt(3), PhysicalPageEnd: ptrInt(3), PageLabelStart: "4", PageLabelEnd: "4", Source: "marker_paginate", PageSource: "pdf_label_sane"}},
	)
	raw.Entities = append(raw.Entities,
		processor.Entity{Ref: "e-toc-only", Text: "Zweites Kapitel", CanonicalForm: "zweites kapitel",
			Mentions: []processor.EntityMention{{ChunkRef: "toc-1", StartChar: 0, EndChar: 14, Confidence: 0.9}}},
		processor.Entity{Ref: "e-byline", Text: "Anabel Ternès", CanonicalForm: "anabel ternès",
			Mentions: []processor.EntityMention{{ChunkRef: "byline-1", StartChar: 0, EndChar: 13, Confidence: 0.9}}},
	)
	raw.EntityRelationships = append(raw.EntityRelationships,
		processor.EntityRelationship{SourceEntityRef: "e-byline", TargetEntityRef: "entity-0001", Type: "named_after",
			EvidenceChunkRefs: []string{"byline-1"}, Extractor: "test"},
	)
	raw.Stats.Chunks, raw.Stats.Entities, raw.Stats.EntityRelationships = 3, 3, 2

	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	snapID, err := h.persist(t, b, dims, markdownArtifact())
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	ctx := context.Background()

	// The gated entity ("Anabel Ternès" byline-only, "Zweites Kapitel"
	// TOC-only) must NOT be in the KG; the body entity must be.
	var n int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE snapshot_id=$1 AND ref IN ('e-toc-only','e-byline')`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("gated entities must never persist, got %d", n)
	}
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_entities WHERE snapshot_id=$1 AND ref='entity-0001'`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("body entity must persist, got %d", n)
	}
	// The named_after edge (byline evidence) must not exist.
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships
		WHERE snapshot_id=$1 AND type='named_after'`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("gated-evidence relationship must never persist, got %d", n)
	}
	// The body relation from validResultRaw persists.
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entity_relationships
		WHERE snapshot_id=$1 AND type='related_to'`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("body relationship must persist, got %d", n)
	}
	// Gated CHUNKS still persist (retrieval keeps them; only KG evidence is gated).
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_chunks WHERE snapshot_id=$1`, snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("all 3 chunks (incl. gated) must persist for retrieval, got %d", n)
	}
}
