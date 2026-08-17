// #166 NACHSCHÄRFUNG: the collection-selection cascade IT. The cascade is a
// documented contract (pinned here):
//
//	doc-exclude   beats collection-include
//	collection-exclude beats doc-include (no resurrection)
//	any included collection => base is EXACTLY those docs
//	only excluded collections => base is everything minus their docs
//
// The VWL_PRÄ acceptance: one collection include, one apply, jobs exactly for
// the collection's documents (with the #159 job-existence semantics).
package repo

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// seedSelDoc seeds a document + attachment (canonical rows like a real sync)
// under one source and returns (docID, attKey, srcID).
func seedSelDoc(t *testing.T, lr *leaseRepo, srcID, docKey, attKey, hash string) (string, string) {
	t.Helper()
	ctx := context.Background()
	var docID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title, canonical_item_id)
		VALUES ($1, $2, 1, 'book', $2,
			(SELECT id FROM zotero_items WHERE source_id=$1 AND zotero_key=$2))
		RETURNING id::text`, srcID, docKey).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path, preferred)
		VALUES ($1, $2, $3, 1, $4, 'imported_file', 'application/pdf', 'x.pdf', '/tmp/x.pdf', true)`,
		srcID, docID, attKey, docKey); err != nil {
		t.Fatal(err)
	}
	return docID, attKey
}

func applySel(t *testing.T, lr *leaseRepo, srcID string, files map[string]AttachmentFileInfo, selection map[string]string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// The apply reconciles collections to the given list (missing ones are
	// marked deleted — passing nil would delete them all and empty the
	// collection expansion), so pass the fixture collections every time,
	// exactly like the real syncer does.
	colls := []zotero.CanonicalCollection{
		{Key: "VWLPRAXY", Name: "VWLPRAXY", Envelope: json.RawMessage(`{"key":"VWLPRAXY"}`)},
		{Key: "SECOND88", Name: "SECOND88", Envelope: json.RawMessage(`{"key":"SECOND88"}`)},
	}
	res, err := lr.rep.ApplyCanonicalBatch(ctx, tx, srcID, zotero.CanonicalBatch{NewVersion: 2}, colls, files, selection)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return res.Enqueued
}

func docJobCount(t *testing.T, lr *leaseRepo, docID string) int {
	t.Helper()
	var n int
	if err := lr.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ingest_jobs WHERE document_id=$1`, docID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCollectionSelectionCascadeIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	var srcID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://zotero.a2c', 'lib-1', 'srv') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	// canonical parent + attachment items (like a real sync). The parent
	// raw_data carries its collections — rebuildMemberships derives the
	// membership table from THIS on every apply, so memberships seeded
	// directly (or collections missing from raw_data) get wiped by the first
	// apply (found the hard way).
	seedItem := func(key, colls string) {
		t.Helper()
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
			VALUES ($1, $2::text, 1, 'book', NULL, concat('{"key":"', $2::text, '"}')::jsonb,
				concat('{"key":"', $2::text, '","version":1,"itemType":"book","title":"', $2::text, '","collections":[', $3::text, ']}')::jsonb)`,
			srcID, key, colls); err != nil {
			t.Fatal(err)
		}
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
			VALUES ($1, $3::text, 1, 'attachment', $2::text, concat('{"key":"', $3::text, '","data":{"path":"storage:x.pdf"}}')::jsonb,
				concat('{"key":"', $3::text, '","version":1,"itemType":"attachment","contentType":"application/pdf","filename":"x.pdf","linkMode":"imported_file"}')::jsonb)`,
			srcID, key, key+"ATT"); err != nil {
			t.Fatal(err)
		}
	}
	seedItem("PRNT1234", `"VWLPRAXY"`)
	seedItem("PRNT5678", `"VWLPRAXY","SECOND88"`)
	seedItem("PRNT9012", "")
	d1, _ := seedSelDoc(t, lr, srcID, "PRNT1234", "PRNT1234ATT", "h1") // in VWL_PRÄ
	d2, _ := seedSelDoc(t, lr, srcID, "PRNT5678", "PRNT5678ATT", "h2") // in VWL_PRÄ
	d3, _ := seedSelDoc(t, lr, srcID, "PRNT9012", "PRNT9012ATT", "h3") // NOT in any collection

	// collection VWL_PRÄ (key VWLPRAXY) with d1+d2; a second collection for d2
	for _, ck := range []string{"VWLPRAXY", "SECOND88"} {
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO zotero_collections (source_id, zotero_key, name, raw_envelope) VALUES ($1, $2, $2, '{}'::jsonb)`, srcID, ck); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range []struct{ itemKey, coll string }{
		{"PRNT1234", "VWLPRAXY"}, {"PRNT5678", "VWLPRAXY"}, {"PRNT5678", "SECOND88"},
	} {
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO zotero_item_collections (item_id, collection_id)
			SELECT i.id, c.id FROM zotero_items i, zotero_collections c
			WHERE i.source_id=$1 AND i.zotero_key=$2 AND c.zotero_key=$3`, srcID, pair.itemKey, pair.coll); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]AttachmentFileInfo{
		"PRNT1234ATT": {Exists: true, Hash: "h1"},
		"PRNT5678ATT": {Exists: true, Hash: "h2"},
		"PRNT9012ATT": {Exists: true, Hash: "h3"},
	}

	// THE VWL_PRÄ acceptance: one collection include -> apply -> jobs EXACTLY
	// for the collection's docs (d3 outside stays job-less).
	if err := lr.rep.SetCollectionSelections(ctx, []CollectionSelectionInput{{CollectionKey: "VWLPRAXY", Mode: "included"}}); err != nil {
		t.Fatal(err)
	}
	gate, err := lr.rep.ResolveEffectiveSelection(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := applySel(t, lr, srcID, files, gate); n != 2 {
		t.Fatalf("VWL_PRÄ include must enqueue exactly the 2 collection docs, got %d", n)
	}
	if c := docJobCount(t, lr, d3); c != 0 {
		t.Fatalf("doc outside selected collections must have no job, got %d", c)
	}

	// Cascade 1: doc-exclude beats collection-include.
	if err := lr.rep.SetSelections(ctx, []SelectionInput{{DocumentID: d1, Mode: "excluded"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `DELETE FROM ingest_jobs`); err != nil {
		t.Fatal(err)
	}
	gate, _ = lr.rep.ResolveEffectiveSelection(ctx, nil, nil)
	if n := applySel(t, lr, srcID, files, gate); n != 1 {
		t.Fatalf("doc-exclude must remove d1 from the collection result: %d", n)
	}
	if c := docJobCount(t, lr, d1); c != 0 {
		t.Fatalf("d1 (collection-included but doc-excluded) must have 0 jobs, got %d", c)
	}

	// Cascade 2: collection-exclude beats doc-include (no resurrection).
	if err := lr.rep.SetCollectionSelections(ctx, []CollectionSelectionInput{
		{CollectionKey: "VWLPRAXY", Mode: "default"}, // reset
		{CollectionKey: "SECOND88", Mode: "excluded"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := lr.rep.SetSelections(ctx, []SelectionInput{{DocumentID: d2, Mode: "included"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `DELETE FROM ingest_jobs`); err != nil {
		t.Fatal(err)
	}
	gate, _ = lr.rep.ResolveEffectiveSelection(ctx, nil, nil)
	// base = all docs minus SECOND88's docs = {d1,d3}; d1 is doc-excluded
	// (still removed); d2 is OUT of base and its doc-include does NOT
	// resurrect it (collection-exclude beats doc-include) -> only d3.
	if n := applySel(t, lr, srcID, files, gate); n != 1 {
		t.Fatalf("only-excluded-collections cascade: want exactly d3's job (1), got %d", n)
	}
	if c := docJobCount(t, lr, d2); c != 0 {
		t.Fatalf("collection-exclude must beat doc-include (no resurrection): d2 jobs=%d", c)
	}
	if c := docJobCount(t, lr, d3); c != 1 {
		t.Fatalf("d3 (outside SECOND88, not doc-excluded) must get its job: %d", c)
	}

	// Resolved view: nested selection visible to the client.
	v, err := lr.rep.ResolveSelectionView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Collections) != 1 || v.Collections[0].CollectionKey != "SECOND88" || v.Collections[0].Mode != "excluded" {
		t.Fatalf("resolved collections wrong: %+v", v.Collections)
	}
	if len(v.Collections[0].DocumentIDs) != 1 || v.Collections[0].DocumentIDs[0] != d2 {
		t.Fatalf("SECOND88 must resolve to d2: %+v", v.Collections[0])
	}
	keys := make([]string, 0, len(v.Documents))
	for k := range v.Documents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 2 { // d1 excluded + d2 included
		t.Fatalf("resolved document rows wrong: %v", keys)
	}
}
