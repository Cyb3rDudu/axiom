package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// TestChatListPaginationIncludesTotalPages covers the total_pages
// field added for frontend pagination UI (audit fix).
func TestChatListPaginationIncludesTotalPages(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	f.chats.page = repo.Paginated{Total: 55, Page: 1, PageSize: 10, TotalPages: 6}
	status, body := doAuthed(f, http.MethodGet, "/api/chats/", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d", status)
	}
	if !bytes.Contains(body, []byte(`"total_pages":6`)) {
		t.Errorf("total_pages missing: %s", body)
	}
}

// TestChatUnifiedPUTUpdatesTitleAndSettings hits the new PUT /api/chats/{id}
// that accepts either field.
func TestChatUnifiedPUTUpdatesTitleAndSettings(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	id := uuid.New().String()

	// Empty body → 400.
	status, _ := doAuthed(f, http.MethodPut, "/api/chats/"+id, `{}`)
	if status != http.StatusBadRequest {
		t.Errorf("empty body: got %d", status)
	}

	// Bad body → 400.
	status, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id, "{")
	if status != http.StatusBadRequest {
		t.Errorf("bad body: got %d", status)
	}

	// Title-only update returns the chat.
	status, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id, `{"title":"Renamed"}`)
	if status != http.StatusOK {
		t.Errorf("title only: got %d", status)
	}

	// Settings-only update returns the chat.
	status, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id, `{"settings":{"theme":"dark"}}`)
	if status != http.StatusOK {
		t.Errorf("settings only: got %d", status)
	}

	// Both fields at once.
	status, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id, `{"title":"x","settings":{"y":1}}`)
	if status != http.StatusOK {
		t.Errorf("both: got %d", status)
	}

	// UpdateTitle NF surfaces as 404.
	f.chats.updNF = true
	status, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id, `{"title":"z"}`)
	if status != http.StatusNotFound {
		t.Errorf("title NF: got %d", status)
	}
	f.chats.updNF = false

	// UpdateTitle generic error → 500.
	f.chats.upd = errors.New("db")
	status, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id, `{"title":"z"}`)
	if status != http.StatusInternalServerError {
		t.Errorf("title err: got %d", status)
	}
	f.chats.upd = nil

	// Refetch failure → 500.
	f.chats.get = errors.New("db")
	status, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id, `{"title":"z"}`)
	if status != http.StatusInternalServerError {
		t.Errorf("refetch err: got %d", status)
	}
}

// TestRAGChunkByIDIncludesEmptyRelationships ensures the frontend's
// chunk-detail page receives the relationships + entities arrays that
// Python emits (audit fix).
func TestRAGChunkByIDIncludesEmptyRelationships(t *testing.T) {
	t.Parallel()
	f := newStubDocFixture(api.DocumentPaths{})
	defer f.close()
	f.chunks.getResp = repo.Chunk{ID: uuid.New(), ChunkID: "abc_0", Text: "hello"}
	s, body := docReq(f, http.MethodGet, "/api/rag/chunks/abc_0", "")
	if s != http.StatusOK {
		t.Fatalf("status: %d", s)
	}
	if !bytes.Contains(body, []byte(`"relationships":[]`)) {
		t.Errorf("relationships missing: %s", body)
	}
	if !bytes.Contains(body, []byte(`"entities":[]`)) {
		t.Errorf("entities missing: %s", body)
	}
}

// TestRAGEntitiesStubHasFullPagination ensures the frontend doesn't
// break on missing pagination fields in the stub payload (audit fix).
func TestRAGEntitiesStubHasFullPagination(t *testing.T) {
	t.Parallel()
	f := newStubDocFixture(api.DocumentPaths{})
	defer f.close()
	s, body := docReq(f, http.MethodGet, "/api/rag/entities", "")
	if s != http.StatusOK {
		t.Fatalf("status: %d", s)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	pag, ok := out["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("pagination missing or wrong type: %v", out["pagination"])
	}
	for _, k := range []string{"total_count", "total_pages", "has_next", "has_previous"} {
		if _, ok := pag[k]; !ok {
			t.Errorf("pagination missing key %q: %s", k, body)
		}
	}
}

// TestDocumentGroupAddDocumentAlternateRoute verifies the Python-parity
// alternate route /api/document-groups/{id}/documents/{doc_id} POST
// (audit fix).
func TestDocumentGroupAddDocumentAlternateRoute(t *testing.T) {
	t.Parallel()
	f := newStubDocFixture(api.DocumentPaths{})
	defer f.close()
	gid := uuid.New().String()
	did := uuid.New().String()
	s, _ := docReq(f, http.MethodPost, "/api/document-groups/"+gid+"/documents/"+did, "")
	if s != http.StatusOK {
		t.Errorf("alternate add route: %d", s)
	}
}
