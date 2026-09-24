// revision_mitschrieb_it_test.go — the F06 (#300) source-revision
// Mits-Schrieb witness at the SYNC completion point: RecordSyncRevisions
// derives revisions from the Zotero mirror, idempotently (unchanged
// content republishes nothing, changed content bumps the revision).
// The heal-point twin (RecordAttachmentRevision) shares the store code;
// its HTTP-side flow is proven by the custody ITs' unchanged ordering.
package sync

import (
	"context"
	"fmt"
	"testing"

	axlibrary "github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
)

func TestRevisionMitschriebSyncCompletion(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	if err := axlibrary.Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("library migrate: %v", err)
	}
	st := axlibrary.NewStore(d.Pool())

	src := fmt.Sprintf("rev-mit-%d", timeNowNanos())
	var sourceID string
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_sources (base_url, library_id, server_id) VALUES ($1,'users/0','srv') RETURNING id::text`,
		"https://zotero.test/"+src).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	var docKey = "DOC" + src
	var docID string
	if err := d.Pool().QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title, creators, doi)
		VALUES ($1,$2,1,'book','Mitschrieb Werk','[{"firstName":"A","lastName":"Autor","creatorType":"author"}]','10.1/mit')
		RETURNING id::text`, sourceID, docKey).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	seedAtt := func(key, hash string) {
		if _, err := d.Pool().Exec(ctx, `
			INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version, parent_zotero_key,
				link_mode, content_type, filename, content_hash, local_path)
			VALUES ($1,$2,$3,1,$4,'imported_file','application/pdf','x.pdf',$5,'/tmp/x.pdf')`,
			sourceID, docID, key, docKey, hash); err != nil {
			t.Fatal(err)
		}
	}
	seedAtt("ATT"+src, "aa"+fmt.Sprintf("%062d", 1))

	n, err := st.RecordSyncRevisions(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("published %d revisions, want 1", n)
	}
	rev1 := latestRevision(t, st, sourceID, docKey, "ATT"+src)
	if rev1.Bibliography.Title != "Mitschrieb Werk" || rev1.ContentTicket != "zat:"+sourceID+":ATT"+src {
		t.Fatalf("revision derived wrong: %+v", rev1)
	}

	// Idempotent Mits-Schrieb: unchanged content republishes NOTHING.
	n, err = st.RecordSyncRevisions(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-sync published %d, want 1 (no new revision rows)", n)
	}
	if rev2 := latestRevision(t, st, sourceID, docKey, "ATT"+src); rev2.RevisionID != rev1.RevisionID {
		t.Fatalf("revision bumped without a content change: %d → %d", rev1.RevisionID, rev2.RevisionID)
	}

	// Heal point: a changed content hash mints the NEXT revision.
	if err := st.RecordAttachmentRevision(ctx, sourceID, docKey, "ATT"+src,
		"bb"+fmt.Sprintf("%062d", 2), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	rev3 := latestRevision(t, st, sourceID, docKey, "ATT"+src)
	if rev3.RevisionID != rev1.RevisionID+1 || rev3.Origin != "heal" {
		t.Fatalf("heal did not bump the revision: %+v", rev3)
	}
}

func latestRevision(t *testing.T, st *axlibrary.Store, sourceID, docKey, attKey string) axlibrary.SourceRevisionDomain {
	t.Helper()
	rev, err := st.LatestRevision(context.Background(), sourceID, docKey, attKey)
	if err != nil {
		t.Fatalf("latest revision: %v", err)
	}
	return rev
}
