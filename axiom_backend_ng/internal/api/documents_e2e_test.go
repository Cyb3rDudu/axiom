package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// seedDocument inserts a document row directly via GORM so the tests
// can exercise list/get/view/delete without spinning up the ingest
// pipeline. Returns the new id + markdown path actually written to disk.
func seedDocument(t *testing.T, f *fixture, userID int32, filename, title, markdownBody string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	metaJSON, _ := json.Marshal(map[string]any{
		"title":             title,
		"authors":           []string{"Alice", "Bob"},
		"publication_year":  2025,
		"journal_or_source": "Test Journal",
		"document_type":     "article",
	})

	var mdPath string
	if markdownBody != "" {
		dir := t.TempDir()
		mdPath = filepath.Join(dir, id.String()+".md")
		if err := os.WriteFile(mdPath, []byte(markdownBody), 0o644); err != nil {
			t.Fatalf("write markdown: %v", err)
		}
	}

	err := f.pg.DB.WithContext(context.Background()).Exec(`
		INSERT INTO documents (id, user_id, filename, original_filename, metadata_,
		                       processing_status, upload_progress, chunk_count,
		                       dense_collection_name, sparse_collection_name,
		                       markdown_path, file_size,
		                       created_at, updated_at)
		VALUES (?, ?, ?, ?, ?::jsonb, 'completed', 100, 3, 'documents_dense', 'documents_sparse', ?, 1234, NOW(), NOW())
	`, id, userID, filename, filename, string(metaJSON), nullable(mdPath)).Error
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}
	return id, mdPath
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestDocumentLibraryReadFlow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, csrf := f.registerAndLogin(t, "lib-reader", "hunter22")

	uid := userIDFor(t, f, "lib-reader")
	docID, mdPath := seedDocument(t, f, uid, "paper.pdf", "A Paper", "# Hello\n")

	// List all (default filters): paginated envelope.
	status, body := f.do(t, client, http.MethodGet, "/api/documents/all", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list all: %d %s", status, body)
	}
	var page repo.PaginatedDocuments
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Documents) != 1 || page.Documents[0].Title != "A Paper" {
		t.Errorf("list all: got %+v", page.Documents)
	}
	if page.Pagination.TotalCount != 1 {
		t.Errorf("total count: %d", page.Pagination.TotalCount)
	}

	// Search filter narrows + returns 0.
	status, body = f.do(t, client, http.MethodGet, "/api/documents/all?search=notmatching", "", nil)
	if status != http.StatusOK {
		t.Fatalf("search list: %d %s", status, body)
	}
	_ = json.Unmarshal(body, &page)
	if page.Pagination.TotalCount != 0 {
		t.Errorf("search narrowing broken: %d", page.Pagination.TotalCount)
	}

	// Simple list (/api/documents/).
	status, body = f.do(t, client, http.MethodGet, "/api/documents/", "", nil)
	if status != http.StatusOK {
		t.Fatalf("simple list: %d %s", status, body)
	}
	var simple []repo.Document
	_ = json.Unmarshal(body, &simple)
	if len(simple) != 1 {
		t.Errorf("simple list: got %d docs", len(simple))
	}

	// Single doc.
	status, body = f.do(t, client, http.MethodGet, "/api/documents/"+docID.String(), "", nil)
	if status != http.StatusOK {
		t.Fatalf("get doc: %d %s", status, body)
	}

	// View — should read the markdown we wrote.
	status, body = f.do(t, client, http.MethodGet, "/api/documents/"+docID.String()+"/view", "", nil)
	if status != http.StatusOK {
		t.Fatalf("view: %d %s", status, body)
	}
	if !bytes.Contains(body, []byte("# Hello")) {
		t.Errorf("view should include markdown body: %s", body)
	}

	// Filter options.
	status, body = f.do(t, client, http.MethodGet, "/api/documents/filter-options", "", nil)
	if status != http.StatusOK {
		t.Fatalf("filter-options: %d", status)
	}
	var opts repo.FilterOptions
	_ = json.Unmarshal(body, &opts)
	if len(opts.Years) == 0 || opts.Years[0] != 2025 {
		t.Errorf("filter-options year: %+v", opts.Years)
	}

	// Metadata patch.
	status, body = f.do(t, client, http.MethodPut, "/api/documents/"+docID.String()+"/metadata", csrf, map[string]any{
		"title": "Renamed Paper",
	})
	if status != http.StatusOK {
		t.Fatalf("metadata patch: %d %s", status, body)
	}
	var doc repo.Document
	_ = json.Unmarshal(body, &doc)
	if doc.Title != "Renamed Paper" {
		t.Errorf("title patch: got %q", doc.Title)
	}

	// Bulk-reprocess marks it pending.
	status, _ = f.do(t, client, http.MethodPost, "/api/documents/bulk-reprocess", csrf, map[string]any{
		"document_ids": []uuid.UUID{docID},
	})
	if status != http.StatusOK {
		t.Errorf("bulk-reprocess: %d", status)
	}

	// Cancel only works when the doc is in 'processing' — we just
	// flipped it to 'pending', so this should 404.
	status, _ = f.do(t, client, http.MethodPost, "/api/documents/"+docID.String()+"/cancel", csrf, nil)
	if status != http.StatusNotFound {
		t.Errorf("cancel when not processing: got %d, want 404", status)
	}

	// Move back to processing, then cancel should 200.
	if err := f.pg.DB.Exec(`UPDATE documents SET processing_status='processing' WHERE id=?`, docID).Error; err != nil {
		t.Fatalf("update status: %v", err)
	}
	status, _ = f.do(t, client, http.MethodPost, "/api/documents/"+docID.String()+"/cancel", csrf, nil)
	if status != http.StatusOK {
		t.Errorf("cancel processing: got %d", status)
	}

	// Bad UUID paths.
	status, _ = f.do(t, client, http.MethodGet, "/api/documents/not-a-uuid", "", nil)
	if status != http.StatusBadRequest {
		t.Errorf("bad uuid: got %d", status)
	}

	// Delete.
	status, _ = f.do(t, client, http.MethodDelete, "/api/documents/"+docID.String(), csrf, nil)
	if status != http.StatusNoContent {
		t.Errorf("delete: got %d", status)
	}
	status, _ = f.do(t, client, http.MethodGet, "/api/documents/"+docID.String(), "", nil)
	if status != http.StatusNotFound {
		t.Errorf("post-delete get: got %d", status)
	}

	_ = mdPath // silence linter if test is restructured
}

func TestDocumentGroupsLifecycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, csrf := f.registerAndLogin(t, "group-user", "hunter22")
	uid := userIDFor(t, f, "group-user")

	// Seed 2 docs to attach.
	doc1, _ := seedDocument(t, f, uid, "d1.pdf", "D1", "")
	doc2, _ := seedDocument(t, f, uid, "d2.pdf", "D2", "")

	// Empty list.
	status, body := f.do(t, client, http.MethodGet, "/api/document-groups/", "", nil)
	if status != http.StatusOK || !bytes.Equal(bytes.TrimSpace(body), []byte("[]")) {
		t.Errorf("empty list: %d %s", status, body)
	}

	// Create.
	status, body = f.do(t, client, http.MethodPost, "/api/document-groups/", csrf, map[string]string{
		"name": "Core Library", "description": "Papers I care about",
	})
	if status != http.StatusCreated {
		t.Fatalf("create group: %d %s", status, body)
	}
	var g repo.DocumentGroup
	_ = json.Unmarshal(body, &g)
	if g.Name != "Core Library" {
		t.Errorf("name: %q", g.Name)
	}

	// Add doc1.
	status, _ = f.do(t, client, http.MethodPost, "/api/document-groups/"+g.ID.String()+"/add-document/"+doc1.String(), csrf, nil)
	if status != http.StatusOK {
		t.Errorf("add doc1: %d", status)
	}
	// Adding again is a no-op (ON CONFLICT).
	status, _ = f.do(t, client, http.MethodPost, "/api/document-groups/"+g.ID.String()+"/add-document/"+doc1.String(), csrf, nil)
	if status != http.StatusOK {
		t.Errorf("idempotent add: %d", status)
	}

	// Bulk-add doc2.
	status, _ = f.do(t, client, http.MethodPost, "/api/document-groups/"+g.ID.String()+"/bulk-add-documents", csrf, []uuid.UUID{doc2})
	if status != http.StatusOK {
		t.Errorf("bulk add: %d", status)
	}

	// List documents in group: 2 rows.
	status, body = f.do(t, client, http.MethodGet, "/api/document-groups/"+g.ID.String()+"/documents/", "", nil)
	if status != http.StatusOK {
		t.Fatalf("group docs: %d %s", status, body)
	}
	var page repo.PaginatedDocuments
	_ = json.Unmarshal(body, &page)
	if page.Pagination.TotalCount != 2 {
		t.Errorf("group docs count: got %d, want 2", page.Pagination.TotalCount)
	}

	// Remove doc1.
	status, _ = f.do(t, client, http.MethodDelete, "/api/document-groups/"+g.ID.String()+"/documents/"+doc1.String(), csrf, nil)
	if status != http.StatusOK {
		t.Errorf("remove: %d", status)
	}

	// Bulk-remove doc2.
	status, _ = f.do(t, client, http.MethodPost, "/api/document-groups/"+g.ID.String()+"/bulk-remove-documents", csrf, []uuid.UUID{doc2})
	if status != http.StatusOK {
		t.Errorf("bulk remove: %d", status)
	}

	// Update group name.
	status, body = f.do(t, client, http.MethodPut, "/api/document-groups/"+g.ID.String(), csrf, map[string]any{
		"name": "Renamed", "description": nil,
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %s", status, body)
	}

	// Get + verify the rename.
	status, body = f.do(t, client, http.MethodGet, "/api/document-groups/"+g.ID.String(), "", nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %s", status, body)
	}
	_ = json.Unmarshal(body, &g)
	if g.Name != "Renamed" {
		t.Errorf("rename: got %q", g.Name)
	}

	// Delete.
	status, _ = f.do(t, client, http.MethodDelete, "/api/document-groups/"+g.ID.String(), csrf, nil)
	if status != http.StatusNoContent {
		t.Errorf("delete: %d", status)
	}

	// Post-delete get is 404.
	status, _ = f.do(t, client, http.MethodGet, "/api/document-groups/"+g.ID.String(), "", nil)
	if status != http.StatusNotFound {
		t.Errorf("post-delete get: %d", status)
	}
}

func TestDocumentGroupDeleteRejectsActiveMission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, csrf := f.registerAndLogin(t, "gblocker", "hunter22")
	uid := userIDFor(t, f, "gblocker")

	status, body := f.do(t, client, http.MethodPost, "/api/document-groups/", csrf, map[string]string{
		"name": "Blocked",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var g repo.DocumentGroup
	_ = json.Unmarshal(body, &g)

	// Seed a chat + mission that references this group via generated_document_group_id.
	chatID := uuid.New()
	missionID := uuid.New()
	err := f.pg.DB.Exec(`
		INSERT INTO chats (id, user_id, title, chat_type, created_at, updated_at)
		VALUES (?, ?, 'c', 'research', NOW(), NOW())`, chatID, uid).Error
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	err = f.pg.DB.Exec(`
		INSERT INTO missions (id, chat_id, user_request, status, generated_document_group_id, created_at, updated_at)
		VALUES (?, ?, 'x', 'running', ?, NOW(), NOW())`, missionID, chatID, g.ID).Error
	if err != nil {
		t.Fatalf("mission: %v", err)
	}

	status, body = f.do(t, client, http.MethodDelete, "/api/document-groups/"+g.ID.String(), csrf, nil)
	if status != http.StatusConflict {
		t.Errorf("delete while mission active: got %d, want 409 (body=%s)", status, body)
	}
}

func TestRAGChunksList(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, _ := f.registerAndLogin(t, "rag-user", "hunter22")
	uid := userIDFor(t, f, "rag-user")

	docID, _ := seedDocument(t, f, uid, "rag.pdf", "RAG Doc", "")
	// Seed a few chunks straight into the DB.
	for i := 0; i < 3; i++ {
		if err := f.pg.DB.Exec(`
			INSERT INTO document_chunks (id, doc_id, chunk_id, chunk_index, chunk_text,
			                             sparse_embedding, chunk_metadata, created_at)
			VALUES (gen_random_uuid(), ?, ?, ?, ?, '{}'::jsonb, '{}'::jsonb, NOW())`,
			docID, docID.String()+"_"+strings.Repeat("x", i+1), i, "chunk text "+string(rune('A'+i))).Error; err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
	}

	status, body := f.do(t, client, http.MethodGet, "/api/rag/chunks?doc_id="+docID.String(), "", nil)
	if status != http.StatusOK {
		t.Fatalf("list chunks: %d %s", status, body)
	}
	var page repo.PaginatedChunks
	_ = json.Unmarshal(body, &page)
	if page.Pagination.TotalCount != 3 {
		t.Errorf("chunk count: got %d, want 3", page.Pagination.TotalCount)
	}
	if len(page.Chunks) == 0 {
		t.Fatal("expected chunks in response")
	}
	chunkID := page.Chunks[0].ChunkID

	// Fetch single chunk.
	status, body = f.do(t, client, http.MethodGet, "/api/rag/chunks/"+chunkID, "", nil)
	if status != http.StatusOK {
		t.Fatalf("get chunk: %d %s", status, body)
	}

	// Entities + graph stubs return empty payloads.
	status, body = f.do(t, client, http.MethodGet, "/api/rag/entities", "", nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"entities":[]`)) {
		t.Errorf("entities stub: %d %s", status, body)
	}
	status, body = f.do(t, client, http.MethodGet, "/api/rag/graph", "", nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"nodes":[]`)) {
		t.Errorf("graph stub: %d %s", status, body)
	}
}

// userIDFor looks up the numeric ID of a registered user so tests can
// seed rows owned by them.
func userIDFor(t *testing.T, f *fixture, username string) int32 {
	t.Helper()
	var id int32
	if err := f.pg.DB.Raw(`SELECT id FROM users WHERE username = ?`, username).Scan(&id).Error; err != nil {
		t.Fatalf("userIDFor: %v", err)
	}
	if id == 0 {
		t.Fatalf("no user row for %q", username)
	}
	return id
}
