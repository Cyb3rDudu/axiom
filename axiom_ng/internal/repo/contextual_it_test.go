package repo

// #255 contextual source class — integration proofs through the REAL chain:
//
//	projection: apply recomputes citation_class from memberships + tags
//	            (collection rule, tag override, direction, reversibility,
//	            and the PASSENGER proof: non-ruled documents stay citable)
//	boot gate:  ResolveContextualRules is LOUD on unknown path/tag/deleted
//	claim gate: contextual documents claim with the KG extraction flags
//	            cleared from the frozen profile (hash follows)
//	persist:    contextual documents contribute ZERO entities/relationships
//	            while chunks + embeddings persist (the equal-rank substrate)
//
// Runs only against a dedicated *_test database (AXIOM_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// ctxSeed seeds the canonical chain for one document like a real sync had:
// parent item (with collections + tags in raw_data) + attachment item +
// document/attachment projections. Returns the document id.
func ctxSeed(t *testing.T, lr *leaseRepo, srcID, docKey string, collections, tagsJSON string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1, $2::text, 1, 'book', NULL, concat('{"key":"', $2::text, '"}')::jsonb,
			concat('{"key":"', $2::text, '","version":1,"itemType":"book","title":"', $2::text,
				'","collections":[', $3::text, '],"tags":', $4::text, '}')::jsonb)`,
		srcID, docKey, collections, tagsJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1, $3::text, 1, 'attachment', $2::text, concat('{"key":"', $3::text, '","data":{"path":"storage:x.pdf"}}')::jsonb,
			concat('{"key":"', $3::text, '","version":1,"itemType":"attachment","contentType":"application/pdf","filename":"x.pdf","linkMode":"imported_file"}')::jsonb)`,
		srcID, docKey, docKey+"ATT"); err != nil {
		t.Fatal(err)
	}
	var docID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title, tags, canonical_item_id)
		VALUES ($1, $2, 1, 'book', $2, $3::jsonb,
			(SELECT id FROM zotero_items WHERE source_id=$1 AND zotero_key=$2))
		ON CONFLICT (source_id, zotero_key) DO UPDATE SET canonical_item_id=EXCLUDED.canonical_item_id, deleted=false
		RETURNING id::text`, srcID, docKey, tagsJSON).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path, preferred, content_hash)
		VALUES ($1, $2, $3, 1, $4, 'imported_file', 'application/pdf', 'x.pdf', '/tmp/x.pdf', true, 'sha256:ctx')`,
		srcID, docID, docKey+"ATT", docKey); err != nil {
		t.Fatal(err)
	}
	return docID
}

// ctxApply runs one canonical apply (projections + memberships + citation
// class) for the seeded world under the given rules.
func ctxApply(t *testing.T, lr *leaseRepo, srcID string, rules ContextualRules) {
	t.Helper()
	ctx := context.Background()
	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	colls := []zotero.CanonicalCollection{
		{Key: "VWLKEY", Name: "VWL", Envelope: []byte(`{"key":"VWLKEY"}`)},
		{Key: "LECTKEY", Name: "Lectures", ParentKey: "VWLKEY", Envelope: []byte(`{"key":"LECTKEY"}`)},
		{Key: "OTHERKEY", Name: "HA", Envelope: []byte(`{"key":"OTHERKEY"}`)},
	}
	files := map[string]AttachmentFileInfo{
		"CTXDOC1ATT": {LocalPath: "/tmp/x.pdf", Exists: true, Hash: "sha256:ctx1"},
		"CTXDOC2ATT": {LocalPath: "/tmp/x.pdf", Exists: true, Hash: "sha256:ctx2"},
		"CTXDOC3ATT": {LocalPath: "/tmp/x.pdf", Exists: true, Hash: "sha256:ctx3"},
	}
	if _, err := lr.rep.ApplyCanonicalBatch(ctx, tx, srcID, zotero.CanonicalBatch{NewVersion: 2}, colls, files, nil, rules); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func ctxClass(t *testing.T, lr *leaseRepo, docID string) string {
	t.Helper()
	var cls string
	if err := lr.pool.QueryRow(context.Background(),
		`SELECT citation_class FROM zotero_documents WHERE id=$1`, docID).Scan(&cls); err != nil {
		t.Fatal(err)
	}
	return cls
}

func TestContextualProjectionIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	var srcID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://zotero.ctx', 'lib-1', 'srv') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}

	lect := ctxSeed(t, lr, srcID, "CTXDOC1", `"LECTKEY"`, `[]`)                        // in VWL/Lectures
	passenger := ctxSeed(t, lr, srcID, "CTXDOC2", `"OTHERKEY"`, `[]`)                  // in HA — NOT ruled
	tagged := ctxSeed(t, lr, srcID, "CTXDOC3", `"OTHERKEY"`, `[{"tag":"contextual"}]`) // outlier: tag, not collection

	rules := ContextualRules{CollectionKeys: []string{"LECTKEY"}, Tags: []string{"contextual"}}
	ctxApply(t, lr, srcID, rules)

	if got := ctxClass(t, lr, lect); got != "contextual" {
		t.Fatalf("Lectures member must be contextual, got %q", got)
	}
	// Passenger proof: the non-ruled document is untouched by the class rule.
	if got := ctxClass(t, lr, passenger); got != "citable" {
		t.Fatalf("non-Lectures passenger must stay citable, got %q", got)
	}
	// Tag override marks a non-member contextual.
	if got := ctxClass(t, lr, tagged); got != "contextual" {
		t.Fatalf("tagged outlier must be contextual, got %q", got)
	}
	// Searchable: the contextual document got its ingest job like everyone
	// else (equal-rank substrate — no selection gate ever sees the class).
	var n int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM ingest_jobs j JOIN zotero_documents d ON d.id=j.document_id
		 WHERE d.zotero_key='CTXDOC1' AND j.status='pending'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("contextual document must be enqueued (searchable) like any other")
	}
	// Hit hydration: the search/passage source block query carries the
	// class — a contextual hit reports contextual, the passenger citable.
	meta, err := lr.rep.DocumentMetaByIDs(ctx, []string{lect, passenger})
	if err != nil {
		t.Fatal(err)
	}
	if cc := meta[lect].CitationClass; cc != "contextual" {
		t.Fatalf("hydrated contextual hit must report contextual, got %q", cc)
	}
	if cc := meta[passenger].CitationClass; cc != "citable" {
		t.Fatalf("hydrated passenger must report citable, got %q", cc)
	}

	// Reversibility: the doc leaves the Lectures collection (raw_data no
	// longer references it; the memberships rebuild drops it) → the next
	// sync recomputes citable. The tag rule is removed alongside.
	if _, err := lr.pool.Exec(ctx, `
		UPDATE zotero_items SET raw_data = raw_data - 'collections' || '{"collections":["OTHERKEY"]}'::jsonb
		WHERE source_id=$1 AND zotero_key='CTXDOC1'`, srcID); err != nil {
		t.Fatal(err)
	}
	ctxApply(t, lr, srcID, ContextualRules{CollectionKeys: []string{"LECTKEY"}})
	if got := ctxClass(t, lr, lect); got != "citable" {
		t.Fatalf("doc moved out of Lectures must recompute citable, got %q", got)
	}
	// Tag direction: a tag never makes a citable doc citable-override — here
	// the tag rule is gone, and the tagged outlier recomputes citable too
	// (the ONLY direction of both rules is toward contextual).
	if got := ctxClass(t, lr, tagged); got != "citable" {
		t.Fatalf("tag rule removed → outlier recomputes citable, got %q", got)
	}

	// Empty rules recompute everything citable (fresh-install semantics).
	ctxApply(t, lr, srcID, ContextualRules{})
	if got := ctxClass(t, lr, lect); got != "citable" {
		t.Fatalf("no rules must compute citable, got %q", got)
	}
}

func TestResolveContextualRulesIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	var srcID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://zotero.res', 'lib-1', 'srv') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	seedColl := func(key, name, parent string, deleted bool) {
		t.Helper()
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO zotero_collections (source_id, zotero_key, name, parent_key, raw_envelope, deleted)
			VALUES ($1,$2,$3,$4,'{}'::jsonb,$5)`, srcID, key, name, parent, deleted); err != nil {
			t.Fatal(err)
		}
	}
	seedColl("VWLKEY", "VWL", "", false)
	seedColl("LECTKEY", "Lectures", "VWLKEY", false)
	seedColl("DEADKEY", "OldLectures", "VWLKEY", true)
	ctxSeed(t, lr, srcID, "TAGDOC1", `[]`, `[{"tag":"Vorlesung"}]`)

	// Happy path: full path resolves to the stable key; tag exists.
	rules, err := lr.rep.ResolveContextualRules(ctx, []string{"VWL/Lectures"}, []string{"Vorlesung"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(rules.CollectionKeys) != 1 || rules.CollectionKeys[0] != "LECTKEY" {
		t.Fatalf("path must stabilize on zotero_key LECTKEY, got %v", rules.CollectionKeys)
	}
	if len(rules.Tags) != 1 || rules.Tags[0] != "Vorlesung" {
		t.Fatalf("tag rule wrong: %v", rules.Tags)
	}

	// LOUD, never silent: unknown path / segment / tag / deleted-only match.
	for _, tc := range []struct {
		paths, tags []string
		want        string
	}{
		{[]string{"VWL/Nope"}, nil, "VWL/Nope"},
		{[]string{"Nope"}, nil, "Nope"},
		{[]string{"VWL//X"}, nil, "empty segment"},
		{[]string{"VWL/OldLectures"}, nil, "deleted"},
		{nil, []string{"NoSuchTag"}, "NoSuchTag"},
	} {
		_, err := lr.rep.ResolveContextualRules(ctx, tc.paths, tc.tags)
		if err == nil {
			t.Fatalf("paths=%v tags=%v must fail loudly, resolved fine", tc.paths, tc.tags)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("error must name %q, got: %v", tc.want, err)
		}
	}
}

// fullProfile mirrors the production default profile (extraction ON) so the
// claim gate's clearing is observable.
func fullProfile() []byte {
	return []byte(`{"profile":"full-rag-v1","extract_entities":true,"extract_relationships":true,"compute_dense_embeddings":true}`)
}

func TestClaimContextualKGGateIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	hash := "sha256:claimctx"
	_, jobCitable := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost/ctx1", libraryID: "users/0",
		docKey: "CITDOC", attKey: "CITDOC", contentHash: &hash, preferred: true,
	}, "pending", 3)
	_, jobContextual := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost/ctx2", libraryID: "users/0",
		docKey: "CTXDOC", attKey: "CTXDOC", contentHash: &hash, preferred: true,
	}, "pending", 3)
	// The lecture doc is contextual; the passenger stays citable.
	if _, err := lr.pool.Exec(ctx,
		`UPDATE zotero_documents SET citation_class='contextual' WHERE zotero_key='CTXDOC'`); err != nil {
		t.Fatal(err)
	}

	opts := ClaimOptions{WorkerID: "w-ctx", LeaseDuration: 120 * time.Second, Profile: fullProfile()}
	claimed := map[string]*ClaimedJob{}
	for range 2 {
		cj, err := lr.rep.ClaimNextJob(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		if cj == nil {
			t.Fatal("expected two claims")
		}
		claimed[cj.JobID] = cj
	}

	type frozenProc struct {
		ExtractEntities      bool `json:"extract_entities"`
		ExtractRelationships bool `json:"extract_relationships"`
	}
	readProfile := func(jobID string) frozenProc {
		var p frozenProc
		var raw []byte
		if err := lr.pool.QueryRow(ctx,
			`SELECT processing_profile::text FROM ingest_jobs WHERE id=$1`, jobID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Passenger: citable document keeps the full profile.
	if p := readProfile(jobCitable); !p.ExtractEntities || !p.ExtractRelationships {
		t.Fatalf("citable passenger must keep extraction flags, got %+v", p)
	}
	// Contextual: frozen profile has the KG flags cleared (stored profile,
	// emitted request and hash all reflect the gate).
	if p := readProfile(jobContextual); p.ExtractEntities || p.ExtractRelationships {
		t.Fatalf("contextual claim must clear extraction flags, got %+v", p)
	}
	if claimed[jobContextual] != nil {
		var fp FrozenProcessing
		if err := json.Unmarshal(claimed[jobContextual].Profile, &fp); err != nil {
			t.Fatal(err)
		}
		if fp.ExtractEntities || fp.ExtractRelationships {
			t.Fatalf("claimed profile must be gated, got %+v", fp)
		}
		if claimed[jobContextual].ProfileHash == claimed[jobCitable].ProfileHash {
			t.Fatal("gated profile must hash differently from the full profile")
		}
	}
}

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
