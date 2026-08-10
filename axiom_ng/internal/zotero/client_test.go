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
		t.Fatal("expected a request to the root library endpoint")
	}
}

func TestListPDFItemsGroupsAttachments(t *testing.T) {
	items := []map[string]any{
		{"key": "BOOK1", "version": 3, "itemType": "book", "title": "A Book",
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace"}},
			"tags": []map[string]string{{"tag": "science"}},
			"collections": []string{"COLL1"}},
		{"key": "ATT-PDF1", "version": 2, "itemType": "attachment",
			"parentItem": "BOOK1", "contentType": "application/pdf",
			"linkMode": "imported_file", "filename": "book.pdf"},
		{"key": "ATT-EPUB1", "version": 2, "itemType": "attachment",
			"parentItem": "BOOK1", "contentType": "application/epub+zip",
			"linkMode": "imported_file", "filename": "book.epub"},
		{"key": "ARTICLE2", "version": 1, "itemType": "journalArticle", "title": "An Article"},
		{"key": "ATT-NOFILE", "version": 1, "itemType": "attachment",
			"parentItem": "BOOK1", "contentType": "text/html", "filename": "notes.html"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json, got %q", r.URL.Query().Get("format"))
		}
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
	got, version, err := api.ListPDFItems(0)
	if err != nil {
		t.Fatalf("ListPDFItems: %v", err)
	}
	if version != 3 {
		t.Errorf("version = %d, want 3", version)
	}
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1 (only BOOK1 has a PDF/EPUB attachment)", len(got))
	}
	book := got[0]
	if book.Key != "BOOK1" || book.Title != "A Book" {
		t.Errorf("unexpected item: %+v", book)
	}
	if len(book.Attachments) != 2 {
		t.Errorf("expected 2 attachments (pdf+epub), got %d", len(book.Attachments))
	}
	if len(book.Creators) != 1 || book.Creators[0].LastName != "Lovelace" {
		t.Errorf("creators not mapped: %+v", book.Creators)
	}
	if len(book.Tags) != 1 || book.Tags[0].Tag != "science" {
		t.Errorf("tags not mapped: %+v", book.Tags)
	}
}

func TestResolveAttachmentPath(t *testing.T) {
	fileURI := "file:///Users/dudu/Zotero/storage/ATT-PDF1/book.pdf"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items/ATT-PDF1/file/view/url" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, fileURI)
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
