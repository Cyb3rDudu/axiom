// #196/#245: SourceView.content_type — the ACTIVE snapshot's attachment
// format is the one format factor of the citation contract (PDF → page by
// trust, EPUB → always APA 7 sections). Twin attachments must report the
// format of the ACTIVE snapshot, not any attachment.
package repo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDocumentMetaContentTypeActiveSnapshot(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	var srcID, docID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://zotero.ct', 'lib-1', 'srv-1') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, 'CTDOC', 1, 'book', 'Twin Book') RETURNING id::text`, srcID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	seedAtt := func(key, ctype string) string {
		var id string
		if err := lr.pool.QueryRow(ctx, `
			INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
				parent_zotero_key, link_mode, content_type, filename)
			VALUES ($1, $2, $3, 1, 'CTDOC', 'imported_file', $4, $3) RETURNING id::text`,
			srcID, docID, key, ctype).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	seedSnap := func(attID string, active bool) {
		if _, err := lr.pool.Exec(ctx, `
			INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
				processor_version, profile_hash, document_id, profile, active)
			VALUES ($1::uuid, 'sha256:'||$1::text, 'p', 'v', 'ph', $2::uuid, '{}', $3)`,
			attID, docID, active); err != nil {
			t.Fatal(err)
		}
	}

	pdfAtt := seedAtt("CTPDF", "application/pdf")
	epubAtt := seedAtt("CTEPUB", "application/epub+zip")
	seedSnap(pdfAtt, false) // the retired twin
	seedSnap(epubAtt, true) // the active one — EPUB wins

	meta, err := lr.rep.DocumentMetaByIDs(ctx, []string{docID})
	if err != nil {
		t.Fatal(err)
	}
	if got := meta[docID].ContentType; got != "application/epub+zip" {
		t.Fatalf("content_type = %q, want the ACTIVE snapshot's format (epub): %+v", got, meta[docID])
	}
	if v := meta[docID].View(docID); v.ContentType != "application/epub+zip" {
		t.Fatalf("SourceView.content_type = %q, want epub: %+v", v.ContentType, v)
	}

	// a document with NO snapshot: content_type degrades to "" (never error)
	var bareID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, 'CTBARE', 1, 'book', 'Bare') RETURNING id::text`, srcID).Scan(&bareID); err != nil {
		t.Fatal(err)
	}
	meta2, err := lr.rep.DocumentMetaByIDs(ctx, []string{bareID})
	if err != nil {
		t.Fatal(err)
	}
	if got := meta2[bareID].ContentType; got != "" {
		t.Fatalf("no snapshot: content_type = %q, want empty", got)
	}
	// #245 correction: the format factor must ALWAYS be on the wire — an
	// unknown format serializes as content_type:"" (present key), never as
	// an absent key (omitempty would make it indistinguishable from an
	// old server).
	wire, err := json.Marshal(meta2[bareID].View(bareID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"content_type":""`) {
		t.Fatalf("content_type key must survive an empty value: %s", wire)
	}
}
