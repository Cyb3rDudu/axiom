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

// TestChildrenPaginationReconstructsAll: a parent with more than `limit`
// children (default 100) must have all of them reconstructed, not truncated.
func TestChildrenPaginationReconstructsAll(t *testing.T) {
	// children[i] = attachment key AX<i> for parent B1
	const total = 101
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if strings.HasSuffix(path, "/items") {
			w.Header().Set("Last-Modified-Version", "200")
			// Delta: parent B1 changed.
			b, _ := json.Marshal([]map[string]any{envelope("B1", "book", "", "A Book", nil)})
			w.Write(b)
			return
		}
		if strings.HasSuffix(path, "/children") && strings.Contains(path, "/B1/children") {
			start := 0
			limit := 100
			if s := r.URL.Query().Get("start"); s != "" {
				fmt.Sscanf(s, "%d", &start)
			}
			if l := r.URL.Query().Get("limit"); l != "" {
				fmt.Sscanf(l, "%d", &limit)
			}
			end := start + limit
			if end > total {
				end = total
			}
			items := make([]map[string]any, 0, end-start)
			for i := start; i < end; i++ {
				key := fmt.Sprintf("AX%d", i)
				items = append(items, map[string]any{
					"key": key, "version": 1,
					"links": map[string]any{"enclosure": map[string]any{"href": "file:///X/" + key + "/c.pdf"}},
					"data": map[string]any{
						"key": key, "version": 1, "itemType": "attachment", "parentItem": "B1",
						"contentType": "application/pdf", "filename": "c.pdf",
					},
				})
			}
			b, _ := json.Marshal(items)
			w.Write(b)
			return
		}
		if strings.HasSuffix(path, "/B1") {
			b, _ := json.Marshal(envelope("B1", "book", "", "A Book", nil))
			w.Write(b)
			return
		}
		http.Error(w, "unexpected path "+path, http.StatusNotFound)
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	res, err := api.ListPDFItems(150)
	if err != nil {
		t.Fatalf("ListPDFItems: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("expected 1 reconstructed parent, got %d", len(res.Items))
	}
	atts := res.Items[0].Attachments
	if len(atts) != total {
		t.Fatalf("expected %d children reconstructed, got %d", total, len(atts))
	}
	seen := map[string]bool{}
	for _, a := range atts {
		seen[a.Key] = true
	}
	if !seen["AX0"] || !seen["AX100"] {
		t.Errorf("children across pages must all be present (got keys: has0=%v has100=%v)", seen["AX0"], seen["AX100"])
	}
}

// TestCanonicalItemsLosslessRoundtrip: unknown fields in an item envelope must
// survive the semantic JSON round-trip through ListCanonicalItems.
func TestCanonicalItemsLosslessRoundtrip(t *testing.T) {
	const unknownEnv = `{"key":"B1","version":3,"library":{"type":"user"},"x_custom_top":42,"data":{"key":"B1","version":3,"itemType":"book","title":"A Book","extraField":{"nested":[1,2,3]},"creator":"unknown-field"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "7")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[%s]`, unknownEnv)
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	items, ver, err := api.ListCanonicalItems(0)
	if err != nil {
		t.Fatalf("ListCanonicalItems: %v", err)
	}
	if ver != 7 {
		t.Errorf("version = %d, want 7", ver)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	it := items[0]
	if it.Key != "B1" || it.ItemType != "book" || it.Version != 3 {
		t.Errorf("dims: key=%s type=%s ver=%d", it.Key, it.ItemType, it.Version)
	}
	// Envelope must contain the unknown top-level field.
	if !strings.Contains(string(it.Envelope), `"x_custom_top":42`) {
		t.Errorf("envelope lost unknown top-level field: %s", it.Envelope)
	}
	// Data must contain the unknown nested field.
	if !strings.Contains(string(it.Data), `"extraField"`) || !strings.Contains(string(it.Data), `"unknown-field"`) {
		t.Errorf("data lost unknown fields: %s", it.Data)
	}
}

// TestCanonicalCollectionsPagination: more than `limit` collections must all be
// returned (pagination).
func TestCanonicalCollectionsPagination(t *testing.T) {
	const total = 120
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		start, limit := 0, 100
		if s := r.URL.Query().Get("start"); s != "" {
			fmt.Sscanf(s, "%d", &start)
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
		}
		end := start + limit
		if end > total {
			end = total
		}
		var parts []string
		for i := start; i < end; i++ {
			parts = append(parts, fmt.Sprintf(`{"key":"C%d","data":{"key":"C%d","name":"col%d","parentCollection":false}}`, i, i, i))
		}
		fmt.Fprintf(w, `[%s]`, strings.Join(parts, ","))
	}))
	defer srv.Close()

	api := NewLocalAPI(srv.URL, "users/0", WithHTTPClient(srv.Client()))
	cols, err := api.ListCanonicalCollections()
	if err != nil {
		t.Fatalf("ListCanonicalCollections: %v", err)
	}
	if len(cols) != total {
		t.Fatalf("expected %d collections, got %d", total, len(cols))
	}
}
