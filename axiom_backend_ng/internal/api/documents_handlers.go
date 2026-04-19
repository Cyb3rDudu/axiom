package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// DocumentStore is the subset of repo.Documents the handlers need.
type DocumentStore interface {
	List(ctx context.Context, userID int32, opt repo.DocumentListOptions) (repo.PaginatedDocuments, error)
	ListSimple(ctx context.Context, userID int32, skip, limit int) ([]repo.Document, error)
	Get(ctx context.Context, userID int32, id uuid.UUID) (repo.Document, error)
	GetRawModel(ctx context.Context, userID int32, id uuid.UUID) (models.Document, error)
	Delete(ctx context.Context, userID int32, id uuid.UUID) error
	BulkDelete(ctx context.Context, userID int32, ids []uuid.UUID) ([]uuid.UUID, map[uuid.UUID]error)
	UpdateMetadata(ctx context.Context, userID int32, id uuid.UUID, patch map[string]any) (repo.Document, error)
	Cancel(ctx context.Context, userID int32, id uuid.UUID) error
	QueueReprocess(ctx context.Context, userID int32, ids []uuid.UUID) (int, error)
	FilterOptionsFor(ctx context.Context, userID int32, groupID *uuid.UUID) (repo.FilterOptions, error)
}

// DocumentPaths captures the on-disk layout used by the view + images
// endpoints so the handlers stay filesystem-agnostic. All values may be
// empty; missing paths produce a 404.
type DocumentPaths struct {
	MarkdownDir       string
	LegacyMarkdownDir string
	ImagesDir         string
}

// DocumentDeps wires the store + filesystem layout.
type DocumentDeps struct {
	Documents DocumentStore
	Paths     DocumentPaths
}

// DocumentMetadataUpdate is the PUT body.
type DocumentMetadataUpdate struct {
	Title           *string   `json:"title,omitempty"`
	Authors         *[]string `json:"authors,omitempty"`
	JournalOrSource *string   `json:"journal_or_source,omitempty"`
	PublicationYear *int      `json:"publication_year,omitempty"`
	Abstract        *string   `json:"abstract,omitempty"`
	Keywords        *[]string `json:"keywords,omitempty"`
	DOI             *string   `json:"doi,omitempty"`
}

// BulkDocumentIDs is the shape of POST bodies that take a list of UUIDs.
type BulkDocumentIDs struct {
	DocumentIDs []uuid.UUID `json:"document_ids"`
}

// BulkOperationResponse matches the Python schemas.BulkOperationResponse.
type BulkOperationResponse struct {
	SuccessCount int        `json:"success_count"`
	FailedCount  int        `json:"failed_count"`
	FailedItems  []BulkFail `json:"failed_items"`
	Message      string     `json:"message"`
}

// BulkFail is one entry of BulkOperationResponse.FailedItems.
type BulkFail struct {
	ID    uuid.UUID `json:"id"`
	Error string    `json:"error"`
}

// ListAll handles GET /api/documents/all.
func (d DocumentDeps) ListAll(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	opt := documentListOptionsFromQuery(r)
	page, err := d.Documents.List(r.Context(), uid, opt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "document list failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// ListSimple handles GET /api/documents/.
func (d DocumentDeps) ListSimple(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	skip := atoi(r.URL.Query().Get("skip"))
	limit := atoi(r.URL.Query().Get("limit"))
	docs, err := d.Documents.ListSimple(r.Context(), uid, skip, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "document list failed")
		return
	}
	if docs == nil {
		docs = []repo.Document{}
	}
	writeJSON(w, http.StatusOK, docs)
}

// Get handles GET /api/documents/{doc_id}.
func (d DocumentDeps) Get(w http.ResponseWriter, r *http.Request) {
	d.ownedDoc(w, r, "doc_id", func(uid int32, id uuid.UUID) {
		doc, err := d.Documents.Get(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "document not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "document fetch failed")
			return
		}
		writeJSON(w, http.StatusOK, doc)
	})
}

// View handles GET /api/documents/{doc_id}/view.
func (d DocumentDeps) View(w http.ResponseWriter, r *http.Request) {
	d.ownedDoc(w, r, "doc_id", func(uid int32, id uuid.UUID) {
		m, err := d.Documents.GetRawModel(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "document not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "document fetch failed")
			return
		}

		content := ""
		for _, p := range markdownCandidates(m, d.Paths) {
			if p == "" {
				continue
			}
			b, err := os.ReadFile(p) //nolint:gosec // path comes from trusted DB column
			if err == nil {
				content = string(b)
				break
			}
		}

		out := map[string]any{
			"id":                m.ID,
			"original_filename": originalFilename(m),
			"content":           content,
			"created_at":        m.CreatedAt,
			"file_size":         m.FileSize,
		}
		if len(m.Metadata) > 0 {
			out["metadata_"] = json.RawMessage(m.Metadata)
			var meta map[string]any
			if err := json.Unmarshal(m.Metadata, &meta); err == nil {
				if title, ok := meta["title"].(string); ok {
					out["title"] = title
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// UpdateMetadata handles PUT /api/documents/{doc_id}/metadata.
func (d DocumentDeps) UpdateMetadata(w http.ResponseWriter, r *http.Request) {
	d.ownedDoc(w, r, "doc_id", func(uid int32, id uuid.UUID) {
		var req DocumentMetadataUpdate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		patch := metadataPatch(req)
		doc, err := d.Documents.UpdateMetadata(r.Context(), uid, id, patch)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "document not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "metadata update failed")
			return
		}
		writeJSON(w, http.StatusOK, doc)
	})
}

// Delete handles DELETE /api/documents/{doc_id}.
func (d DocumentDeps) Delete(w http.ResponseWriter, r *http.Request) {
	d.ownedDoc(w, r, "doc_id", func(uid int32, id uuid.UUID) {
		err := d.Documents.Delete(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "document not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "document delete failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// Cancel handles POST /api/documents/{doc_id}/cancel.
func (d DocumentDeps) Cancel(w http.ResponseWriter, r *http.Request) {
	d.ownedDoc(w, r, "doc_id", func(uid int32, id uuid.UUID) {
		err := d.Documents.Cancel(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "document not processing")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cancel failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Processing cancellation requested"})
	})
}

// BulkDelete handles POST /api/documents/bulk-delete.
func (d DocumentDeps) BulkDelete(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	var ids []uuid.UUID
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	ok, fail := d.Documents.BulkDelete(r.Context(), uid, ids)
	if len(fail) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusMultiStatus
	writeJSON(w, status, bulkResponseFromResults(ok, fail, "bulk delete completed with failures"))
}

// BulkReprocess handles POST /api/documents/bulk-reprocess.
func (d DocumentDeps) BulkReprocess(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	var req BulkDocumentIDs
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	updated, err := d.Documents.QueueReprocess(r.Context(), uid, req.DocumentIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reprocess failed")
		return
	}
	writeJSON(w, http.StatusOK, BulkOperationResponse{
		SuccessCount: updated,
		FailedCount:  len(req.DocumentIDs) - updated,
		FailedItems:  []BulkFail{},
		Message:      "documents queued for reprocessing",
	})
}

// FilterOptions handles GET /api/documents/filter-options.
func (d DocumentDeps) FilterOptions(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	var groupID *uuid.UUID
	if s := r.URL.Query().Get("group_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid group_id")
			return
		}
		groupID = &id
	}
	opts, err := d.Documents.FilterOptionsFor(r.Context(), uid, groupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "filter options failed")
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// Image handles GET /api/images/{doc_id}/{image_filename}. Resolves the
// path under ImagesDir and serves it. Prevents directory traversal.
func (d DocumentDeps) Image(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	docID, err := uuid.Parse(chi.URLParam(r, "doc_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid doc id")
		return
	}
	if _, err := d.Documents.Get(r.Context(), uid, docID); err != nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	name := chi.URLParam(r, "image_filename")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		writeError(w, http.StatusBadRequest, "invalid image filename")
		return
	}
	if d.Paths.ImagesDir == "" {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	p := filepath.Join(d.Paths.ImagesDir, docID.String(), name)
	if !strings.HasPrefix(p, filepath.Clean(d.Paths.ImagesDir)+string(os.PathSeparator)) {
		writeError(w, http.StatusBadRequest, "invalid image path")
		return
	}
	f, err := os.Open(p) //nolint:gosec // path is constructed from validated parts
	if err != nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", contentTypeForFilename(name))
	_, _ = io.Copy(w, f)
}

// ownedDoc parses {param} as a UUID and dispatches to fn when an
// authenticated user is present. 400 on bad id.
func (d DocumentDeps) ownedDoc(w http.ResponseWriter, r *http.Request, param string, fn func(int32, uuid.UUID)) {
	uid := requireUserID(r)
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+param)
		return
	}
	fn(uid, id)
}

func documentListOptionsFromQuery(r *http.Request) repo.DocumentListOptions {
	q := r.URL.Query()
	opt := repo.DocumentListOptions{
		Page:         atoi(q.Get("page")),
		Limit:        atoi(q.Get("limit")),
		Search:       q.Get("search"),
		Author:       q.Get("author"),
		Journal:      q.Get("journal"),
		DocumentType: q.Get("document_type"),
		Status:       q.Get("status"),
		SortBy:       q.Get("sort_by"),
		SortOrder:    q.Get("sort_order"),
	}
	if y := q.Get("year"); y != "" {
		if n, err := strconv.Atoi(y); err == nil {
			v := int32(n)
			opt.Year = &v
		}
	}
	if g := q.Get("group_id"); g != "" {
		if id, err := uuid.Parse(g); err == nil {
			opt.GroupID = &id
		}
	}
	return opt
}

// metadataPatch builds the JSONB patch map out of a PUT body. Only
// fields the client actually sent are included.
func metadataPatch(req DocumentMetadataUpdate) map[string]any {
	patch := map[string]any{}
	if req.Title != nil {
		patch["title"] = *req.Title
	}
	if req.Authors != nil {
		patch["authors"] = *req.Authors
	}
	if req.JournalOrSource != nil {
		patch["journal_or_source"] = *req.JournalOrSource
	}
	if req.PublicationYear != nil {
		patch["publication_year"] = *req.PublicationYear
	}
	if req.Abstract != nil {
		patch["abstract"] = *req.Abstract
	}
	if req.Keywords != nil {
		patch["keywords"] = *req.Keywords
	}
	if req.DOI != nil {
		patch["doi"] = *req.DOI
	}
	return patch
}

func markdownCandidates(m models.Document, paths DocumentPaths) []string {
	out := make([]string, 0, 3)
	if m.MarkdownPath != nil && *m.MarkdownPath != "" {
		out = append(out, *m.MarkdownPath)
	}
	if paths.MarkdownDir != "" {
		out = append(out, filepath.Join(paths.MarkdownDir, m.ID.String()+".md"))
	}
	if paths.LegacyMarkdownDir != "" {
		out = append(out, filepath.Join(paths.LegacyMarkdownDir, m.ID.String()+".md"))
	}
	return out
}

func originalFilename(m models.Document) string {
	if m.OriginalFilename != nil && *m.OriginalFilename != "" {
		return *m.OriginalFilename
	}
	return m.Filename
}

func bulkResponseFromResults(ok []uuid.UUID, fail map[uuid.UUID]error, msg string) BulkOperationResponse {
	out := BulkOperationResponse{
		SuccessCount: len(ok),
		FailedCount:  len(fail),
		FailedItems:  make([]BulkFail, 0, len(fail)),
		Message:      msg,
	}
	for id, err := range fail {
		out.FailedItems = append(out.FailedItems, BulkFail{ID: id, Error: err.Error()})
	}
	return out
}

func contentTypeForFilename(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".bmp":
		return "image/bmp"
	default:
		return "application/octet-stream"
	}
}
