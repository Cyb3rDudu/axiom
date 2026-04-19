package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/server"
)

// --- document stub ---

type stubDocs struct {
	listErr     error
	listResp    repo.PaginatedDocuments
	simpleErr   error
	simpleResp  []repo.Document
	getErr      error
	getNF       bool
	getResp     repo.Document
	rawErr      error
	rawNF       bool
	rawModel    models.Document
	delErr      error
	delNF       bool
	updErr      error
	updNF       bool
	updResp     repo.Document
	cancelErr   error
	cancelNF    bool
	reqErr      error
	reqCount    int
	filtErr     error
	filtResp    repo.FilterOptions
	bulkDelOK   []uuid.UUID
	bulkDelFail map[uuid.UUID]error
}

func (s *stubDocs) List(context.Context, int32, repo.DocumentListOptions) (repo.PaginatedDocuments, error) {
	return s.listResp, s.listErr
}
func (s *stubDocs) ListSimple(context.Context, int32, int, int) ([]repo.Document, error) {
	return s.simpleResp, s.simpleErr
}
func (s *stubDocs) Get(context.Context, int32, uuid.UUID) (repo.Document, error) {
	if s.getNF {
		return repo.Document{}, repo.ErrNotFound
	}
	return s.getResp, s.getErr
}
func (s *stubDocs) GetRawModel(context.Context, int32, uuid.UUID) (models.Document, error) {
	if s.rawNF {
		return models.Document{}, repo.ErrNotFound
	}
	return s.rawModel, s.rawErr
}
func (s *stubDocs) Delete(context.Context, int32, uuid.UUID) error {
	if s.delNF {
		return repo.ErrNotFound
	}
	return s.delErr
}
func (s *stubDocs) BulkDelete(context.Context, int32, []uuid.UUID) ([]uuid.UUID, map[uuid.UUID]error) {
	return s.bulkDelOK, s.bulkDelFail
}
func (s *stubDocs) UpdateMetadata(context.Context, int32, uuid.UUID, map[string]any) (repo.Document, error) {
	if s.updNF {
		return repo.Document{}, repo.ErrNotFound
	}
	return s.updResp, s.updErr
}
func (s *stubDocs) Cancel(context.Context, int32, uuid.UUID) error {
	if s.cancelNF {
		return repo.ErrNotFound
	}
	return s.cancelErr
}
func (s *stubDocs) QueueReprocess(context.Context, int32, []uuid.UUID) (int, error) {
	return s.reqCount, s.reqErr
}
func (s *stubDocs) FilterOptionsFor(context.Context, int32, *uuid.UUID) (repo.FilterOptions, error) {
	return s.filtResp, s.filtErr
}

// --- group stub ---

type stubGroups struct {
	createErr   error
	createResp  repo.DocumentGroup
	listErr     error
	listResp    []repo.DocumentGroupSummary
	getErr      error
	getNF       bool
	getResp     repo.DocumentGroup
	updErr      error
	updNF       bool
	updResp     repo.DocumentGroup
	delErr      error
	delNF       bool
	delConflict bool
	addErr      error
	addNF       bool
	remErr      error
	remNF       bool
	bulkAddOK   []uuid.UUID
	bulkAddFail map[uuid.UUID]error
	bulkRemOK   []uuid.UUID
	bulkRemFail map[uuid.UUID]error
}

func (s *stubGroups) Create(context.Context, int32, string, string) (repo.DocumentGroup, error) {
	return s.createResp, s.createErr
}
func (s *stubGroups) List(context.Context, int32, int, int) ([]repo.DocumentGroupSummary, error) {
	return s.listResp, s.listErr
}
func (s *stubGroups) Get(context.Context, int32, uuid.UUID) (repo.DocumentGroup, error) {
	if s.getNF {
		return repo.DocumentGroup{}, repo.ErrNotFound
	}
	return s.getResp, s.getErr
}
func (s *stubGroups) Update(context.Context, int32, uuid.UUID, string, string, bool) (repo.DocumentGroup, error) {
	if s.updNF {
		return repo.DocumentGroup{}, repo.ErrNotFound
	}
	return s.updResp, s.updErr
}
func (s *stubGroups) Delete(context.Context, int32, uuid.UUID) error {
	if s.delConflict {
		return repo.ErrGroupHasActiveMissions
	}
	if s.delNF {
		return repo.ErrNotFound
	}
	return s.delErr
}
func (s *stubGroups) AddDocument(context.Context, int32, uuid.UUID, uuid.UUID) error {
	if s.addNF {
		return repo.ErrNotFound
	}
	return s.addErr
}
func (s *stubGroups) RemoveDocument(context.Context, int32, uuid.UUID, uuid.UUID) error {
	if s.remNF {
		return repo.ErrNotFound
	}
	return s.remErr
}
func (s *stubGroups) BulkAddDocuments(context.Context, int32, uuid.UUID, []uuid.UUID) ([]uuid.UUID, map[uuid.UUID]error) {
	return s.bulkAddOK, s.bulkAddFail
}
func (s *stubGroups) BulkRemoveDocuments(context.Context, int32, uuid.UUID, []uuid.UUID) ([]uuid.UUID, map[uuid.UUID]error) {
	return s.bulkRemOK, s.bulkRemFail
}

// --- chunks stub ---

type stubChunks struct {
	listErr  error
	listResp repo.PaginatedChunks
	getErr   error
	getNF    bool
	getResp  repo.Chunk
}

func (s *stubChunks) List(context.Context, int32, repo.ChunkListOptions) (repo.PaginatedChunks, error) {
	return s.listResp, s.listErr
}
func (s *stubChunks) GetByChunkID(context.Context, int32, string) (repo.Chunk, error) {
	if s.getNF {
		return repo.Chunk{}, repo.ErrNotFound
	}
	return s.getResp, s.getErr
}

// stubDocFixture is a server wired with all three document-related stubs.
type stubDocFixture struct {
	srv    *httptest.Server
	docs   *stubDocs
	groups *stubGroups
	chunks *stubChunks
	token  string
	csrf   string
}

func newStubDocFixture(paths api.DocumentPaths) *stubDocFixture {
	signer, _ := auth.NewSigner("stub")
	docs := &stubDocs{}
	groups := &stubGroups{}
	chunks := &stubChunks{}
	deps := server.Deps{
		Documents:      api.DocumentDeps{Documents: docs, Paths: paths},
		DocumentGroups: api.DocumentGroupDeps{Groups: groups, Documents: docs},
		RAG:            api.RAGDeps{Chunks: chunks},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: stubLookup{user: authctx.User{ID: 1, Username: "alice"}},
		},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	token, _ := signer.Issue("alice", false, auth.AccessTokenLifetime)
	csrf, _ := auth.NewCSRFToken()
	return &stubDocFixture{
		srv:    httptest.NewServer(s.Handler()),
		docs:   docs,
		groups: groups,
		chunks: chunks,
		token:  token,
		csrf:   csrf,
	}
}

func (f *stubDocFixture) close() { f.srv.Close() }

func docReq(f *stubDocFixture, method, path, body string) (int, []byte) {
	var rdr io.Reader
	if body != "" {
		rdr = newReader(body)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: f.token})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookie, Value: f.csrf})
	req.Header.Set(auth.CSRFHeader, f.csrf)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed below
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func newReader(s string) io.Reader { return &stringReader{s: s} }

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func TestDocumentHandlersErrorBranches(t *testing.T) {
	t.Parallel()
	f := newStubDocFixture(api.DocumentPaths{})
	defer f.close()

	id := uuid.New().String()

	f.docs.listErr = errors.New("db")
	s, _ := docReq(f, http.MethodGet, "/api/documents/all", "")
	if s != http.StatusInternalServerError {
		t.Errorf("list err: %d", s)
	}
	f.docs.listErr = nil

	f.docs.simpleErr = errors.New("db")
	s, _ = docReq(f, http.MethodGet, "/api/documents/", "")
	if s != http.StatusInternalServerError {
		t.Errorf("simple err: %d", s)
	}
	f.docs.simpleErr = nil

	f.docs.getNF = true
	s, _ = docReq(f, http.MethodGet, "/api/documents/"+id, "")
	if s != http.StatusNotFound {
		t.Errorf("get nf: %d", s)
	}
	f.docs.getNF = false
	f.docs.getErr = errors.New("db")
	s, _ = docReq(f, http.MethodGet, "/api/documents/"+id, "")
	if s != http.StatusInternalServerError {
		t.Errorf("get err: %d", s)
	}
	f.docs.getErr = nil

	f.docs.rawNF = true
	s, _ = docReq(f, http.MethodGet, "/api/documents/"+id+"/view", "")
	if s != http.StatusNotFound {
		t.Errorf("view nf: %d", s)
	}
	f.docs.rawNF = false
	f.docs.rawErr = errors.New("db")
	s, _ = docReq(f, http.MethodGet, "/api/documents/"+id+"/view", "")
	if s != http.StatusInternalServerError {
		t.Errorf("view err: %d", s)
	}
	f.docs.rawErr = nil

	// View success path with markdown file on disk.
	tmp := t.TempDir()
	mdPath := filepath.Join(tmp, "x.md")
	if err := os.WriteFile(mdPath, []byte("# body"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.docs.rawModel = models.Document{ID: uuid.New(), Filename: "x.pdf", MarkdownPath: &mdPath}
	s, body := docReq(f, http.MethodGet, "/api/documents/"+f.docs.rawModel.ID.String()+"/view", "")
	if s != http.StatusOK {
		t.Errorf("view ok: %d %s", s, body)
	}

	// Metadata patch: bad json, NF, err.
	s, _ = docReq(f, http.MethodPut, "/api/documents/"+id+"/metadata", "{")
	if s != http.StatusBadRequest {
		t.Errorf("meta bad: %d", s)
	}
	f.docs.updNF = true
	s, _ = docReq(f, http.MethodPut, "/api/documents/"+id+"/metadata", `{"title":"x"}`)
	if s != http.StatusNotFound {
		t.Errorf("meta nf: %d", s)
	}
	f.docs.updNF = false
	f.docs.updErr = errors.New("db")
	s, _ = docReq(f, http.MethodPut, "/api/documents/"+id+"/metadata", `{"title":"x"}`)
	if s != http.StatusInternalServerError {
		t.Errorf("meta err: %d", s)
	}
	f.docs.updErr = nil

	// Delete: NF + err.
	f.docs.delNF = true
	s, _ = docReq(f, http.MethodDelete, "/api/documents/"+id, "")
	if s != http.StatusNotFound {
		t.Errorf("del nf: %d", s)
	}
	f.docs.delNF = false
	f.docs.delErr = errors.New("db")
	s, _ = docReq(f, http.MethodDelete, "/api/documents/"+id, "")
	if s != http.StatusInternalServerError {
		t.Errorf("del err: %d", s)
	}
	f.docs.delErr = nil

	// Cancel: NF + err.
	f.docs.cancelNF = true
	s, _ = docReq(f, http.MethodPost, "/api/documents/"+id+"/cancel", "")
	if s != http.StatusNotFound {
		t.Errorf("cancel nf: %d", s)
	}
	f.docs.cancelNF = false
	f.docs.cancelErr = errors.New("db")
	s, _ = docReq(f, http.MethodPost, "/api/documents/"+id+"/cancel", "")
	if s != http.StatusInternalServerError {
		t.Errorf("cancel err: %d", s)
	}
	f.docs.cancelErr = nil

	// Bulk-delete: bad body, all-succeed (204), partial (207).
	s, _ = docReq(f, http.MethodPost, "/api/documents/bulk-delete", "{")
	if s != http.StatusBadRequest {
		t.Errorf("bulk-del bad: %d", s)
	}
	f.docs.bulkDelOK = []uuid.UUID{uuid.New()}
	f.docs.bulkDelFail = map[uuid.UUID]error{}
	s, _ = docReq(f, http.MethodPost, "/api/documents/bulk-delete", `["00000000-0000-0000-0000-000000000001"]`)
	if s != http.StatusNoContent {
		t.Errorf("bulk-del 204: %d", s)
	}
	f.docs.bulkDelFail = map[uuid.UUID]error{uuid.New(): errors.New("x")}
	s, _ = docReq(f, http.MethodPost, "/api/documents/bulk-delete", `["00000000-0000-0000-0000-000000000001"]`)
	if s != http.StatusMultiStatus {
		t.Errorf("bulk-del 207: %d", s)
	}

	// Bulk-reprocess: bad body + err + ok.
	s, _ = docReq(f, http.MethodPost, "/api/documents/bulk-reprocess", "{")
	if s != http.StatusBadRequest {
		t.Errorf("reproc bad: %d", s)
	}
	f.docs.reqErr = errors.New("db")
	s, _ = docReq(f, http.MethodPost, "/api/documents/bulk-reprocess", `{"document_ids":["00000000-0000-0000-0000-000000000001"]}`)
	if s != http.StatusInternalServerError {
		t.Errorf("reproc err: %d", s)
	}
	f.docs.reqErr = nil
	f.docs.reqCount = 1
	s, _ = docReq(f, http.MethodPost, "/api/documents/bulk-reprocess", `{"document_ids":["00000000-0000-0000-0000-000000000001"]}`)
	if s != http.StatusOK {
		t.Errorf("reproc ok: %d", s)
	}

	// Filter options: bad group id, err, ok.
	s, _ = docReq(f, http.MethodGet, "/api/documents/filter-options?group_id=notauuid", "")
	if s != http.StatusBadRequest {
		t.Errorf("filt bad: %d", s)
	}
	f.docs.filtErr = errors.New("db")
	s, _ = docReq(f, http.MethodGet, "/api/documents/filter-options", "")
	if s != http.StatusInternalServerError {
		t.Errorf("filt err: %d", s)
	}
	f.docs.filtErr = nil

	// Images: path traversal is blocked both by Go's http.Client URL
	// normalization (client side) AND by our handler's filename check.
	// With a stdlib client the traversal attempt is rewritten to
	// /etc/passwd server-side, which doesn't match any route → 404.
	s, _ = docReq(f, http.MethodGet, "/api/images/"+id+"/..%2fetc%2fpasswd", "")
	if s != http.StatusBadRequest && s != http.StatusNotFound {
		t.Errorf("image traversal: %d (want 400 or 404)", s)
	}
	// No images dir configured → 404.
	s, _ = docReq(f, http.MethodGet, "/api/images/"+id+"/x.png", "")
	if s != http.StatusNotFound {
		t.Errorf("image no dir: %d", s)
	}
	// Serve a real file.
	imgDir := t.TempDir()
	docDir := filepath.Join(imgDir, id)
	if err := os.Mkdir(docDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docDir, "x.png"), []byte{0x89, 0x50}, 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	f2 := newStubDocFixture(api.DocumentPaths{ImagesDir: imgDir})
	defer f2.close()
	s, body = docReq(f2, http.MethodGet, "/api/images/"+id+"/x.png", "")
	if s != http.StatusOK || len(body) == 0 {
		t.Errorf("image serve: %d body-len=%d", s, len(body))
	}
}

func TestDocumentGroupHandlersErrorBranches(t *testing.T) {
	t.Parallel()
	f := newStubDocFixture(api.DocumentPaths{})
	defer f.close()
	id := uuid.New().String()
	docID := uuid.New().String()

	f.groups.listErr = errors.New("db")
	s, _ := docReq(f, http.MethodGet, "/api/document-groups/", "")
	if s != http.StatusInternalServerError {
		t.Errorf("group list err: %d", s)
	}
	f.groups.listErr = nil

	// Create: bad json, missing name, err.
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/", "{")
	if s != http.StatusBadRequest {
		t.Errorf("grp create bad: %d", s)
	}
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/", `{}`)
	if s != http.StatusBadRequest {
		t.Errorf("grp create no name: %d", s)
	}
	f.groups.createErr = errors.New("db")
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/", `{"name":"x"}`)
	if s != http.StatusInternalServerError {
		t.Errorf("grp create err: %d", s)
	}
	f.groups.createErr = nil

	// Get: NF + err.
	f.groups.getNF = true
	s, _ = docReq(f, http.MethodGet, "/api/document-groups/"+id, "")
	if s != http.StatusNotFound {
		t.Errorf("grp get nf: %d", s)
	}
	f.groups.getNF = false
	f.groups.getErr = errors.New("db")
	s, _ = docReq(f, http.MethodGet, "/api/document-groups/"+id, "")
	if s != http.StatusInternalServerError {
		t.Errorf("grp get err: %d", s)
	}
	f.groups.getErr = nil

	// Update: bad json, NF, err.
	s, _ = docReq(f, http.MethodPut, "/api/document-groups/"+id, "{")
	if s != http.StatusBadRequest {
		t.Errorf("grp upd bad: %d", s)
	}
	f.groups.updNF = true
	s, _ = docReq(f, http.MethodPut, "/api/document-groups/"+id, `{"name":"y"}`)
	if s != http.StatusNotFound {
		t.Errorf("grp upd nf: %d", s)
	}
	f.groups.updNF = false
	f.groups.updErr = errors.New("db")
	s, _ = docReq(f, http.MethodPut, "/api/document-groups/"+id, `{"name":"y"}`)
	if s != http.StatusInternalServerError {
		t.Errorf("grp upd err: %d", s)
	}
	f.groups.updErr = nil

	// Delete: conflict, NF, err.
	f.groups.delConflict = true
	s, _ = docReq(f, http.MethodDelete, "/api/document-groups/"+id, "")
	if s != http.StatusConflict {
		t.Errorf("grp del conflict: %d", s)
	}
	f.groups.delConflict = false
	f.groups.delNF = true
	s, _ = docReq(f, http.MethodDelete, "/api/document-groups/"+id, "")
	if s != http.StatusNotFound {
		t.Errorf("grp del nf: %d", s)
	}
	f.groups.delNF = false
	f.groups.delErr = errors.New("db")
	s, _ = docReq(f, http.MethodDelete, "/api/document-groups/"+id, "")
	if s != http.StatusInternalServerError {
		t.Errorf("grp del err: %d", s)
	}
	f.groups.delErr = nil

	// AddDocument NF + err.
	f.groups.addNF = true
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/"+id+"/add-document/"+docID, "")
	if s != http.StatusNotFound {
		t.Errorf("grp add nf: %d", s)
	}
	f.groups.addNF = false
	f.groups.addErr = errors.New("db")
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/"+id+"/add-document/"+docID, "")
	if s != http.StatusInternalServerError {
		t.Errorf("grp add err: %d", s)
	}
	f.groups.addErr = nil

	// Bad UUIDs on add/remove.
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/not-a-uuid/add-document/"+docID, "")
	if s != http.StatusBadRequest {
		t.Errorf("bad group id add: %d", s)
	}
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/"+id+"/add-document/not-a-uuid", "")
	if s != http.StatusBadRequest {
		t.Errorf("bad doc id add: %d", s)
	}

	// Remove NF + err.
	f.groups.remNF = true
	s, _ = docReq(f, http.MethodDelete, "/api/document-groups/"+id+"/documents/"+docID, "")
	if s != http.StatusNotFound {
		t.Errorf("grp rem nf: %d", s)
	}
	f.groups.remNF = false
	f.groups.remErr = errors.New("db")
	s, _ = docReq(f, http.MethodDelete, "/api/document-groups/"+id+"/documents/"+docID, "")
	if s != http.StatusInternalServerError {
		t.Errorf("grp rem err: %d", s)
	}
	f.groups.remErr = nil

	// Bulk add/remove: bad json.
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/"+id+"/bulk-add-documents", "{")
	if s != http.StatusBadRequest {
		t.Errorf("bulk-add bad: %d", s)
	}
	s, _ = docReq(f, http.MethodPost, "/api/document-groups/"+id+"/bulk-remove-documents", "{")
	if s != http.StatusBadRequest {
		t.Errorf("bulk-rem bad: %d", s)
	}

	// Bad group id on list-docs.
	s, _ = docReq(f, http.MethodGet, "/api/document-groups/not-a-uuid/documents/", "")
	if s != http.StatusBadRequest {
		t.Errorf("bad group id on list: %d", s)
	}
	// List-docs fanning through docs.List.
	f.docs.listErr = errors.New("db")
	s, _ = docReq(f, http.MethodGet, "/api/document-groups/"+id+"/documents/", "")
	if s != http.StatusInternalServerError {
		t.Errorf("grp listdocs err: %d", s)
	}
	f.docs.listErr = nil
}

func TestRAGHandlersErrorBranches(t *testing.T) {
	t.Parallel()
	f := newStubDocFixture(api.DocumentPaths{})
	defer f.close()

	// Bad doc_id query.
	s, _ := docReq(f, http.MethodGet, "/api/rag/chunks?doc_id=not-a-uuid", "")
	if s != http.StatusBadRequest {
		t.Errorf("bad doc_id: %d", s)
	}

	// List err.
	f.chunks.listErr = errors.New("db")
	s, _ = docReq(f, http.MethodGet, "/api/rag/chunks", "")
	if s != http.StatusInternalServerError {
		t.Errorf("list err: %d", s)
	}
	f.chunks.listErr = nil

	// Get NF + err.
	f.chunks.getNF = true
	s, _ = docReq(f, http.MethodGet, "/api/rag/chunks/abc", "")
	if s != http.StatusNotFound {
		t.Errorf("get nf: %d", s)
	}
	f.chunks.getNF = false
	f.chunks.getErr = errors.New("db")
	s, _ = docReq(f, http.MethodGet, "/api/rag/chunks/abc", "")
	if s != http.StatusInternalServerError {
		t.Errorf("get err: %d", s)
	}
	f.chunks.getErr = nil
}

// Ensure the json shape of the bulk response is marshalable with the
// bulkFail / bulkResponseFromResults helpers.
func TestBulkOperationResponseJSONShape(t *testing.T) {
	t.Parallel()
	r := api.BulkOperationResponse{
		SuccessCount: 1,
		FailedCount:  1,
		FailedItems:  []api.BulkFail{{ID: uuid.New(), Error: "boom"}},
		Message:      "x",
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !containsAll(b, `"success_count":1`, `"failed_items":[`, `"error":"boom"`) {
		t.Errorf("unexpected body: %s", b)
	}
}

func containsAll(b []byte, subs ...string) bool {
	for _, s := range subs {
		if !contains(b, s) {
			return false
		}
	}
	return true
}

func contains(b []byte, s string) bool {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}
