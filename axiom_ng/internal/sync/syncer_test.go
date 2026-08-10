package sync

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// fakeSource is a minimal zotero.Source returning one fixed document.
type fakeSource struct {
	serverID string
	item     zotero.Item
}

func (f *fakeSource) ServerID() string { return f.serverID }

func (f *fakeSource) ListCollections() ([]zotero.Collection, error) { return nil, nil }

func (f *fakeSource) ListPDFItems(since int64) (zotero.ListResult, error) {
	return zotero.ListResult{
		Items:        []zotero.Item{f.item},
		AffectedKeys: []string{f.item.Key},
		NewVersion:   42,
	}, nil
}

func (f *fakeSource) ListDeletedKeys(since int64) ([]zotero.DeleteEvent, int64, error) {
	return nil, 42, nil
}
func (f *fakeSource) FetchParent(parentKey string) (*zotero.Item, error) { return nil, nil }

func (f *fakeSource) ResolveAttachmentPath(key string) (string, error) {
	return f.item.Attachments[0].LocalPath, nil
}

func TestSyncEndToEndAndIdempotent(t *testing.T) {
	dsn := os.Getenv("AXIOMNG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A real local file so ContentHash has something to hash. A unique key and
	// file content per run keeps this integration test isolated from earlier
	// runs against the same database.
	dir := t.TempDir()
	pdf := filepath.Join(dir, "doc.pdf")
	key := fmt.Sprintf("BK%d", time.Now().UnixNano())
	attKey := "A" + key
	if err := os.WriteFile(pdf, []byte("pdf content "+key), 0o600); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{
		serverID: "test-server",
		item: zotero.Item{
			Key: key, Version: 1, ItemType: "book", Title: "A Book",
			Creators:    []zotero.Creator{{FirstName: "Ada", LastName: "Lovelace"}},
			Attachments: []zotero.Attachment{{Key: attKey, ParentKey: key, ContentType: "application/pdf", Filename: "doc.pdf", LocalPath: pdf}},
		},
	}

	r := repo.New(d.Pool())
	svc := New(src, r, "http://test/api", "users/0", log.Default())

	res1, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if res1.Enqueued != 1 {
		t.Fatalf("first sync enqueued %d, want 1", res1.Enqueued)
	}
	if res1.NewVersion != 42 || res1.ServerID != "test-server" {
		t.Errorf("unexpected result: %+v", res1)
	}

	// A second sync over the same unchanged source must not enqueue again.
	res2, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res2.Enqueued != 0 {
		t.Errorf("second sync enqueued %d, want 0 (idempotent)", res2.Enqueued)
	}

	// And the mirror rows must exist.
	docID, err := r.DocumentID(ctx, res1.SourceID, key)
	if err != nil {
		t.Fatalf("document lookup: %v", err)
	}
	if docID == "" {
		t.Fatal("document not mirrored")
	}
	attID, err := r.AttachmentID(ctx, res1.SourceID, attKey)
	if err != nil {
		t.Fatalf("attachment lookup: %v", err)
	}
	if attID == "" {
		t.Fatal("attachment not mirrored")
	}
}

// missingSource is a fake zotero.Source whose preferred attachment has no
// local file on disk, so hashing fails and a failed ingest job must be recorded.
type missingSource struct {
	serverID string
	item     zotero.Item
}

func (f *missingSource) ServerID() string { return f.serverID }

func (f *missingSource) ListCollections() ([]zotero.Collection, error) { return nil, nil }

func (f *missingSource) ListPDFItems(since int64) (zotero.ListResult, error) {
	return zotero.ListResult{
		Items:        []zotero.Item{f.item},
		AffectedKeys: []string{f.item.Key},
		NewVersion:   42,
	}, nil
}

func (f *missingSource) ListDeletedKeys(since int64) ([]zotero.DeleteEvent, int64, error) {
	return nil, 42, nil
}
func (f *missingSource) FetchParent(parentKey string) (*zotero.Item, error) { return nil, nil }

func (f *missingSource) ResolveAttachmentPath(key string) (string, error) {
	return "", nil
}

// TestSyncRecordsMissingFileAsFailedJob verifies that a preferred attachment
// whose local file is missing is recorded as a failed ingest job (not silently
// dropped) and that the source cursor still advances — because the failure was
// persisted. If recording the failure were to error, Run would abort instead.
func TestSyncRecordsMissingFileAsFailedJob(t *testing.T) {
	dsn := os.Getenv("AXIOMNG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	missingPath := filepath.Join(t.TempDir(), "missing.pdf") // does not exist
	src := &missingSource{
		serverID: "test-server",
		item: zotero.Item{
			Key: "M1", Version: 1, ItemType: "book", Title: "Missing",
			Attachments: []zotero.Attachment{{Key: "M-A1", Version: 1, ParentKey: "M1", ContentType: "application/pdf", Filename: "missing.pdf", LocalPath: missingPath}},
		},
	}

	r := repo.New(d.Pool())
	svc := New(src, r, "http://test/api", "users/0", log.Default())

	res, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("sync with missing file should not fail: %v", err)
	}
	if res.Enqueued != 0 {
		t.Errorf("Enqueued = %d, want 0 (missing file yields no job)", res.Enqueued)
	}
	// The failure must be persisted as a failed job.
	jobs, err := r.ListJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	found := false
	for _, j := range jobs {
		if j.ErrorCode != nil && *j.ErrorCode == "FILE_NOT_FOUND" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a failed ingest job with FILE_NOT_FOUND")
	}
	// Cursor must have advanced (the failure was recorded, sync completed).
	if res.NewVersion != 42 {
		t.Errorf("NewVersion = %d, want 42 (cursor advanced after recorded failure)", res.NewVersion)
	}
}
