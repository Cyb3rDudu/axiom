// /api/passage/{chunk_id} — the A1 context primitive (#165). Degradation
// pattern as everywhere: search not configured → 503; unknown chunk → 404;
// inactive-snapshot chunk → 404 WITH hint payload.
package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
	"github.com/go-chi/chi/v5"
)

// PassageService is the passage surface (implemented by search.Service).
type PassageService interface {
	GetPassage(ctx context.Context, chunkID string) (*search.Passage, error)
}

// SetPassageService wires the passage handler (nil keeps the route 503ing).
func (s *Server) SetPassageService(svc PassageService) { s.passageSvc = svc }

func (s *Server) handlePassage(w http.ResponseWriter, r *http.Request) {
	if s.passageSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "passage not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" || !isUUID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "chunk id must be a UUID"})
		return
	}
	p, err := s.passageSvc.GetPassage(r.Context(), id)
	if err != nil {
		var inactive *search.InactiveSnapshotError
		switch {
		case errors.As(err, &inactive):
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error":      inactive.Error(),
				"hint":       "chunk belongs to an inactive (superseded) snapshot; current chunks are reachable via /api/search",
				"chunk_id":   inactive.ChunkID,
				"snapshot":   inactive.SnapshotID,
				"attachment": inactive.AttachmentID,
			})
		case errors.Is(err, search.ErrPassageNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "chunk not found"})
		default:
			s.log.Printf("passage: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "passage unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, p)
}
