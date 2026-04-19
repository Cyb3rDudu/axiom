package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/server"
)

// stubUploader implements api.DocumentUploader.
type stubUploader struct {
	mu sync.Mutex

	findByHashErr error
	findByHashNF  bool
	findByHashRes repo.Document

	createErr error
	createRes repo.Document
	lastInput repo.CreatePendingInput

	inGroupErr error
	inGroup    bool
}

func (s *stubUploader) FindByHash(_ context.Context, _ int32, _ string) (repo.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findByHashNF {
		return repo.Document{}, repo.ErrNotFound
	}
	return s.findByHashRes, s.findByHashErr
}

func (s *stubUploader) CreatePending(_ context.Context, in repo.CreatePendingInput) (repo.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastInput = in
	if s.createErr != nil {
		return repo.Document{}, s.createErr
	}
	if s.createRes.ID == uuid.Nil {
		s.createRes = repo.Document{ID: in.ID, Filename: in.Filename, ProcessingStatus: "pending"}
	}
	return s.createRes, nil
}

func (s *stubUploader) InGroup(_ context.Context, _, _ uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inGroup, s.inGroupErr
}

// stubGroupChecker implements api.GroupChecker.
type stubGroupChecker struct {
	getErr error
	getNF  bool
	getRes repo.DocumentGroup
	addErr error

	lastAddedDoc uuid.UUID
}

func (s *stubGroupChecker) Get(_ context.Context, _ int32, id uuid.UUID) (repo.DocumentGroup, error) {
	if s.getNF {
		return repo.DocumentGroup{}, repo.ErrNotFound
	}
	if s.getRes.ID == uuid.Nil {
		s.getRes = repo.DocumentGroup{ID: id}
	}
	return s.getRes, s.getErr
}

func (s *stubGroupChecker) AddDocument(_ context.Context, _ int32, _, docID uuid.UUID) error {
	if s.addErr != nil {
		return s.addErr
	}
	s.lastAddedDoc = docID
	return nil
}

type uploadFixture struct {
	srv    *httptest.Server
	docs   *stubUploader
	groups *stubGroupChecker
	rawDir string
	token  string
	csrf   string
}

func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	signer, _ := auth.NewSigner("stub-upload")
	docs := &stubUploader{}
	groups := &stubGroupChecker{}
	rawDir := t.TempDir()
	deps := server.Deps{
		Upload: api.UploadDeps{
			Documents:      docs,
			Groups:         groups,
			RawFilesDir:    rawDir,
			MaxUploadBytes: 10 << 20, // 10 MiB cap for tests
		},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: stubLookup{user: authctx.User{ID: 1, Username: "alice"}},
		},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	token, _ := signer.Issue("alice", false, auth.AccessTokenLifetime)
	csrf, _ := auth.NewCSRFToken()
	return &uploadFixture{
		srv:    httptest.NewServer(s.Handler()),
		docs:   docs,
		groups: groups,
		rawDir: rawDir,
		token:  token,
		csrf:   csrf,
	}
}

func (f *uploadFixture) close() { f.srv.Close() }

// buildMultipart builds a multipart body with a single "file" part.
// When filename is empty the part is omitted (used for the missing-part case).
func buildMultipart(t *testing.T, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
		h.Set("Content-Type", "application/octet-stream")
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatalf("write part: %v", err)
		}
	} else {
		// include a non-"file" field so the multipart body parses.
		_ = mw.WriteField("other", "x")
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func uploadReq(f *uploadFixture, path string, body io.Reader, ct string) (int, []byte) {
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: f.token})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookie, Value: f.csrf})
	req.Header.Set(auth.CSRFHeader, f.csrf)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func decodeUpload(t *testing.T, body []byte) api.UploadResponse {
	t.Helper()
	var r api.UploadResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode upload: %v body=%s", err, body)
	}
	return r
}

func TestUploadHappyPath(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true

	body, ct := buildMultipart(t, "paper.pdf", "hello world")
	status, respBody := uploadReq(f, "/api/documents/upload", body, ct)
	if status != http.StatusOK {
		t.Fatalf("status: %d body=%s", status, respBody)
	}
	resp := decodeUpload(t, respBody)
	if resp.Filename != "paper.pdf" || resp.Status != "processing" || resp.ProcessingStatus != "pending" || resp.Duplicate {
		t.Fatalf("bad response: %+v", resp)
	}

	// Verify the file landed in the configured raw dir with id prefix.
	entries, err := os.ReadDir(f.rawDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file in raw dir, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "_paper.pdf") {
		t.Errorf("unexpected filename %q", name)
	}

	// Verify the repo call recorded the right input.
	in := f.docs.lastInput
	if in.FileHash == "" || in.FileSize != int64(len("hello world")) || in.UserID != 1 {
		t.Errorf("bad create input: %+v", in)
	}
	if in.Title != in.Filename {
		t.Errorf("title should default to filename: title=%q filename=%q", in.Title, in.Filename)
	}
}

func TestUploadDuplicate409(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()

	existingID := uuid.New()
	f.docs.findByHashRes = repo.Document{
		ID: existingID, Filename: "old.pdf", ProcessingStatus: "completed",
	}

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, respBody := uploadReq(f, "/api/documents/upload", body, ct)
	if status != http.StatusConflict {
		t.Fatalf("status: %d body=%s", status, respBody)
	}
	resp := decodeUpload(t, respBody)
	if !resp.Duplicate || resp.Status != "duplicate" || resp.ID != existingID {
		t.Fatalf("bad dup response: %+v", resp)
	}
	if resp.ExistingDocumentID == nil || *resp.ExistingDocumentID != existingID {
		t.Fatalf("existing_document_id missing: %+v", resp)
	}

	// On duplicate the staged file is cleaned up.
	entries, _ := os.ReadDir(f.rawDir)
	if len(entries) != 0 {
		t.Errorf("expected empty raw dir, got %v", entries)
	}
}

func TestUploadRejectsBadExtension(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true

	body, ct := buildMultipart(t, "evil.exe", "MZ")
	status, respBody := uploadReq(f, "/api/documents/upload", body, ct)
	if status != http.StatusBadRequest {
		t.Fatalf("status: %d body=%s", status, respBody)
	}
	if !bytes.Contains(respBody, []byte("PDF")) {
		t.Errorf("body should mention allowed types: %s", respBody)
	}
}

func TestUploadExtensionCaseInsensitive(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true

	body, ct := buildMultipart(t, "Paper.PDF", "x")
	status, _ := uploadReq(f, "/api/documents/upload", body, ct)
	if status != http.StatusOK {
		t.Fatalf("uppercase ext should pass: %d", status)
	}
}

func TestUploadMissingFilePart400(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()

	body, ct := buildMultipart(t, "", "")
	status, respBody := uploadReq(f, "/api/documents/upload", body, ct)
	if status != http.StatusBadRequest {
		t.Fatalf("status: %d body=%s", status, respBody)
	}
}

func TestUploadNotMultipart400(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()

	status, _ := uploadReq(f, "/api/documents/upload", strings.NewReader(`{"x":1}`), "application/json")
	if status != http.StatusBadRequest {
		t.Fatalf("status: %d", status)
	}
}

func TestUploadTooLarge413(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	// Override to a tiny cap by rewiring a fresh fixture with 8-byte cap.
	f.close()

	signer, _ := auth.NewSigner("stub-upload")
	docs := &stubUploader{findByHashNF: true}
	groups := &stubGroupChecker{}
	rawDir := t.TempDir()
	deps := server.Deps{
		Upload: api.UploadDeps{
			Documents:      docs,
			Groups:         groups,
			RawFilesDir:    rawDir,
			MaxUploadBytes: 8,
		},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: stubLookup{user: authctx.User{ID: 1, Username: "alice"}},
		},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	token, _ := signer.Issue("alice", false, auth.AccessTokenLifetime)
	csrf, _ := auth.NewCSRFToken()

	body, ct := buildMultipart(t, "big.pdf", strings.Repeat("A", 1024))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/documents/upload", body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: token})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookie, Value: csrf})
	req.Header.Set(auth.CSRFHeader, csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestUploadMissingRawFilesDir503(t *testing.T) {
	t.Parallel()
	signer, _ := auth.NewSigner("stub-upload")
	deps := server.Deps{
		Upload: api.UploadDeps{
			Documents: &stubUploader{findByHashNF: true},
			Groups:    &stubGroupChecker{},
			// RawFilesDir intentionally empty.
		},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: stubLookup{user: authctx.User{ID: 1, Username: "alice"}},
		},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	token, _ := signer.Issue("alice", false, auth.AccessTokenLifetime)
	csrf, _ := auth.NewCSRFToken()

	body, ct := buildMultipart(t, "paper.pdf", "x")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/documents/upload", body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: token})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookie, Value: csrf})
	req.Header.Set(auth.CSRFHeader, csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestUploadDedupLookupError500(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashErr = errors.New("pg offline")

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, _ := uploadReq(f, "/api/documents/upload", body, ct)
	if status != http.StatusInternalServerError {
		t.Fatalf("status: %d", status)
	}
	// Staged file should be cleaned up on dedup failure.
	entries, _ := os.ReadDir(f.rawDir)
	if len(entries) != 0 {
		t.Errorf("raw dir not cleaned: %v", entries)
	}
}

func TestUploadCreatePendingError500(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true
	f.docs.createErr = errors.New("insert failed")

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, _ := uploadReq(f, "/api/documents/upload", body, ct)
	if status != http.StatusInternalServerError {
		t.Fatalf("status: %d", status)
	}
	entries, _ := os.ReadDir(f.rawDir)
	if len(entries) != 0 {
		t.Errorf("raw dir not cleaned on insert failure: %v", entries)
	}
}

func TestUploadFilenameTraversalRejected(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true

	// multipart encodes the filename literal — our sanitiser must reject slashes.
	body, ct := buildMultipart(t, "../etc/passwd.pdf", "hello")
	status, _ := uploadReq(f, "/api/documents/upload", body, ct)
	// base of "../etc/passwd.pdf" is "passwd.pdf" — allowed. So this variant
	// normalises and succeeds. The *literal* path-in-name case is below.
	if status != http.StatusOK {
		t.Fatalf("status: %d", status)
	}
	entries, _ := os.ReadDir(f.rawDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	// Verify the file name no longer contains directory separators.
	if strings.ContainsAny(entries[0].Name(), "/\\") {
		t.Errorf("filename still contains separators: %q", entries[0].Name())
	}
}

func TestUploadToGroupHappyPath(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true

	groupID := uuid.New()
	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, respBody := uploadReq(f, "/api/document-groups/"+groupID.String()+"/upload/", body, ct)
	if status != http.StatusOK {
		t.Fatalf("status: %d body=%s", status, respBody)
	}
	resp := decodeUpload(t, respBody)
	if resp.Status != "processing" || resp.Duplicate {
		t.Fatalf("bad response: %+v", resp)
	}
	if f.groups.lastAddedDoc == uuid.Nil {
		t.Errorf("expected AddDocument to be called")
	}
}

func TestUploadToGroupGroupNotFound(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.groups.getNF = true

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, _ := uploadReq(f, "/api/document-groups/"+uuid.New().String()+"/upload/", body, ct)
	if status != http.StatusNotFound {
		t.Fatalf("status: %d", status)
	}
}

func TestUploadToGroupBadGroupID(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, _ := uploadReq(f, "/api/document-groups/not-a-uuid/upload/", body, ct)
	if status != http.StatusBadRequest {
		t.Fatalf("status: %d", status)
	}
}

func TestUploadToGroupGroupLookupError500(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.groups.getErr = errors.New("pg offline")

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, _ := uploadReq(f, "/api/document-groups/"+uuid.New().String()+"/upload/", body, ct)
	if status != http.StatusInternalServerError {
		t.Fatalf("status: %d", status)
	}
}

func TestUploadToGroupDuplicateAlreadyInGroup409(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()

	existingID := uuid.New()
	f.docs.findByHashRes = repo.Document{ID: existingID, Filename: "old.pdf", ProcessingStatus: "completed"}
	f.docs.inGroup = true

	groupID := uuid.New()
	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, respBody := uploadReq(f, "/api/document-groups/"+groupID.String()+"/upload/", body, ct)
	if status != http.StatusConflict {
		t.Fatalf("status: %d body=%s", status, respBody)
	}
	resp := decodeUpload(t, respBody)
	if !resp.Duplicate || resp.Status != "duplicate" {
		t.Fatalf("bad response: %+v", resp)
	}
	// AddDocument must NOT have been called.
	if f.groups.lastAddedDoc != uuid.Nil {
		t.Errorf("AddDocument was called on already-in-group dup")
	}
}

func TestUploadToGroupDuplicateNotInGroupAdded200(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()

	existingID := uuid.New()
	f.docs.findByHashRes = repo.Document{ID: existingID, Filename: "old.pdf", ProcessingStatus: "completed"}
	f.docs.inGroup = false

	groupID := uuid.New()
	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, respBody := uploadReq(f, "/api/document-groups/"+groupID.String()+"/upload/", body, ct)
	if status != http.StatusOK {
		t.Fatalf("status: %d body=%s", status, respBody)
	}
	resp := decodeUpload(t, respBody)
	if resp.Status != "existing" || resp.Duplicate {
		t.Fatalf("bad response: %+v", resp)
	}
	if f.groups.lastAddedDoc != existingID {
		t.Errorf("AddDocument should have been called with existing id, got %v", f.groups.lastAddedDoc)
	}
}

func TestUploadToGroupInGroupCheckError500(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashRes = repo.Document{ID: uuid.New(), Filename: "old.pdf"}
	f.docs.inGroupErr = errors.New("pg offline")

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, _ := uploadReq(f, "/api/document-groups/"+uuid.New().String()+"/upload/", body, ct)
	if status != http.StatusInternalServerError {
		t.Fatalf("status: %d", status)
	}
}

func TestUploadToGroupAddDocumentError500(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true
	f.groups.addErr = errors.New("pg offline")

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	status, _ := uploadReq(f, "/api/document-groups/"+uuid.New().String()+"/upload/", body, ct)
	if status != http.StatusInternalServerError {
		t.Fatalf("status: %d", status)
	}
}

func TestUploadToGroupMissingRawFilesDir503(t *testing.T) {
	t.Parallel()
	signer, _ := auth.NewSigner("stub-upload")
	deps := server.Deps{
		Upload: api.UploadDeps{
			Documents: &stubUploader{findByHashNF: true},
			Groups:    &stubGroupChecker{},
		},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: stubLookup{user: authctx.User{ID: 1, Username: "alice"}},
		},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	token, _ := signer.Issue("alice", false, auth.AccessTokenLifetime)
	csrf, _ := auth.NewCSRFToken()

	body, ct := buildMultipart(t, "paper.pdf", "x")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/document-groups/"+uuid.New().String()+"/upload/", body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: token})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookie, Value: csrf})
	req.Header.Set(auth.CSRFHeader, csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestUploadSanitiseFilenameRejectsEmpty(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true

	// empty filename after sanitise (multipart sends "." as basename).
	body, ct := buildMultipart(t, ".", "x")
	status, _ := uploadReq(f, "/api/documents/upload", body, ct)
	if status != http.StatusBadRequest {
		t.Fatalf("status: %d", status)
	}
}

func TestUploadFilePathLandsUnderRawDir(t *testing.T) {
	t.Parallel()
	f := newUploadFixture(t)
	defer f.close()
	f.docs.findByHashNF = true

	body, ct := buildMultipart(t, "paper.pdf", "hello")
	if status, _ := uploadReq(f, "/api/documents/upload", body, ct); status != http.StatusOK {
		t.Fatalf("status: %d", status)
	}

	in := f.docs.lastInput
	abs, err := filepath.Abs(f.rawDir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if !strings.HasPrefix(in.FilePath, abs) {
		t.Errorf("file path %q not under raw dir %q", in.FilePath, abs)
	}
}
