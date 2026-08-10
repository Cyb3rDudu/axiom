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
