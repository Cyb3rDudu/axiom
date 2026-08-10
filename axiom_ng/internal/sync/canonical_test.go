package sync

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

// canonicalFake implements zotero.Source + the canonicalFetcher capability for
// RunCanonical, returning a fixed set of items and collections.
type canonicalFake struct {
	serverID    string
	baseURL     string
	items       []zotero.CanonicalItem
	collections []zotero.CanonicalCollection
	version     int64
}

func (c *canonicalFake) ServerID() string { return c.serverID }
func (c *canonicalFake) ListCollections() ([]zotero.Collection, error) {
	return nil, nil
}
func (c *canonicalFake) ListPDFItems(since int64) (zotero.ListResult, error) {
	return zotero.ListResult{}, nil
}
func (c *canonicalFake) ListDeletedKeys(since int64) ([]zotero.DeleteEvent, int64, error) {
	return nil, c.version, nil
}
func (c *canonicalFake) ResolveAttachmentPath(key string) (string, error) { return "", nil }
func (c *canonicalFake) FetchParent(key string) (*zotero.Item, error)     { return nil, nil }
func (c *canonicalFake) ListCanonicalItems(since int64) ([]zotero.CanonicalItem, int64, error) {
	return c.items, c.version, nil
}
func (c *canonicalFake) ListCanonicalCollections() ([]zotero.CanonicalCollection, error) {
	return c.collections, nil
}

func mkItemJSON(key, itemType, parent, title string, extra map[string]any) zotero.CanonicalItem {
	data := map[string]any{"key": key, "version": 1, "itemType": itemType}
	if title != "" {
		data["title"] = title
	}
	if parent != "" {
		data["parentItem"] = parent
	}
	for k, v := range extra {
		data[k] = v
	}
	env := map[string]any{"key": key, "version": 1, "data": data}
	envB, _ := jsonMarshal(env)
	dataB, _ := jsonMarshal(data)
	it := zotero.CanonicalItem{Key: key, Version: 1, ItemType: itemType, ParentKey: parent, Envelope: envB, Data: dataB}
	return it
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// TestRunCanonicalLosslessAndNoAnnotateEnqueue verifies RunCanonical stores all
// item types losslessly (incl. note/annotation), derives the document/attachment
// projection for the bibliographic parent's preferred attachment, and enqueues
// a job only for it (never for notes/annotations).
func TestRunCanonicalLosslessAndNoAnnotateEnqueue(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOMNG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pdfPath := t.TempDir() + "/a.pdf"
	os.WriteFile(pdfPath, []byte("a"), 0o600)

	src := &canonicalFake{
		serverID: "canon",
		baseURL:  newScriptedBase(),
		version:  7,
	}
	src.items = []zotero.CanonicalItem{
		mkItemJSON("B1", "book", "", "A Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
			"date":     "2020",
			"DOI":      "10.1000/xyz",
		}),
		mkItemJSON("A1", "attachment", "B1", "a.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "a.pdf",
		}),
		mkItemJSON("N1", "note", "B1", "A Note", map[string]any{"note": "Some annotation text"}),
	}
	// The attachment's local path is in the envelope links.enclosure.
	envForA1, _ := json.Marshal(map[string]any{
		"key": "A1", "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "A1", "version": 1, "itemType": "attachment", "parentItem": "B1", "contentType": "application/pdf", "filename": "a.pdf"},
	})
	src.items[1].Envelope = envForA1
	src.collections = []zotero.CanonicalCollection{
		{Key: "C1", Name: "Top", ParentKey: "", Envelope: json.RawMessage(`{"key":"C1","data":{"key":"C1","name":"Top","parentCollection":false}}`)},
	}

	repoObj := repo.New(d.Pool())
	svc := New(src, repoObj, src.baseURL, "users/0", log.Default())
	res, err := svc.RunCanonical(ctx)
	if err != nil {
		t.Fatalf("RunCanonical: %v", err)
	}
	if res.Items != 3 {
		t.Errorf("expected 3 canonical items (book+attachment+note), got %d", res.Items)
	}
	if res.Collections != 1 {
		t.Errorf("expected 1 collection, got %d", res.Collections)
	}
	if res.Documents != 1 {
		t.Errorf("expected 1 document projection (only the book), got %d", res.Documents)
	}
	if res.Enqueued != 1 {
		t.Errorf("expected exactly 1 enqueued job (the PDF), got %d", res.Enqueued)
	}

	// Note/annotation must exist canonically but have no job.
	var noteCount int
	if err := d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM zotero_items WHERE item_type='note' AND source_id=$1`, res.SourceID).Scan(&noteCount); err != nil {
		t.Fatal(err)
	}
	if noteCount != 1 {
		t.Errorf("expected 1 canonical note row, got %d", noteCount)
	}

	// Roundtrip: the canonical item's envelope/data survive and fields intact.
	var rawData string
	if err := d.Pool().QueryRow(ctx,
		`SELECT raw_data::text FROM zotero_items WHERE zotero_key='B1' AND source_id=$1`, res.SourceID).Scan(&rawData); err != nil {
		t.Fatal(err)
	}
	if !containsStr(rawData, "10.1000/xyz") {
		t.Errorf("canonical raw_data lost DOI field: %s", rawData)
	}
	// The note must exist and its text preserved.
	var noteData string
	if err := d.Pool().QueryRow(ctx,
		`SELECT raw_data::text FROM zotero_items WHERE zotero_key='N1' AND source_id=$1`, res.SourceID).Scan(&noteData); err != nil {
		t.Fatal(err)
	}
	if !containsStr(noteData, "Some annotation text") {
		t.Errorf("canonical note raw_data lost text: %s", noteData)
	}
}
