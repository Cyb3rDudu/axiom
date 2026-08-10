package repo

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func cnt() *int64 {
	return &_cnt
}

var _cnt int64

func uniq(kind string) string {
	_cnt++
	return fmt.Sprintf("%s%x-%d", kind, _cnt, time.Now().UnixNano())
}
func newUUID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	// A unique base_url per run keeps each test isolated.
	key := uniq("S")
	if err := pool.QueryRow(ctx,
		`INSERT INTO zotero_sources (base_url, library_id, server_id)
		 VALUES ($1,'users/0','test') RETURNING id::text`, "http://"+key).Scan(&id); err != nil {
		t.Fatalf("new source: %v", err)
	}
	return id
}

func newDoc(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sourceID string) string {
	t.Helper()
	var id string
	key := uniq("D")
	if err := pool.QueryRow(ctx,
		`INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		 VALUES ($1, $2, 1, 'book', $2) RETURNING id::text`, sourceID, key).Scan(&id); err != nil {
		t.Fatalf("new doc: %v", err)
	}
	return id
}

func newAtt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sourceID, docID string) string {
	return newAttForDoc(t, ctx, pool, sourceID, docID, "parent")
}

func newAttForDoc(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sourceID, docID, parentKey string) string {
	t.Helper()
	var id string
	key := uniq("A")
	if err := pool.QueryRow(ctx,
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode, content_type, filename)
		 VALUES ($1, $2, $3, 1, $4, 'imported_file', 'application/pdf', 'a.pdf') RETURNING id::text`,
		sourceID, docID, key, parentKey).Scan(&id); err != nil {
		t.Fatalf("new att: %v", err)
	}
	return id
}

func testRepo(t *testing.T, ctx context.Context) (*Repo, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("AXIOMNG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(d.Pool()), d.Pool()
}

// TestListJobsReadsFailedJobWithNullContentHash verifies that a failed job whose
// content_hash is NULL (created via EnqueueFailed) can be read back by ListJobs
// without a scan error, so GET /api/ingest/jobs returns it instead of a 500.
func TestListJobsReadsFailedJobWithNullContentHash(t *testing.T) {
	ctx := context.Background()
	r, pool := testRepo(t, ctx)

	sourceID := newUUID(t, ctx, pool)
	docID := newDoc(t, ctx, pool, sourceID)
	attID := newAtt(t, ctx, pool, sourceID, docID)

	if err := r.EnqueueFailed(ctx, FailedJob{
		SourceID: sourceID, DocumentID: docID, AttachmentID: attID,
		ErrorCode: "FILE_NOT_FOUND", ErrorMessage: "no such file", Retryable: false,
	}); err != nil {
		t.Fatalf("EnqueueFailed: %v", err)
	}

	jobs, err := r.ListJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ListJobs must not fail on NULL content_hash: %v", err)
	}
	found := false
	for _, j := range jobs {
		if j.AttachmentID == attID {
			found = true
			if j.ContentHash != nil {
				t.Errorf("failed job content_hash = %v, want nil", *j.ContentHash)
			}
			if j.Status != "failed" || j.ErrorCode == nil || *j.ErrorCode != "FILE_NOT_FOUND" {
				t.Errorf("unexpected failed job: status=%s code=%v", j.Status, j.ErrorCode)
			}
		}
	}
	if !found {
		t.Fatal("failed job not returned by ListJobs")
	}
}

// TestSetSourceVersionMonotonic verifies SetSourceVersion never moves the cursor
// backwards even when called with an older version after a newer one.
func TestSetSourceVersionMonotonic(t *testing.T) {
	ctx := context.Background()
	r, pool := testRepo(t, ctx)
	sourceID := newUUID(t, ctx, pool)

	if err := r.SetSourceVersion(ctx, sourceID, 181); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSourceVersion(ctx, sourceID, 180); err != nil {
		t.Fatal(err)
	}
	v, err := r.SourceVersion(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if v != 181 {
		t.Errorf("version = %d, want 181 (must not regress under concurrent syncs)", v)
	}
}

// TestReconcileMarksRemovedAndUnpreferred verifies that after a sync removed an
// attachment (not in seenKeys) and switched the preferred file, the old
// attachment is marked deleted and no longer preferred.
func TestReconcileMarksRemovedAndUnpreferred(t *testing.T) {
	ctx := context.Background()
	r, pool := testRepo(t, ctx)
	sourceID := newUUID(t, ctx, pool)
	docID := newDoc(t, ctx, pool, sourceID)
	var docKey string
	if err := pool.QueryRow(ctx,
		`SELECT zotero_key FROM zotero_documents WHERE id = $1`, docID).Scan(&docKey); err != nil {
		t.Fatal(err)
	}
	attPdfID := newAttForDoc(t, ctx, pool, sourceID, docID, docKey)

	var pdfKey string
	if err := pool.QueryRow(ctx,
		`SELECT zotero_key FROM zotero_attachments WHERE id = $1`, attPdfID).Scan(&pdfKey); err != nil {
		t.Fatal(err)
	}

	// Add a second attachment, the EPUB, that will become the preferred file.
	var attEpubID, epubKey string
	if err := pool.QueryRow(ctx,
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode, content_type, filename)
		 VALUES ($1,$2,$3,1,$4,'imported_file','application/epub+zip','a.epub') RETURNING id::text`,
		sourceID, docID, uniq("E"), docKey).Scan(&attEpubID); err != nil {
		t.Fatalf("new epub: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT zotero_key FROM zotero_attachments WHERE id = $1`, attEpubID).Scan(&epubKey); err != nil {
		t.Fatal(err)
	}

	// Simulate: PDF was previously the preferred, now only the EPUB is seen.
	if err := r.UpdateAttachmentFileInfo(ctx, sourceID, pdfKey, "hashA", 10, 1, true); err != nil {
		t.Fatal(err)
	}

	// The PDF is no longer present and not preferred anymore.
	if err := r.Reconcile(ctx, ReconcileReq{
		SourceID:             sourceID,
		AffectedDocKeys:      []string{docKey},
		SeenAttachments:      map[string][]string{docKey: {epubKey}},
		PreferredAttachments: map[string]string{docKey: epubKey},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var pdfDeleted, epubDeleted bool
	var pdfPreferred, epubPreferred bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted, preferred FROM zotero_attachments WHERE id = $1`, attPdfID).Scan(&pdfDeleted, &pdfPreferred); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT deleted, preferred FROM zotero_attachments WHERE id = $1`, attEpubID).Scan(&epubDeleted, &epubPreferred); err != nil {
		t.Fatal(err)
	}
	if !pdfDeleted {
		t.Errorf("removed PDF should be marked deleted")
	}
	if pdfPreferred {
		t.Errorf("removed PDF should no longer be preferred")
	}
	if epubDeleted {
		t.Errorf("EPUB should stay not-deleted")
	}
}

// TestReconcileSingleDeletedAttachment: deleting a single attachment (via a
// deleted key that matches an attachment, not a document) leaves the parent
// document and sibling attachments intact.
func TestReconcileSingleDeletedAttachment(t *testing.T) {
	ctx := context.Background()
	r, pool := testRepo(t, ctx)
	sourceID := newUUID(t, ctx, pool)
	docID := newDoc(t, ctx, pool, sourceID)
	var docKey string
	if err := pool.QueryRow(ctx, `SELECT zotero_key FROM zotero_documents WHERE id=$1`, docID).Scan(&docKey); err != nil {
		t.Fatal(err)
	}
	att1 := newAttForDoc(t, ctx, pool, sourceID, docID, docKey)
	var att1Key string
	if err := pool.QueryRow(ctx, `SELECT zotero_key FROM zotero_attachments WHERE id=$1`, att1).Scan(&att1Key); err != nil {
		t.Fatal(err)
	}
	// A sibling attachment that must survive.
	var att2Key string
	if err := pool.QueryRow(ctx,
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode, content_type, filename)
		 VALUES ($1,$2,$3,1,$4,'imported_file','application/pdf','b.pdf') RETURNING zotero_key`,
		sourceID, docID, "sibling"+docKey, docKey).Scan(&att2Key); err != nil {
		t.Fatal(err)
	}

	// Delete only the single attachment by its key.
	if err := r.Reconcile(ctx, ReconcileReq{
		SourceID:       sourceID,
		DeletedDocKeys: []string{att1Key},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	flags := attFlagsByKey(t, r, ctx, sourceID, docKey)
	if a := flags[att1Key]; !a[0] {
		t.Errorf("deleted attachment %s must be marked deleted", att1Key)
	}
	if a := flags[att2Key]; a[0] {
		t.Errorf("sibling attachment %s must survive single-attachment deletion", att2Key)
	}
	var docFlag bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted FROM zotero_documents WHERE source_id=$1 AND zotero_key=$2`,
		sourceID, docKey).Scan(&docFlag); err != nil {
		t.Fatal(err)
	}
	if docFlag {
		t.Errorf("parent document must NOT be deleted when only an attachment was removed")
	}
}

func attFlagsByKey(t *testing.T, r *Repo, ctx context.Context, sourceID, docKey string) map[string][2]bool {
	t.Helper()
	rows, err := r.pool.Query(ctx, `
		SELECT zotero_key, deleted, preferred FROM zotero_attachments
		WHERE source_id=$1 AND parent_zotero_key=$2`, sourceID, docKey)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	out := map[string][2]bool{}
	for rows.Next() {
		var k string
		var del, pref bool
		rows.Scan(&k, &del, &pref)
		out[k] = [2]bool{del, pref}
	}
	return out
}
