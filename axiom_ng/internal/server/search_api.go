package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

// SearchService is the retrieval surface behind POST /api/search (R3 #133).
type SearchService interface {
	Search(ctx context.Context, req search.Request) (*search.Response, error)
}

// SetSearchService wires the search handler (nil keeps the route answering
// 503, same degraded pattern as sync/jobs).
func (s *Server) SetSearchService(svc SearchService) { s.searchSvc = svc }

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.searchSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search not configured"})
		return
	}
	var req search.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	resp, err := s.searchSvc.Search(r.Context(), req)
	if err != nil {
		var bad search.ErrBadRequest
		if errors.As(err, &bad) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad.Error()})
			return
		}
		s.log.Printf("search: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
