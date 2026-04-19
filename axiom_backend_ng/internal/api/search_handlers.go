package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/retriever"
)

// SearchFulltextClient is the subset of *opensearch.Client the handler
// uses. Tests stub it directly.
type SearchFulltextClient interface {
	BM25Search(ctx context.Context, opt opensearch.SearchOptions) ([]opensearch.Hit, error)
}

// HybridRetriever is the subset of *retriever.Retriever the /api/search
// handler uses.
type HybridRetriever interface {
	Retrieve(ctx context.Context, opt retriever.Options) ([]retriever.Result, error)
}

// UserDocIDs returns the UUIDs the current user owns, used to scope
// OpenSearch queries. Any implementation that reads from the documents
// table works. DocIDsInGroup narrows the list to documents that belong
// to the given group (ownership still enforced).
type UserDocIDs interface {
	DocIDs(ctx context.Context, userID int32) ([]uuid.UUID, error)
	DocIDsInGroup(ctx context.Context, userID int32, groupID uuid.UUID) ([]uuid.UUID, error)
}

// SearchDeps wires fulltext + hybrid search.
type SearchDeps struct {
	OpenSearch SearchFulltextClient
	Retriever  HybridRetriever
	UserDocs   UserDocIDs
}

// FulltextHit matches axiom_backend/api/documents.py's search/fulltext
// response item shape (dedup'd by doc_id, highest-score kept, snippet
// truncated at 200 chars). Extra fields (authors, year, type,
// original_filename) are surfaced from the indexed metadata — the
// frontend's search-results card reads them directly.
type FulltextHit struct {
	ID               uuid.UUID      `json:"id"`
	Title            string         `json:"title,omitempty"`
	OriginalFilename string         `json:"original_filename,omitempty"`
	Authors          []string       `json:"authors,omitempty"`
	PublicationYear  *int           `json:"publication_year,omitempty"`
	DocumentType     string         `json:"document_type,omitempty"`
	Score            float64        `json:"score"`
	Snippet          string         `json:"snippet"`
	Metadata         map[string]any `json:"metadata_,omitempty"`
}

// Fulltext handles GET /api/documents/search/fulltext. 503 when
// OpenSearch is disabled; empty slice when the query has no query
// string or no matches.
func (d SearchDeps) Fulltext(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	q := r.URL.Query().Get("query")
	if len(q) < 2 {
		writeError(w, http.StatusBadRequest, "query must be at least 2 characters")
		return
	}
	if d.OpenSearch == nil {
		writeError(w, http.StatusServiceUnavailable, "OpenSearch disabled")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limit = n
		}
	}
	var groupFilter *uuid.UUID
	if v := r.URL.Query().Get("group_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid group_id")
			return
		}
		groupFilter = &id
	}

	docIDs, err := d.resolveUserDocIDs(r.Context(), uid, groupFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "document list failed")
		return
	}
	if len(docIDs) == 0 {
		writeJSON(w, http.StatusOK, []FulltextHit{})
		return
	}

	hits, err := d.OpenSearch.BM25Search(r.Context(), opensearch.SearchOptions{
		Query:  q,
		DocIDs: docIDs,
		Size:   limit * 3, // over-fetch so post-dedup we can still hit `limit`
	})
	if err != nil {
		if errors.Is(err, opensearch.ErrDisabled) {
			writeError(w, http.StatusServiceUnavailable, "OpenSearch disabled")
			return
		}
		writeError(w, http.StatusBadGateway, "OpenSearch query failed")
		return
	}

	// Dedup by doc_id — keep the highest-scoring chunk per document
	// (matches Python's behaviour in documents.py:160-165).
	seen := map[uuid.UUID]FulltextHit{}
	order := []uuid.UUID{}
	for _, h := range hits {
		snippet := h.Text
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		cur, ok := seen[h.DocID]
		if ok && cur.Score >= h.Score {
			continue
		}
		if !ok {
			order = append(order, h.DocID)
		}
		seen[h.DocID] = FulltextHit{
			ID:               h.DocID,
			Score:            h.Score,
			Snippet:          snippet,
			Metadata:         h.Metadata,
			Title:            stringFromMetadata(h.Metadata, "title"),
			OriginalFilename: stringFromMetadata(h.Metadata, "original_filename"),
			Authors:          stringSliceFromMetadata(h.Metadata, "authors"),
			PublicationYear:  intFromMetadata(h.Metadata, "publication_year"),
			DocumentType:     stringFromMetadata(h.Metadata, "document_type"),
		}
	}
	out := make([]FulltextHit, 0, len(order))
	for _, id := range order {
		out = append(out, seen[id])
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// Search handles GET /api/search/. Wraps Retriever.Retrieve with the
// `{results: [...]}` envelope the Python API emits.
func (d SearchDeps) Search(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	n := 10
	if v := r.URL.Query().Get("n_results"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			if parsed > 100 {
				parsed = 100
			}
			n = parsed
		}
	}
	if d.Retriever == nil {
		writeJSON(w, http.StatusOK, map[string]any{"results": []any{}})
		return
	}

	var groupFilter *uuid.UUID
	if v := r.URL.Query().Get("group_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid group_id")
			return
		}
		groupFilter = &id
	}
	docIDs, err := d.resolveUserDocIDs(r.Context(), uid, groupFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "document list failed")
		return
	}
	if len(docIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"results": []any{}})
		return
	}

	results, err := d.Retriever.Retrieve(r.Context(), retriever.Options{
		Query:       q,
		NResults:    n,
		DocIDs:      docIDs,
		UseReranker: r.URL.Query().Get("rerank") != "false",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	if results == nil {
		results = []retriever.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// resolveUserDocIDs lists the doc_ids for the current user, optionally
// scoped to a group. Returns an empty slice (not nil) when the user
// has no documents — callers should then short-circuit.
func (d SearchDeps) resolveUserDocIDs(ctx context.Context, userID int32, groupID *uuid.UUID) ([]uuid.UUID, error) {
	if d.UserDocs == nil {
		return nil, nil
	}
	if groupID != nil {
		return d.UserDocs.DocIDsInGroup(ctx, userID, *groupID)
	}
	return d.UserDocs.DocIDs(ctx, userID)
}

// stringFromMetadata pulls a string field out of the OpenSearch
// `metadata` object. Falls back to empty string.
func stringFromMetadata(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// stringSliceFromMetadata returns a []string from a JSONB list field.
// Accepts either a native []any of strings or a comma-separated string
// (the Python backend has stored both shapes over time).
func stringSliceFromMetadata(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	switch s := v.(type) {
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	case []string:
		return s
	case string:
		if s == "" {
			return nil
		}
		return []string{s}
	}
	return nil
}

// intFromMetadata returns a *int from an integer-ish metadata field.
// JSON numbers round-trip as float64 through map[string]any.
func intFromMetadata(m map[string]any, key string) *int {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	switch n := v.(type) {
	case int:
		return &n
	case int32:
		v := int(n)
		return &v
	case int64:
		v := int(n)
		return &v
	case float64:
		v := int(n)
		return &v
	}
	return nil
}

// UserDocsRepoAdapter adapts repo.Documents into UserDocIDs. Lives in
// the api package so the repo layer doesn't need to know about the
// retriever.
type UserDocsRepoAdapter struct {
	DB DocumentIDLister
}

// DocumentIDLister is the tiny slice of the Documents repo the
// adapter needs.
type DocumentIDLister interface {
	IDsForUser(ctx context.Context, userID int32) ([]uuid.UUID, error)
	IDsForUserInGroup(ctx context.Context, userID int32, groupID uuid.UUID) ([]uuid.UUID, error)
}

// DocIDs forwards to the repo.
func (a UserDocsRepoAdapter) DocIDs(ctx context.Context, userID int32) ([]uuid.UUID, error) {
	return a.DB.IDsForUser(ctx, userID)
}

// DocIDsInGroup forwards the group-scoped query to the repo.
func (a UserDocsRepoAdapter) DocIDsInGroup(ctx context.Context, userID int32, groupID uuid.UUID) ([]uuid.UUID, error) {
	return a.DB.IDsForUserInGroup(ctx, userID, groupID)
}

// Compile-time sanity: make sure repo.Document's store fits the adapter.
var _ UserDocIDs = UserDocsRepoAdapter{}

// Documents repo extension: IDsForUser. Define it here as a tiny
// adapter over the existing List query so we don't need to touch
// every Documents struct.
type docIDLister struct{ d *repo.Documents }

// NewDocumentIDLister wraps a repo.Documents for use as the ID lister.
func NewDocumentIDLister(d *repo.Documents) DocumentIDLister { return docIDLister{d: d} }

// IDsForUser returns the UUIDs the user owns. Uses the raw ListSimple
// API with a generous limit so moderately-large libraries still fit.
func (l docIDLister) IDsForUser(ctx context.Context, userID int32) ([]uuid.UUID, error) {
	const cap = 2000
	docs, err := l.d.ListSimple(ctx, userID, 0, cap)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(docs))
	for _, doc := range docs {
		out = append(out, doc.ID)
	}
	return out, nil
}

// IDsForUserInGroup returns the UUIDs the user owns that ALSO belong
// to the given group. Uses the paginated List with an explicit group
// filter (parity with Python's intersection query in documents.py).
func (l docIDLister) IDsForUserInGroup(ctx context.Context, userID int32, groupID uuid.UUID) ([]uuid.UUID, error) {
	const cap = 2000
	page, err := l.d.List(ctx, userID, repo.DocumentListOptions{
		Page:    1,
		Limit:   cap,
		GroupID: &groupID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(page.Documents))
	for _, doc := range page.Documents {
		out = append(out, doc.ID)
	}
	return out, nil
}
