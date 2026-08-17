// A2 #166: selection API (GET/PUT /api/zotero/selection) and the client's
// sync-state listing (GET /api/zotero/documents?sync_state=…).
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// SelectionRepo is what the selection + documents routes need (repo.Repo).
type SelectionRepo interface {
	SetSelections(ctx context.Context, in []repo.SelectionInput) error
	SelectionModes(ctx context.Context) (map[string]string, error)
	ListZoteroDocuments(ctx context.Context, syncState string) ([]repo.ZoteroDocumentState, error)
}

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
	writeJSON(w, http.StatusOK, map[string]any{"selection": m})
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
		Selection []repo.SelectionInput `json:"selection"`
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
	if err := s.selectionRepo.SetSelections(r.Context(), body.Selection); err != nil {
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
