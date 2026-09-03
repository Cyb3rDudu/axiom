// /api/passage/{chunk_id} — the A1 context primitive (#165). Degradation
// pattern as everywhere: search not configured → 503; unknown chunk → 404;
// inactive-snapshot chunk → 404 WITH hint payload.
//
// #194 exact-page citations: ?at=<char offset> (optional) derives the exact
// print page of a hit position from the chunk's per-paragraph page map and
// adds {"page_at": {"label": ...}} to the response. Without a map (pre-#194
// generations) the field is absent and the span remains the honest envelope.
package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

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

// handlePassageAt — GET /api/passage/{id}?at=N: exact print page of the
// character position N within the chunk text (#194).
func (s *Server) handlePassageAt(w http.ResponseWriter, r *http.Request) {
	if s.passageSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "passage not configured"})
		return
	}
	atRaw := r.URL.Query().Get("at")
	at, err := strconv.Atoi(atRaw)
	if err != nil || at < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "?at must be a non-negative char offset"})
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" || !isUUID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "chunk id must be a UUID"})
		return
	}
	p, err := s.passageSvc.GetPassage(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chunk not found or unavailable"})
		return
	}
	if p.Locator.Kind == "epub_cfi" {
		// #245: EPUB citations are ALWAYS APA 7 sections — page data is
		// stored but dormant and never served. Honest 404 beats a page
		// number the citation contract retired.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "EPUB passages cite APA 7 sections (chapter/section/paragraph), never pages",
		})
		return
	}
	if len(p.ParagraphPages) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "chunk has no paragraph page map (pre-#194 generation); the span is the envelope",
			"span":  p.Locator,
		})
		return
	}
	label := p.ParagraphPages[0][1]
	off := 0
	for _, b := range p.ParagraphPages {
		if v, err := strconv.Atoi(b[0]); err == nil && v <= at {
			off = v
			label = b[1]
		}
	}
	_ = off
	writeJSON(w, http.StatusOK, map[string]any{"chunk_id": id, "at": at, "page": label})
}
