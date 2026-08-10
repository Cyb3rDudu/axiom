package zotero

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// envelope builds a Zotero local-api object envelope.
func envelope(key, itemType, parent, title string, extra map[string]any) map[string]any {
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
	return map[string]any{"key": key, "version": 1, "data": data}
}

func TestListPDFItemsCursorNeverFallsBelowSince(t *testing.T) {
	var lastSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastSince = r.URL.Query().Get("since")
		// Header version 181 while since-covered delta returns no items.
		w.Header().Set("Last-Modified-Version", "181")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	res, err := api.ListPDFItems(179)
	if err != nil {
		t.Fatalf("ListPDFItems: %v", err)
	}
	if res.NewVersion != 181 {
		t.Fatalf("newVersion = %d, want 181 (from header)", res.NewVersion)
	}
	if !strings.Contains(lastSince, "179") {
		t.Fatalf("since query = %q, want 179", lastSince)
	}
}

func TestListPDFItemsFullSync(t *testing.T) {
	items := []map[string]any{
		envelope("B1", "book", "", "A Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace"}},
		}),
		map[string]any{
			"key": "A1", "version": 1,
			"links": map[string]any{"enclosure": map[string]any{"href": "file:///X/storage/A1/doc.pdf"}},
			"data": map[string]any{
				"key": "A1", "version": 1, "itemType": "attachment", "parentItem": "B1",
				"contentType": "application/pdf", "filename": "doc.pdf",
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "5")
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
	listed := res.Items
	ver := res.NewVersion
	if ver != 5 {
		t.Fatalf("version = %d, want 5", ver)
	}
	if len(listed) != 1 || listed[0].Key != "B1" || len(listed[0].Attachments) != 1 {
		t.Fatalf("full sync mismatch: %+v", listed)
	}
}

func TestListPDFItemsDeltaReconstructsParentOnAttachmentChange(t *testing.T) {
	// Server routes:
	//   GET /items?since=100 -> only the changed attachment A2 (parent B1 changed indirectly)
	//   GET /items/B1/children -> the full attachment list (A1, A2)
	//   GET /items/B1 -> the parent document
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if strings.HasSuffix(path, "/items") {
			w.Header().Set("Last-Modified-Version", "110")
			// delta: only the changed attachment is present, no parent
			a2 := map[string]any{
				"key": "A2", "version": 9,
				"links": map[string]any{"enclosure": map[string]any{"href": "file:///X/A2/doc.pdf"}},
				"data": map[string]any{
					"key": "A2", "version": 9, "itemType": "attachment", "parentItem": "B1",
					"contentType": "application/pdf", "filename": "doc2.pdf",
				},
			}
			b, _ := json.Marshal([]map[string]any{a2})
			w.Write(b)
			return
		}
		if strings.HasSuffix(path, "/children") && strings.Contains(path, "/B1/children") {
			// full current children of B1 (A1 + A2)
			a1 := map[string]any{
				"key": "A1", "version": 3,
				"links": map[string]any{"enclosure": map[string]any{"href": "file:///X/A1/doc.pdf"}},
				"data": map[string]any{
					"key": "A1", "version": 3, "itemType": "attachment", "parentItem": "B1",
					"contentType": "application/pdf", "filename": "doc1.pdf",
				},
			}
			b, _ := json.Marshal([]map[string]any{a1})
			w.Write(b)
			return
		}
		if strings.HasSuffix(path, "/B1") {
			parent := envelope("B1", "book", "", "A Book", nil)
			b, _ := json.Marshal(parent)
			w.Write(b)
			return
		}
		http.Error(w, "unexpected path "+path, http.StatusNotFound)
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	res, err := api.ListPDFItems(100)
	if err != nil {
		t.Fatalf("ListPDFItems: %v", err)
	}
	listed := res.Items
	ver := res.NewVersion
	if ver != 110 {
		t.Fatalf("version = %d, want 110", ver)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 reconstructed parent, got %+v", listed)
	}
	if listed[0].Key != "B1" || listed[0].Title != "A Book" {
		t.Fatalf("parent not reconstructed: %+v", listed[0])
	}
	if len(listed[0].Attachments) != 1 || listed[0].Attachments[0].Key != "A1" {
		t.Fatalf("expected children to be reconstructed from /children, got %+v", listed[0].Attachments)
	}
}

// TestListPDFItemsErrorsWhenReconstructionFails verifies that a transient
// failure during delta reconstruction (e.g. /children returning 500) surfaces
// as an error instead of silently advancing the sync cursor over incomplete
// data.
func TestListPDFItemsErrorsWhenReconstructionFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if strings.HasSuffix(path, "/items") {
			w.Header().Set("Last-Modified-Version", "110")
			// Delta: parent B1 changed (only the parent appears).
			b, _ := json.Marshal([]map[string]any{envelope("B1", "book", "", "A Book", nil)})
			w.Write(b)
			return
		}
		// /items/B1/children -> transient 500
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	_, err := api.ListPDFItems(100)
	if err == nil {
		t.Fatal("expected error when children reconstruction fails; cursor must not advance")
	}
}
