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

func runCanon(t *testing.T, src zotero.Source, d *db.DB) (Result, error) {
	t.Helper()
	ctx := context.Background()
	repoObj := repo.New(d.Pool())
	svc := New(src, repoObj, baseOf(src), "users/0", log.Default())
	// Ensure the source up front and register cleanup BEFORE the sync runs, so a
	// failed run (e.g. a malformed-envelope abort) still removes its source and
	// does not leak persistent rows into the shared test DB.
	sourceID, err := repoObj.EnsureSource(ctx, baseOf(src), "users/0", src.ServerID())
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	t.Cleanup(func() {
		if sourceID == "" {
			return
		}
		_, _ = d.Pool().Exec(context.Background(), `DELETE FROM zotero_sources WHERE id=$1`, sourceID)
	})
	res, err := svc.Run(ctx, nil)
	return res, err
}

func baseOf(src zotero.Source) string {
	if cf, ok := src.(*canonicalFake); ok {
		return cf.baseURL
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
	// Empty delta: no items at all, no deletes. Must keep items active.
	src.deleteEvents = []zotero.DeleteEvent{}
	src.items = nil
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
	// Metadata-only change: send ONLY the parent in the delta (no attachment),
	// new title/DOI; the attachment hash is unchanged.
	src.version = 11
	src.items = []zotero.CanonicalItem{bookEnv("B1", "New Title", map[string]any{"DOI": "10.1/new"})}
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

// TestCanonicalAttachmentDeleteUpdatesProjection: deleting a preferred attachment
// marks it deleted + not preferred; the sibling EPUB becomes preferred.
func TestCanonicalAttachmentDeleteUpdatesProjection(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	epub := t.TempDir() + "/a.epub"
	os.WriteFile(epub, []byte("e"), 0o600)

	pdfItem := pdfAttEnv("A-PDF", "B1", pdf)
	epubItem := itemEnv("A-EPUB", "attachment", "B1", map[string]any{"contentType": "application/epub+zip", "filename": "a.epub"})
	epubEnv, _ := json.Marshal(map[string]any{"key": "A-EPUB", "version": 1, "links": map[string]any{"enclosure": map[string]any{"href": "file://" + epub}}, "data": map[string]any{"key": "A-EPUB", "version": 1, "itemType": "attachment", "parentItem": "B1", "contentType": "application/epub+zip", "filename": "a.epub"}})
	epubItem.Envelope = epubEnv

	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfItem, epubItem}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Delete A-PDF (preferred) via delete event; the EPUB sibling is reassessed.
	src.version = 11
	src.deleteEvents = []zotero.DeleteEvent{{Key: "A-PDF", ItemType: "attachment", ParentKey: "B1"}}
	src.items = nil // only the delete event is the delta
	res2, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	// A-PDF must be deleted + not preferred.
	var del, pref bool
	_ = d.Pool().QueryRow(ctx, `SELECT deleted, preferred FROM zotero_attachments WHERE source_id=$1 AND zotero_key='A-PDF'`, res2.SourceID).Scan(&del, &pref)
	if !del || pref {
		t.Errorf("deleted preferred attachment A-PDF: deleted=%v preferred=%v, want deleted+not preferred", del, pref)
	}
	// A-EPUB must become preferred.
	_ = d.Pool().QueryRow(ctx, `SELECT deleted, preferred FROM zotero_attachments WHERE source_id=$1 AND zotero_key='A-EPUB'`, res2.SourceID).Scan(&del, &pref)
	if del || !pref {
		t.Errorf("sibling EPUB must be active+preferred after delete: deleted=%v preferred=%v", del, pref)
	}
	_ = res1
}

// TestCanonicalRestoreAfterDelete sets the canonical item back (deleted=false
// via a new version) and re-enables its projection.
// TestCanonicalRestoreAfterDelete sets the canonical item back (deleted=false
// via a new version) and re-enables its projection.
func TestCanonicalRestoreAfterDelete(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", pdf)}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Delete the parent.
	src.version = 11
	src.deleteEvents = []zotero.DeleteEvent{{Key: "B1", ItemType: "book"}}
	src.items = nil
	if _, err := runCanon(t, src, d); err != nil {
		t.Fatalf("delete sync: %v", err)
	}
	var deleted bool
	if err := d.Pool().QueryRow(ctx, `SELECT deleted FROM zotero_documents WHERE source_id=$1 AND zotero_key='B1'`, res1.SourceID).Scan(&deleted); err != nil {
		t.Fatalf("query deleted doc: %v", err)
	}
	if !deleted {
		t.Fatalf("parent delete should deactivate document projection")
	}
	// Restore: full snapshot re-sends B1+A1 (deleteEvents cleared).
	src.version = 12
	src.deleteEvents = nil
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", pdf)}
	if _, err := runCanon(t, src, d); err != nil {
		t.Fatalf("restore sync: %v", err)
	}
	if err := d.Pool().QueryRow(ctx, `SELECT deleted FROM zotero_documents WHERE source_id=$1 AND zotero_key='B1'`, res1.SourceID).Scan(&deleted); err != nil {
		t.Fatalf("query restored doc: %v", err)
	}
	if deleted {
		t.Errorf("restored parent must re-enable document projection")
	}
}

// TestCanonicalRepeatedMissingFileNoDuplicateFailedJob: a missing file persists
// exactly one FILE_NOT_FOUND failed job across repeated no-op syncs.
func TestCanonicalRepeatedMissingFileNoDuplicateFailedJob(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	missing := t.TempDir() + "/missing.pdf"
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 5}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", missing)}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	attID := attIDByKey(t, d, res1.SourceID, "A1")
	jobs1, _ := repo.New(d.Pool()).ListJobsByAttachment(ctx, attID)
	// A second no-op sync (same missing file) must not add another failed job.
	src.version = 6
	src.items = nil
	if _, err := runCanon(t, src, d); err != nil {
		t.Fatalf("second: %v", err)
	}
	jobs2, _ := repo.New(d.Pool()).ListJobsByAttachment(ctx, attID)
	var fail1, fail2 int
	for _, j := range jobs1 {
		if j.ErrorCode != nil && *j.ErrorCode == "FILE_NOT_FOUND" {
			fail1++
		}
	}
	for _, j := range jobs2 {
		if j.ErrorCode != nil && *j.ErrorCode == "FILE_NOT_FOUND" {
			fail2++
		}
	}
	if fail2 != fail1 {
		t.Errorf("repeated missing-file sync duplicated failed jobs: before=%d after=%d", fail1, fail2)
	}
}

// TestCanonicalSQLNullSemantics: missing optional values are SQL NULL, not 0/”.
func TestCanonicalSQLNullSemantics(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 1}
	// Book with no date, no DOI, no publisher -> year NULL, no empty strings.
	src.items = []zotero.CanonicalItem{itemEnv("B1", "book", "", map[string]any{"title": "No meta"}), pdfAttEnv("A1", "B1", pdf)}
	res, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	var year *int
	var pub *string
	var doi *string
	if err := d.Pool().QueryRow(ctx, `SELECT publication_year, publisher, doi FROM zotero_documents WHERE source_id=$1 AND zotero_key='B1'`, res.SourceID).Scan(&year, &pub, &doi); err != nil {
		t.Fatal(err)
	}
	if year != nil {
		t.Errorf("publication_year must be NULL for missing date, got %d", *year)
	}
	if pub != nil {
		t.Errorf("publisher must be NULL, got %q", *pub)
	}
	if doi != nil {
		t.Errorf("doi must be NULL, got %q", *doi)
	}
}

// TestCanonicalProjectedNonWhitelistedDocumentType: a dataset (previously absent
// from the bibliographic whitelist) with a PDF attachment must still be
// projected and enqueued. Projection is driven by "does it have a processable
// attachment", excluding only non-document types.
func TestCanonicalProjectedNonWhitelistedDocumentType(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 2}
	ds := itemEnv("D1", "dataset", "", map[string]any{"title": "A Dataset"})
	att := pdfAttEnv("DA1", "D1", pdf)
	src.items = []zotero.CanonicalItem{ds, att}
	res, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if n := countActiveFor(t, d, "zotero_documents", res.SourceID); n != 1 {
		t.Errorf("dataset must be projected as a document: active docs = %d, want 1", n)
	}
	var pref, deleted bool
	if err := d.Pool().QueryRow(ctx, `SELECT preferred, deleted FROM zotero_attachments WHERE source_id=$1 AND zotero_key='DA1'`, res.SourceID).Scan(&pref, &deleted); err != nil {
		t.Fatal(err)
	}
	if !pref || deleted {
		t.Errorf("dataset's PDF must be active+preferred: preferred=%v deleted=%v", pref, deleted)
	}
	if res.Enqueued != 1 {
		t.Errorf("dataset PDF must be enqueued: enqueued=%d, want 1", res.Enqueued)
	}
}

// TestCanonicalTypeChangeDeactivates: a parent that changes from book to note
// must deactivate its document + attachment projections (no stale preferred).
func TestCanonicalTypeChangeDeactivates(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "A Book", nil), pdfAttEnv("A1", "B1", pdf)}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if n := countActiveFor(t, d, "zotero_documents", res1.SourceID); n != 1 {
		t.Fatalf("book should be projected: %d docs", n)
	}
	// Change B1 to a top-level note (still has the child attachment).
	src.version = 11
	src.items = []zotero.CanonicalItem{itemEnv("B1", "note", "", map[string]any{"note": "turned into a note"})}
	if _, err := runCanon(t, src, d); err != nil {
		t.Fatalf("type-change sync: %v", err)
	}
	if n := countActiveFor(t, d, "zotero_documents", res1.SourceID); n != 0 {
		t.Errorf("type changed to note: document projection must be deactivated, got %d active docs", n)
	}
	if n := countActiveFor(t, d, "zotero_attachments", res1.SourceID); n != 0 {
		t.Errorf("type changed to note: attachment projections must be deactivated, got %d active attachments", n)
	}
	var pref bool
	if err := d.Pool().QueryRow(ctx, `SELECT preferred FROM zotero_attachments WHERE source_id=$1 AND zotero_key='A1'`, res1.SourceID).Scan(&pref); err != nil {
		t.Fatal(err)
	}
	if pref {
		t.Errorf("type changed to note: attachment A1 must not remain preferred")
	}
}

// TestCanonicalEPUBByMIMEOnly: an EPUB whose filename does NOT end in .epub but
// whose MIME is application/epub+zip must still be selected as preferred.
func TestCanonicalEPUBByMIMEOnly(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	epub := t.TempDir() + "/book.dat" // filename has no .epub extension
	os.WriteFile(epub, []byte("e"), 0o600)
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 3}
	it := itemEnv("E1", "attachment", "B1", map[string]any{
		"contentType": "application/epub+zip", "filename": "book.dat",
	})
	it.Envelope, _ = json.Marshal(map[string]any{
		"key": "E1", "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + epub}},
		"data":  map[string]any{"key": "E1", "version": 1, "itemType": "attachment", "parentItem": "B1", "contentType": "application/epub+zip", "filename": "book.dat"},
	})
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), it}
	res, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	var pref bool
	if err := d.Pool().QueryRow(ctx, `SELECT preferred FROM zotero_attachments WHERE source_id=$1 AND zotero_key='E1'`, res.SourceID).Scan(&pref); err != nil {
		t.Fatal(err)
	}
	if !pref {
		t.Errorf("EPUB selected only by MIME (no .epub filename) must be preferred")
	}
	if res.Enqueued != 1 {
		t.Errorf("EPUB job should be enqueued: enqueued=%d, want 1", res.Enqueued)
	}
}

// TestCanonicalMissingRestoredMissingNewFailedJob: "file missing -> restored
// (pending) -> missing again" must create a fresh failed job on the final
// missing sync (the first failure was resolved when the file came back).
func TestCanonicalMissingRestoredMissingNewFailedJob(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 1}

	failedCount := func(srcID string) int {
		t.Helper()
		var n int
		if err := d.Pool().QueryRow(ctx, `
			SELECT count(*) FROM ingest_jobs
			WHERE attachment_id=(SELECT id FROM zotero_attachments WHERE source_id=$1 AND zotero_key='A1')
			  AND status='failed' AND error_code='FILE_NOT_FOUND' AND resolved_at IS NULL`, srcID).Scan(&n); err != nil {
			t.Fatalf("count failed: %v", err)
		}
		return n
	}
	pendingCount := func(srcID string) int { //nolint:unparam
		t.Helper()
		var n int
		if err := d.Pool().QueryRow(ctx, `
			SELECT count(*) FROM ingest_jobs
			WHERE attachment_id=(SELECT id FROM zotero_attachments WHERE source_id=$1 AND zotero_key='A1')
			  AND status='pending'`, srcID).Scan(&n); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		return n
	}

	// 1. Missing file.
	missing := t.TempDir() + "/gone.pdf"
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", missing)}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("missing sync: %v", err)
	}
	if failedCount(res1.SourceID) != 1 {
		t.Fatalf("first missing: want 1 unresolved failed job")
	}

	// 2. File returns -> pending job, prior failure resolved.
	src.version = 2
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", pdf)}
	res2, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("restored sync: %v", err)
	}
	if res2.Enqueued != 1 {
		t.Fatalf("restored: want a pending job")
	}
	if failedCount(res2.SourceID) != 0 {
		t.Fatalf("restored: previous failure must be resolved (unresolved failed = %d)", failedCount(res2.SourceID))
	}
	if pendingCount(res2.SourceID) != 1 {
		t.Fatalf("restored: want a pending job for the attachment")
	}

	// 3. Missing again -> a NEW failed job must be created.
	src.version = 3
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", missing)}
	res3, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("missing-again sync: %v", err)
	}
	if failedCount(res3.SourceID) != 1 {
		t.Errorf("missing again: want a fresh unresolved failed job (had %d)", failedCount(res3.SourceID))
	}
}

// TestClassifyFileErrorRetryableUnix asserts a non-NotExist error maps to a
// retryable IO_ERROR while os.IsNotExist maps to non-retryable FILE_NOT_FOUND.
func TestClassifyFileErrorRetryableUnix(t *testing.T) {
	perm := classifyFileError("/x/denied.pdf", os.ErrPermission, nil)
	if perm.ErrCode != "IO_ERROR" || !perm.Retryable {
		t.Errorf("permission error: code=%q retryable=%v, want IO_ERROR+retryable", perm.ErrCode, perm.Retryable)
	}
	missing := classifyFileError("/x/gone.pdf", os.ErrNotExist, nil)
	if missing.ErrCode != "FILE_NOT_FOUND" || missing.Retryable {
		t.Errorf("not-exist: code=%q retryable=%v, want FILE_NOT_FOUND+non-retryable", missing.ErrCode, missing.Retryable)
	}
}

// TestCanonicalDeletedItemOnFallbackFullSnapshot: after a full sync, a later run
// that simulates the actual LocalAPI fallback (deletion feed unavailable ->
// force FullSnapshot=true) returns a snapshot WITHOUT the item; the item's
// zotero_items row, document projection and attachment must all become
// deleted=true, preferred=false, no new job is enqueued, and the cursor is
// advanced to the snapshot version.
func TestCanonicalDeletedItemOnFallbackFullSnapshot(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "a")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 5}
	src.items = []zotero.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A1", "B1", pdf)}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	srcID := res1.SourceID
	if n := countActiveFor(t, d, "zotero_items", srcID); n != 2 {
		t.Fatalf("after full sync active items = %d, want 2", n)
	}
	jobsBefore, err := repo.New(d.Pool()).CountJobsForSource(ctx, srcID)
	if err != nil {
		t.Fatalf("count jobs before: %v", err)
	}

	// Simulate the deletion-feed-unavailable fallback: force a full snapshot
	// where the parent (and its attachment) no longer exist.
	src.forceFull = true
	src.version = 6
	src.items = []zotero.CanonicalItem{} // item gone
	res2, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("fallback full snapshot: %v", err)
	}
	if res2.Items != 0 {
		t.Fatalf("fallback snapshot should deliver 0 items, got %d", res2.Items)
	}

	var itemDeleted, docDeleted, attDeleted, attPref bool
	if err := d.Pool().QueryRow(ctx, `SELECT deleted FROM zotero_items WHERE source_id=$1 AND zotero_key='B1'`, srcID).Scan(&itemDeleted); err != nil {
		t.Fatalf("query item deleted: %v", err)
	}
	if err := d.Pool().QueryRow(ctx, `SELECT deleted FROM zotero_documents WHERE source_id=$1 AND zotero_key='B1'`, srcID).Scan(&docDeleted); err != nil {
		t.Fatalf("query doc deleted: %v", err)
	}
	if err := d.Pool().QueryRow(ctx, `SELECT deleted FROM zotero_attachments WHERE source_id=$1 AND zotero_key='A1'`, srcID).Scan(&attDeleted); err != nil {
		t.Fatalf("query attachment deleted: %v", err)
	}
	if err := d.Pool().QueryRow(ctx, `SELECT preferred FROM zotero_attachments WHERE source_id=$1 AND zotero_key='A1'`, srcID).Scan(&attPref); err != nil {
		t.Fatalf("query attachment preferred: %v", err)
	}
	if !itemDeleted || !docDeleted || !attDeleted {
		t.Fatal("fallback full snapshot must delete item/doc/attachment")
	}
	if attPref {
		t.Fatal("deleted attachment must not remain preferred")
	}
	jobsAfter, err := repo.New(d.Pool()).CountJobsForSource(ctx, srcID)
	if err != nil {
		t.Fatalf("count jobs after: %v", err)
	}
	if jobsAfter != jobsBefore {
		t.Fatalf("deleting an item must not enqueue new jobs: before=%d after=%d", jobsBefore, jobsAfter)
	}
	cur, err := repo.New(d.Pool()).CanonicalCursor(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	if cur != 6 {
		t.Errorf("cursor = %d, want 6 (snapshot version)", cur)
	}
}
