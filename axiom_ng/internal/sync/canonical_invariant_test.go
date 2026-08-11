package sync

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

func runCanon(t *testing.T, src zotero.Source, d *db.DB) (CanonicalResult, error) {
	t.Helper()
	svc := New(src, repo.New(d.Pool()), baseOf(src), "users/0", log.Default())
	return svc.RunCanonical(context.Background())
}

func baseOf(src zotero.Source) string {
	if cf, ok := src.(*canonicalFake); ok {
		return cf.baseURL
	}
	if ss, ok := src.(*errDeletedSource); ok {
		return ss.baseURL
	}
	return "http://test/api"
}

func countActiveFor(t *testing.T, d *db.DB, table, sourceID string) int {
	t.Helper()
	var n int
	err := d.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM `+table+` WHERE source_id=$1 AND deleted=false`, sourceID).Scan(&n)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// itemEnv builds the raw envelope from data for a canonical item.
func itemEnv(key, itemType, parent string, data map[string]any) zotero.CanonicalItem {
	dd := map[string]any{"key": key, "version": 1, "itemType": itemType}
	if parent != "" {
		dd["parentItem"] = parent
	}
	for k, v := range data {
		dd[k] = v
	}
	envB, _ := json.Marshal(map[string]any{"key": key, "version": 1, "data": dd})
	dataB, _ := json.Marshal(dd)
	return zotero.CanonicalItem{Key: key, Version: 1, ItemType: itemType, ParentKey: parent, Envelope: envB, Data: dataB}
}

func bookEnv(key, title string, extra map[string]any) zotero.CanonicalItem {
	m := map[string]any{"title": title, "date": "2020", "DOI": "10.1/" + key}
	for k, v := range extra {
		m[k] = v
	}
	return itemEnv(key, "book", "", m)
}

func pdfAttEnv(key, parent, path string) zotero.CanonicalItem {
	envB, _ := json.Marshal(map[string]any{
		"key": key, "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + path}},
		"data":  map[string]any{"key": key, "version": 1, "itemType": "attachment", "parentItem": parent, "contentType": "application/pdf", "filename": "a.pdf"},
	})
	dataB, _ := json.Marshal(map[string]any{"key": key, "version": 1, "itemType": "attachment", "parentItem": parent, "contentType": "application/pdf", "filename": "a.pdf"})
	return zotero.CanonicalItem{Key: key, Version: 1, ItemType: "attachment", ParentKey: parent, Envelope: envB, Data: dataB}
}

// TestCanonicalFullThenEmptyDeltaKeepsItems: a full sync populates; a subsequent
// empty delta must NOT delete the canonical items.
func TestCanonicalFullThenEmptyDeltaKeepsItems(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", pdf)}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if n := countActiveFor(t, d, "zotero_items", res1.SourceID); n != 2 {
		t.Fatalf("after full sync: %d active items, want 2", n)
	}
	// Empty delta (same version, no new items, no deletes).
	src.deleteEvents = []zotero.DeleteEvent{}
	res2, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if n := countActiveFor(t, d, "zotero_items", res2.SourceID); n != 2 {
		t.Errorf("empty delta deleted canonical items: want 2 active, got %d", n)
	}
}

// TestCanonicalMetadataDeltaNoNewJob: a pure metadata change of a parent updates
// title/DOI but, with an unchanged attachment hash, enqueues no new job.
func TestCanonicalMetadataDeltaNoNewJob(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Old Title", nil), pdfAttEnv("A1", "B1", pdf)}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	jobsBefore, err := repo.New(d.Pool()).CountJobsForSource(ctx, res1.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	// Metadata-only change: same attachment, new title/DOI.
	src.version = 11
	src.items = []zotero.CanonicalItem{bookEnv("B1", "New Title", map[string]any{"DOI": "10.1/new"}), pdfAttEnv("A1", "B1", pdf)}
	res2, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	jobsAfter, err := repo.New(d.Pool()).CountJobsForSource(ctx, res2.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	if res2.NewVersion != 11 {
		t.Errorf("cursor = %d, want 11", res2.NewVersion)
	}
	if jobsAfter != jobsBefore {
		t.Errorf("metadata-only change created a new job: before=%d after=%d", jobsBefore, jobsAfter)
	}
	// Title projection updated.
	var title string
	if err := d.Pool().QueryRow(ctx, `SELECT title FROM zotero_documents WHERE source_id=$1 AND zotero_key='B1'`, res2.SourceID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "New Title" {
		t.Errorf("title projection = %q, want New Title", title)
	}
}

// TestCanonicalMissingFilePersistsFailedJob: a preferred attachment whose file is
// missing yields a FILE_NOT_FOUND failed job and the item stays active (not
// deleted), with a defined cursor advance.
func TestCanonicalMissingFilePersistsFailedJob(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	missing := t.TempDir() + "/missing.pdf" // does not exist
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 5}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", missing)}
	res, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	// Failed job with FILE_NOT_FOUND for A1.
	jobs, err := repo.New(d.Pool()).ListJobsByAttachment(ctx, attIDByKey(t, d, res.SourceID, "A1"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range jobs {
		if j.ErrorCode != nil && *j.ErrorCode == "FILE_NOT_FOUND" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a FILE_NOT_FOUND failed job for missing attachment")
	}
	if res.NewVersion != 5 {
		t.Errorf("cursor did not advance with recorded failure: %d", res.NewVersion)
	}
}

// TestCanonicalMalformedEnvelopeAbortsNoCursor: a non-decodable item envelope
// aborts the sync and does not advance the cursor.
func TestCanonicalMalformedEnvelopeAbortsNoCursor(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 9}
	// A book + attachment + a malformed envelope.
	bad := zotero.CanonicalItem{Key: "X1", Version: 1, ItemType: "", Envelope: json.RawMessage(`{broken`)}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", pdf), bad}
	if _, err := runCanon(t, src, d); err == nil {
		t.Fatal("expected error on malformed envelope; cursor must not advance")
	}
}

// TestCanonicalFreshDBPreferredWithStats: a fresh DB (unique source) yields
// exactly 16 preferred attachments across the real-style corpus with hash/size.
// Here we model two documents with PDF attachments and assert 2 preferred have
// content_hash + file_size set and siblings are not preferred.
func TestCanonicalFreshDBPreferredWithStats(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdfA := makePdf(t, "a")
	pdfB := makePdf(t, "b")
	epubC := t.TempDir() + "/c.epub"
	os.WriteFile(epubC, []byte("c"), 0o600)

	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 3}
	src.items = []zotero.CanonicalItem{
		bookEnv("B1", "Book One", nil), pdfAttEnv("A1", "B1", pdfA),
		bookEnv("B2", "Book Two", nil), pdfAttEnv("A2", "B2", pdfB), pdfAttEnv("A2b", "B2", pdfB),
		// a third doc whose only attachment is an EPUB (preferred rule).
		bookEnv("B3", "Book Three", nil),
	}
	// B3 gets an EPUB attachment (preferred) via a separate path would need it in items.
	epubItem := itemEnv("A3", "attachment", "B3", map[string]any{"contentType": "application/epub+zip", "filename": "c.epub"})
	epubEnv, _ := json.Marshal(map[string]any{"key": "A3", "version": 1, "links": map[string]any{"enclosure": map[string]any{"href": "file://" + epubC}}, "data": map[string]any{"key": "A3", "version": 1, "itemType": "attachment", "parentItem": "B3", "contentType": "application/epub+zip", "filename": "c.epub"}})
	epubItem.Envelope = epubEnv
	src.items = append(src.items, epubItem)

	res, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	// B1: A1 preferred. B2: exactly one of A2/A2b preferred (sibling false).
	// B3: A3 (epub) preferred.
	sourceID := res.SourceID
	var prefCount, withHash, withSize, b2pref int
	_ = d.Pool().QueryRow(ctx, `SELECT count(*) FROM zotero_attachments WHERE source_id=$1 AND preferred=true`, sourceID).Scan(&prefCount)
	_ = d.Pool().QueryRow(ctx, `SELECT count(*) FROM zotero_attachments WHERE source_id=$1 AND preferred=true AND content_hash IS NOT NULL`, sourceID).Scan(&withHash)
	_ = d.Pool().QueryRow(ctx, `SELECT count(*) FROM zotero_attachments WHERE source_id=$1 AND preferred=true AND file_size IS NOT NULL`, sourceID).Scan(&withSize)
	// Exactly one preferred attachment for document B2 (A2 or A2b), not both.
	_ = d.Pool().QueryRow(ctx, `SELECT count(*) FROM zotero_attachments a WHERE a.source_id=$1 AND a.preferred=true AND a.document_id=(SELECT id FROM zotero_documents WHERE source_id=$1 AND zotero_key='B2')`, sourceID).Scan(&b2pref)
	if prefCount != 3 {
		t.Errorf("preferred = %d, want 3 (one per doc)", prefCount)
	}
	if withHash != 3 || withSize != 3 {
		t.Errorf("preferred with hash=%d size=%d, want 3 each", withHash, withSize)
	}
	if b2pref != 1 {
		t.Errorf("document B2 must have exactly 1 preferred (sibling cleared), got %d", b2pref)
	}
	if res.Documents != 3 {
		t.Errorf("doc projections = %d, want 3", res.Documents)
	}
}
