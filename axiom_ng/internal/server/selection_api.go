// A2 #166: selection API (GET/PUT /api/zotero/selection) and the client's
// sync-state listing (GET /api/zotero/documents?sync_state=…).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// SelectionRepo is what the selection + documents routes need (repo.Repo).
type SelectionRepo interface {
	SetSelectionBatch(ctx context.Context, docs []repo.SelectionInput, colls []repo.CollectionSelectionInput) error
	SelectionModes(ctx context.Context) (map[string]string, error)
	CollectionSelectionModes(ctx context.Context) (map[string]string, error)
	ResolveSelectionView(ctx context.Context) (*repo.ResolvedSelection, error)
	ListZoteroDocuments(ctx context.Context, syncState string) ([]repo.ZoteroDocumentState, error)
}

// collectionKeyPattern: Zotero collection keys are 8-char alphanumerics.
var collectionKeyRe = regexp.MustCompile(`^[A-Z0-9]{8}$`)

// SetSelectionRepo wires the selection routes (nil keeps them 503ing).
func (s *Server) SetSelectionRepo(r SelectionRepo) { s.selectionRepo = r }

// handleGetSelection returns the persisted selection map.
func (s *Server) handleGetSelection(w http.ResponseWriter, r *http.Request) {
	if s.selectionRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "selection not configured"})
		return
	}
	m, err := s.selectionRepo.SelectionModes(r.Context())
	if err != nil {
		s.log.Printf("selection get: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "selection unavailable"})
		return
	}
	colls, err := s.selectionRepo.CollectionSelectionModes(r.Context())
	if err != nil {
		s.log.Printf("selection get: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "selection unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"selection": m, "collections": colls})
}

// handlePutSelection applies a selection batch: entries
// {document_id, mode}; mode included/excluded upserts, "default" resets to
// the no-row default. Validated here (trust boundary): ids must be UUIDs,
// modes one of the three.
func (s *Server) handlePutSelection(w http.ResponseWriter, r *http.Request) {
	if s.selectionRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "selection not configured"})
		return
	}
	var body struct {
		Selection   []repo.SelectionInput           `json:"selection"`
		Collections []repo.CollectionSelectionInput `json:"collections"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid selection body"})
		return
	}
	for _, e := range body.Selection {
		if !isUUID(e.DocumentID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "document_id must be a UUID"})
			return
		}
		switch e.Mode {
		case "included", "excluded", "default", "":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be included|excluded|default"})
			return
		}
	}
	for _, e := range body.Collections {
		if !collectionKeyRe.MatchString(e.CollectionKey) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection_key must be a Zotero key (8 alphanumerics)"})
			return
		}
		switch e.Mode {
		case "included", "excluded", "default", "":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be included|excluded|default"})
			return
		}
	}
	// BOTH layers in one transaction (#166): a half-applied selection would
	// silently flip sync semantics for the other layer. (The wiring-gap
	// lesson: this handler once validated the collections and dropped them —
	// caught by Hivemind's live probe, not by the repo-direct ITs.)
	if err := s.selectionRepo.SetSelectionBatch(r.Context(), body.Selection, body.Collections); err != nil {
		var fk *pgconn.PgError
		if errors.As(err, &fk) && fk.Code == "23503" {
			// A selection entry names a document that does not exist — the
			// client's mistake, not a server fault.
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unknown document id in selection"})
			return
		}
		s.log.Printf("selection put: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "selection write failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleZoteroDocuments is the client's sync-state listing (#166 Ziel 4).
func (s *Server) handleZoteroDocuments(w http.ResponseWriter, r *http.Request) {
	if s.selectionRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "documents not configured"})
		return
	}
	state := r.URL.Query().Get("sync_state")
	switch state {
	case "", "synced", "held", "processing", "pending":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sync_state must be synced|held|processing|pending"})
		return
	}
	docs, err := s.selectionRepo.ListZoteroDocuments(r.Context(), state)
	if err != nil {
		s.log.Printf("documents list: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "documents unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// handleSelectionResolved shows the client WHAT it chose: per selected
// collection the resolved document ids, plus the document rows
// (#166 NACHSCHÄRFUNG Ziel 3).
func (s *Server) handleSelectionResolved(w http.ResponseWriter, r *http.Request) {
	if s.selectionRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "selection not configured"})
		return
	}
	v, err := s.selectionRepo.ResolveSelectionView(r.Context())
	if err != nil {
		s.log.Printf("selection resolved: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "selection unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, v)
}
