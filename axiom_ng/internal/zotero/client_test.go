package zotero

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestLocalAPIServerID(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.Header().Set("Zotero-Server-ID", "test-server-1")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	if got := api.ServerID(); got != "test-server-1" {
		t.Fatalf("ServerID = %q, want test-server-1", got)
	}
	if !called.Load() {
		t.Fatal("expected a request to the API root")
	}
}

// itemObject builds a Zotero local-API item envelope with a data object and an
// optional file enclosure link.
func itemObject(key, itemType, parent, title string, attrs map[string]any) map[string]any {
	return map[string]any{
		"key": key, "version": 1,
		"data": func() map[string]any {
			d := map[string]any{"key": key, "version": 1, "itemType": itemType, "title": title}
			if parent != "" {
				d["parentItem"] = parent
			}
			for k, v := range attrs {
				d[k] = v
			}
			return d
		}(),
	}
}

func TestListPDFItemsGroupsAttachments(t *testing.T) {
	items := []map[string]any{
		itemObject("BOOK1", "book", "", "A Book", map[string]any{
			"creators":    []map[string]string{{"firstName": "Ada", "lastName": "Lovelace"}},
			"tags":        []map[string]string{{"tag": "science"}},
			"collections": []string{"COLL1"},
		}),
		map[string]any{
			"key": "ATT-PDF1", "version": 1,
			"links": map[string]any{"enclosure": map[string]any{"href": "file:///X/storage/ATT-PDF1/book.pdf"}},
			"data": map[string]any{
				"key": "ATT-PDF1", "version": 1, "itemType": "attachment",
				"parentItem": "BOOK1", "contentType": "application/pdf",
				"linkMode": "imported_file", "filename": "book.pdf",
			},
		},
		map[string]any{
			"key": "ATT-EPUB1", "version": 1,
			"data": map[string]any{
				"key": "ATT-EPUB1", "version": 1, "itemType": "attachment",
				"parentItem": "BOOK1", "contentType": "application/epub+zip",
				"linkMode": "imported_file", "filename": "book.epub",
			},
		},
		map[string]any{
			"key": "ATT-NOFILE", "version": 1,
			"data": map[string]any{
				"key": "ATT-NOFILE", "version": 1, "itemType": "attachment",
				"parentItem": "BOOK1", "contentType": "text/html", "filename": "notes.html",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[`)
		for i, it := range items {
			if i > 0 {
				fmt.Fprint(w, `,`)
			}
			b, _ := json.Marshal(it)
			w.Write(b)
		}
		fmt.Fprint(w, `]`)
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	res, err := api.ListPDFItems(0)
	if err != nil {
		t.Fatalf("ListPDFItems: %v", err)
	}
	got := res.Items
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1 (BOOK1 has attachments)", len(got))
	}
	book := got[0]
	if book.Key != "BOOK1" || book.Title != "A Book" {
		t.Errorf("unexpected item: %+v", book)
	}
	// The client now returns all attachments (incl. non-PDF/EPUB) so the sync
	// layer can reconcile removed files; preferred selection happens later via
	// PreferredAttachment.
	if len(book.Attachments) != 3 {
		t.Errorf("expected 3 attachments (pdf+epub+html), got %d", len(book.Attachments))
	}
	if book.Attachments[0].Key != "ATT-PDF1" || book.Attachments[0].LocalPath != "file:///X/storage/ATT-PDF1/book.pdf" {
		t.Errorf("pdf attachment not resolved: %+v", book.Attachments[0])
	}
	if len(book.Creators) != 1 || book.Creators[0].LastName != "Lovelace" {
		t.Errorf("creators not mapped: %+v", book.Creators)
	}
	if len(book.Tags) != 1 || book.Tags[0].Tag != "science" {
		t.Errorf("tags not mapped: %+v", book.Tags)
	}
}

func TestListCollections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"key":"C1","data":{"key":"C1","name":"MBA","parentCollection":false}},
			{"key":"C2","data":{"key":"C2","name":"Child","parentCollection":"C1"}}
		]`)
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	cols, err := api.ListCollections()
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(cols) != 2 {
		t.Fatalf("got %d collections, want 2", len(cols))
	}
	if cols[0].Name != "MBA" || cols[1].Parent != "C1" {
		t.Errorf("unexpected collections: %+v", cols)
	}
}

func TestResolveAttachmentPath(t *testing.T) {
	fileURI := "file:///Users/dudu/Zotero/storage/ATT-PDF1/book.pdf"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"key":"ATT-PDF1","links":{"enclosure":{"href":%q}},"data":{}}`, fileURI)
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	got, err := api.ResolveAttachmentPath("ATT-PDF1")
	if err != nil {
		t.Fatalf("ResolveAttachmentPath: %v", err)
	}
	if got != fileURI {
		t.Errorf("got %q, want %q", got, fileURI)
	}
}

// TestListDeletedKeysMergesTrashAndPermanent verifies ListDeletedKeys merges the
// /items/trash feed (currently-trashed, rich details) with the permanent
// /deleted feed (keys), deduplicating by key. This covers the case where an
// item is trashed and purged between two syncs: it appears only in /deleted,
// not in the live trash.
func TestListDeletedKeysMergesTrashAndPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "42")
		switch r.URL.Path {
		case "/users/0/items/trash":
			// Today's live trash: only T1 (T2 was already purged).
			json.NewEncoder(w).Encode([]map[string]any{
				{"key": "T1", "data": map[string]any{"key": "T1", "itemType": "book"}},
			})
		case "/users/0/deleted":
			// Permanent deletions since last sync: T2 (gone from trash) + D1.
			json.NewEncoder(w).Encode(map[string]any{
				"collections": []string{"C9"},
				"items":       []string{"T2", "D1"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	events, version, err := api.ListDeletedKeys(40)
	if err != nil {
		t.Fatalf("ListDeletedKeys: %v", err)
	}
	if version != 42 {
		t.Errorf("version = %d, want 42", version)
	}
	got := map[string]bool{}
	for _, e := range events {
		got[e.Key] = true
	}
	// T1: live trash, T2: permanent /deleted, D1: permanent /deleted.
	for _, k := range []string{"T1", "T2", "D1"} {
		if !got[k] {
			t.Errorf("missing deleted event for %s", k)
		}
	}
	if len(events) != 3 {
		t.Errorf("expected exactly 3 deduplicated events, got %d", len(events))
	}
	// The purge-without-trash-sighting case is the key regression: T2 must be
	// deleted even though it is absent from both the delta and the live trash.
	if !got["T2"] {
		t.Error("permanently-deleted key T2 must produce a delete event")
	}
}

// fullCanonicalServer wires an httptest server with configurable Last-Modified-
// Version per path and an optional delete-feed status, returning the API.
func fullCanonicalServer(t *testing.T, itemVersion, trashVersion, deletedVersion int64, deletedStatus int) *LocalAPI {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/items":
			w.Header().Set("Last-Modified-Version", fmt.Sprintf("%d", itemVersion))
			json.NewEncoder(w).Encode([]map[string]any{itemObject("B1", "book", "", "A", nil)})
		case "/users/0/items/trash":
			w.Header().Set("Last-Modified-Version", fmt.Sprintf("%d", trashVersion))
			json.NewEncoder(w).Encode([]map[string]any{})
		case "/users/0/deleted":
			w.Header().Set("Last-Modified-Version", fmt.Sprintf("%d", deletedVersion))
			if deletedStatus != 0 {
				w.WriteHeader(deletedStatus)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []string{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
}

// TestCanonicalFallbackOnMissingDeletedFeed: when /deleted returns 501, an
// incremental canonical sync must fall back to a FULL snapshot so deletions are
// reconciled by item absence, and never report an incremental (unknown-deletion)
// batch.
func TestCanonicalFallbackOnMissingDeletedFeed(t *testing.T) {
	api := fullCanonicalServer(t, 41, 41, 42, http.StatusNotImplemented)
	b, err := api.ListCanonicalItems(5)
	if err != nil {
		t.Fatalf("ListCanonicalItems: %v", err)
	}
	if !b.FullSnapshot {
		t.Error("missing /deleted feed must force FullSnapshot=true (reconcile-by-absence), got incremental")
	}
	if len(b.Items) < 1 {
		t.Error("full-snapshot fallback must still deliver the items")
	}
	if b.NewVersion < 41 {
		t.Errorf("NewVersion = %d, want >= 41 (item feed version)", b.NewVersion)
	}
}

// TestCanonicalCursorIsMaxOfFeeds: the new cursor must be max(since,
// itemsVersion, deletedVersion), not just the item feed version.
func TestCanonicalCursorIsMaxOfFeeds(t *testing.T) {
	api := fullCanonicalServer(t, 41, 41, 42, 0)
	b, err := api.ListCanonicalItems(5)
	if err != nil {
		t.Fatalf("ListCanonicalItems: %v", err)
	}
	if b.NewVersion != 42 {
		t.Errorf("NewVersion must be max(items=41, deleted=42) = 42, got %d", b.NewVersion)
	}
	if b.FullSnapshot {
		t.Error("deleted feed usable: expected an incremental batch, not a full snapshot")
	}
}
