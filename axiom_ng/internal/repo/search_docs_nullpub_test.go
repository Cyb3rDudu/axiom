// Regression #158: a NULL publisher (production reality from older writes /
// unmapped institution) must degrade THAT document's hydration, not poison the
// entire DocumentMetaByIDs batch — the bug made every hit of a query lose its
// book title whenever one NULL-publisher doc was in the candidate set.
package repo

import (
	"context"
	"testing"
)

func TestDocumentMetaByIDsNULLPublisherDoesNotPoisonBatch(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	var srcID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://zotero.nullpub', 'lib-1', 'srv-1') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	seedDoc := func(key, title string, publisher any) string {
		var id string
		if err := lr.pool.QueryRow(ctx, `
			INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title,
				creators, publication_year, publication_date, publisher, deleted)
			VALUES ($1, $2, 7, 'book', $3, '[]', 2024, '2024', $4, false)
			RETURNING id::text`, srcID, key, title, publisher).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	nullPub := seedDoc("NULLPUB1", "Book Without Publisher", nil) // the poison seed
	okPub := seedDoc("OKPUB1", "Book With Publisher", "Springer")

	meta, err := lr.rep.DocumentMetaByIDs(ctx, []string{nullPub, okPub})
	if err != nil {
		t.Fatalf("hydration must survive a NULL-publisher doc in the batch: %v", err)
	}
	if len(meta) != 2 {
		t.Fatalf("expected 2 hydrated docs, got %d: %v", len(meta), meta)
	}
	if meta[nullPub].Title != "Book Without Publisher" {
		t.Errorf("NULL-publisher doc: title = %q", meta[nullPub].Title)
	}
	if meta[nullPub].Publisher != "" {
		t.Errorf("NULL publisher must hydrate as empty string, got %q", meta[nullPub].Publisher)
	}
	if meta[okPub].Publisher != "Springer" {
		t.Errorf("healthy doc: publisher = %q", meta[okPub].Publisher)
	}
}
