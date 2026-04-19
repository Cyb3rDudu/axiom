package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// DocumentUploader is the subset of repo.Documents Upload needs.
// Defined separately so tests can stub without wiring the full store.
type DocumentUploader interface {
	FindByHash(ctx context.Context, userID int32, hash string) (repo.Document, error)
	CreatePending(ctx context.Context, in repo.CreatePendingInput) (repo.Document, error)
	InGroup(ctx context.Context, docID, groupID uuid.UUID) (bool, error)
}

// GroupChecker verifies a (user, group) ownership pair without
// bringing the whole DocumentGroupStore along.
type GroupChecker interface {
	Get(ctx context.Context, userID int32, id uuid.UUID) (repo.DocumentGroup, error)
	AddDocument(ctx context.Context, userID int32, groupID, docID uuid.UUID) error
}

// UploadDeps is injected into the router alongside DocumentDeps /
// DocumentGroupDeps. Kept separate so the file-system surface is
// plainly visible in one place.
type UploadDeps struct {
	Documents      DocumentUploader
	Groups         GroupChecker
	RawFilesDir    string
	MaxUploadBytes int64 // 0 → DefaultMaxUploadBytes
}

// DefaultMaxUploadBytes caps a single upload at 2 GiB, matching the
// nginx `client_max_body_size 2000M` from the Python stack.
const DefaultMaxUploadBytes = 2 << 30

// UploadResponse is the shape documents.py returns on every upload
// outcome (success, duplicate, or added-to-group). Mirrors Python's
// ad-hoc dict exactly so the frontend sees identical keys.
type UploadResponse struct {
	ID                 uuid.UUID  `json:"id"`
	Filename           string     `json:"filename"`
	Status             string     `json:"status"`
	ProcessingStatus   string     `json:"processing_status"`
	Message            string     `json:"message"`
	Duplicate          bool       `json:"duplicate"`
	ExistingDocumentID *uuid.UUID `json:"existing_document_id,omitempty"`
}

// Upload handles POST /api/documents/upload. Writes the file to
// RawFilesDir/{doc_id}_{filename}, hashes via SHA256, deduplicates
// against the user's existing library, and inserts a pending
// Document row — the Python doc-processor (or the in-process
// ingest worker once Phase 3 lands) does the rest.
func (d UploadDeps) Upload(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	if d.RawFilesDir == "" {
		writeError(w, http.StatusServiceUnavailable, "upload directory not configured")
		return
	}

	uploaded, err := d.readAndStoreUpload(r, uid)
	if err != nil {
		handleUploadError(w, err)
		return
	}

	existing, err := d.Documents.FindByHash(r.Context(), uid, uploaded.hash)
	if err == nil {
		// Duplicate. Clean up the staged file and return 409 with
		// the matching existing document id.
		_ = os.Remove(uploaded.finalPath)
		existingID := existing.ID
		writeJSON(w, http.StatusConflict, UploadResponse{
			ID:                 existing.ID,
			Filename:           existing.Filename,
			Status:             "duplicate",
			ProcessingStatus:   existing.ProcessingStatus,
			Message:            "This document has already been uploaded: " + existing.Filename,
			Duplicate:          true,
			ExistingDocumentID: &existingID,
		})
		return
	}
	if !errors.Is(err, repo.ErrNotFound) {
		_ = os.Remove(uploaded.finalPath)
		writeError(w, http.StatusInternalServerError, "dedup lookup failed")
		return
	}

	doc, err := d.Documents.CreatePending(r.Context(), repo.CreatePendingInput{
		ID:         uploaded.docID,
		UserID:     uid,
		Filename:   uploaded.filename,
		FileSize:   uploaded.size,
		FilePath:   uploaded.finalPath,
		FileHash:   uploaded.hash,
		Title:      uploaded.filename,
		UploadedAt: time.Now().UTC(),
	})
	if err != nil {
		_ = os.Remove(uploaded.finalPath)
		writeError(w, http.StatusInternalServerError, "document insert failed")
		return
	}
	writeJSON(w, http.StatusOK, UploadResponse{
		ID:               doc.ID,
		Filename:         doc.Filename,
		Status:           "processing",
		ProcessingStatus: "pending",
		Message:          "Document uploaded and processing started",
		Duplicate:        false,
	})
}

// UploadToGroup handles POST /api/document-groups/{group_id}/upload/.
// Mirrors Upload but adds the document to the group on success; if a
// duplicate already exists and is not yet in the group, returns 200
// with `status="existing"` instead of 409.
func (d UploadDeps) UploadToGroup(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	if d.RawFilesDir == "" {
		writeError(w, http.StatusServiceUnavailable, "upload directory not configured")
		return
	}
	groupID, err := uuid.Parse(chi.URLParam(r, "group_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	if _, err := d.Groups.Get(r.Context(), uid, groupID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "document group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "group lookup failed")
		return
	}

	uploaded, err := d.readAndStoreUpload(r, uid)
	if err != nil {
		handleUploadError(w, err)
		return
	}

	existing, err := d.Documents.FindByHash(r.Context(), uid, uploaded.hash)
	if err == nil {
		_ = os.Remove(uploaded.finalPath)
		inGroup, cerr := d.Documents.InGroup(r.Context(), existing.ID, groupID)
		if cerr != nil {
			writeError(w, http.StatusInternalServerError, "group membership check failed")
			return
		}
		existingID := existing.ID
		if inGroup {
			writeJSON(w, http.StatusConflict, UploadResponse{
				ID:                 existing.ID,
				Filename:           existing.Filename,
				Status:             "duplicate",
				ProcessingStatus:   existing.ProcessingStatus,
				Message:            "Document '" + existing.Filename + "' already exists in this group",
				Duplicate:          true,
				ExistingDocumentID: &existingID,
			})
			return
		}
		if aerr := d.Groups.AddDocument(r.Context(), uid, groupID, existing.ID); aerr != nil {
			writeError(w, http.StatusInternalServerError, "add-to-group failed")
			return
		}
		writeJSON(w, http.StatusOK, UploadResponse{
			ID:                 existing.ID,
			Filename:           existing.Filename,
			Status:             "existing",
			ProcessingStatus:   existing.ProcessingStatus,
			Message:            "Existing document '" + existing.Filename + "' was added to the group",
			Duplicate:          false,
			ExistingDocumentID: &existingID,
		})
		return
	}
	if !errors.Is(err, repo.ErrNotFound) {
		_ = os.Remove(uploaded.finalPath)
		writeError(w, http.StatusInternalServerError, "dedup lookup failed")
		return
	}

	doc, err := d.Documents.CreatePending(r.Context(), repo.CreatePendingInput{
		ID:         uploaded.docID,
		UserID:     uid,
		Filename:   uploaded.filename,
		FileSize:   uploaded.size,
		FilePath:   uploaded.finalPath,
		FileHash:   uploaded.hash,
		Title:      uploaded.filename,
		UploadedAt: time.Now().UTC(),
	})
	if err != nil {
		_ = os.Remove(uploaded.finalPath)
		writeError(w, http.StatusInternalServerError, "document insert failed")
		return
	}
	if err := d.Groups.AddDocument(r.Context(), uid, groupID, doc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "add-to-group failed")
		return
	}
	writeJSON(w, http.StatusOK, UploadResponse{
		ID:               doc.ID,
		Filename:         doc.Filename,
		Status:           "processing",
		ProcessingStatus: "pending",
		Message:          "Document uploaded and processing started",
		Duplicate:        false,
	})
}

// uploadStaged holds the intermediate state produced by
// readAndStoreUpload so the caller can either commit (keep the file)
// or clean up (os.Remove).
type uploadStaged struct {
	docID     uuid.UUID
	filename  string
	size      int64
	hash      string
	finalPath string
}

// uploadErrorKind lets handleUploadError map the typed error back to
// the right HTTP status without leaking implementation details.
type uploadErrorKind int

const (
	errBadRequest uploadErrorKind = iota
	errBadExtension
	errTooLarge
	errInternal
)

type uploadError struct {
	kind uploadErrorKind
	msg  string
	wrap error
}

func (e *uploadError) Error() string {
	if e.wrap != nil {
		return e.msg + ": " + e.wrap.Error()
	}
	return e.msg
}

func (e *uploadError) Unwrap() error { return e.wrap }

func handleUploadError(w http.ResponseWriter, err error) {
	var ue *uploadError
	if !errors.As(err, &ue) {
		writeError(w, http.StatusInternalServerError, "upload failed")
		return
	}
	switch ue.kind {
	case errBadRequest:
		writeError(w, http.StatusBadRequest, ue.msg)
	case errBadExtension:
		writeError(w, http.StatusBadRequest, ue.msg)
	case errTooLarge:
		writeError(w, http.StatusRequestEntityTooLarge, ue.msg)
	default:
		writeError(w, http.StatusInternalServerError, ue.msg)
	}
}

// allowedUploadExtensions matches the Python whitelist.
var allowedUploadExtensions = map[string]struct{}{
	".pdf":      {},
	".docx":     {},
	".doc":      {},
	".md":       {},
	".markdown": {},
}

func extensionAllowed(name string) bool {
	_, ok := allowedUploadExtensions[strings.ToLower(filepath.Ext(name))]
	return ok
}

// readAndStoreUpload streams the "file" field of a multipart POST to
// disk while hashing it. It returns uploadStaged on success so the
// caller can decide whether to keep the file (commit) or delete it
// (rollback). The final filename is `{doc_id}_{sanitised_filename}`
// under RawFilesDir, mirroring the Python layout.
func (d UploadDeps) readAndStoreUpload(r *http.Request, _ int32) (uploadStaged, error) {
	maxBytes := d.MaxUploadBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxUploadBytes
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if errors.Is(err, http.ErrNotMultipart) {
			return uploadStaged{}, &uploadError{kind: errBadRequest, msg: "multipart request required"}
		}
		// Go surfaces MaxBytesReader overflows as *http.MaxBytesError
		// (Go 1.19+) — but ParseMultipartForm wraps them, so check
		// substring as a reliable fallback.
		if strings.Contains(err.Error(), "http: request body too large") {
			return uploadStaged{}, &uploadError{kind: errTooLarge, msg: "file too large"}
		}
		return uploadStaged{}, &uploadError{kind: errBadRequest, msg: "invalid multipart", wrap: err}
	}

	fh, part, err := r.FormFile("file")
	if err != nil {
		return uploadStaged{}, &uploadError{kind: errBadRequest, msg: "missing 'file' part", wrap: err}
	}
	defer func() { _ = fh.Close() }()

	originalName := sanitiseFilename(part.Filename)
	if originalName == "" {
		return uploadStaged{}, &uploadError{kind: errBadRequest, msg: "filename required"}
	}
	if !extensionAllowed(originalName) {
		return uploadStaged{}, &uploadError{
			kind: errBadExtension,
			msg:  "Only PDF, Word (docx, doc), and Markdown (md, markdown) files are supported",
		}
	}

	docID := uuid.New()
	if err := os.MkdirAll(d.RawFilesDir, 0o755); err != nil {
		return uploadStaged{}, &uploadError{kind: errInternal, msg: "create upload dir", wrap: err}
	}
	finalPath := filepath.Join(d.RawFilesDir, docID.String()+"_"+originalName)

	out, err := os.Create(finalPath) //nolint:gosec // path is validated + under RawFilesDir
	if err != nil {
		return uploadStaged{}, &uploadError{kind: errInternal, msg: "create file", wrap: err}
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), fh)
	closeErr := out.Close()
	if err != nil {
		_ = os.Remove(finalPath)
		if strings.Contains(err.Error(), "http: request body too large") {
			return uploadStaged{}, &uploadError{kind: errTooLarge, msg: "file too large"}
		}
		return uploadStaged{}, &uploadError{kind: errInternal, msg: "write file", wrap: err}
	}
	if closeErr != nil {
		_ = os.Remove(finalPath)
		return uploadStaged{}, &uploadError{kind: errInternal, msg: "close file", wrap: closeErr}
	}

	return uploadStaged{
		docID:     docID,
		filename:  originalName,
		size:      n,
		hash:      hex.EncodeToString(hash.Sum(nil)),
		finalPath: finalPath,
	}, nil
}

// sanitiseFilename strips directory components and control characters.
// The final component is preserved verbatim so the frontend can still
// present a human-readable name.
func sanitiseFilename(name string) string {
	base := filepath.Base(name)
	if base == "." || base == ".." || base == "/" || base == `\` {
		return ""
	}
	// Reject anything that would break out of the target directory.
	if strings.ContainsAny(base, "\x00/\\") {
		return ""
	}
	return strings.TrimSpace(base)
}
