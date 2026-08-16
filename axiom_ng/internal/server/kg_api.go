// Package server — knowledge-graph API (R6, #136). Read-only routes over
// the L6 graph data; the mention-stability filter (>=2) is the documented
// default, configurable per request via min_mentions.
package server

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/go-chi/chi/v5"
)

// KGService is the graph surface behind /api/kg/* (implemented by repo.Repo).
type KGService interface {
	SearchKGEntities(ctx context.Context, q string, minMentions, limit int) ([]repo.KGEntity, error)
	KGNeighbors(ctx context.Context, entityID string, minMentions, limit int) ([]repo.KGNeighbor, error)
	KGRelations(ctx context.Context, relType, entityID string, minMentions, limit int) ([]repo.KGRelationView, error)
}

// SetKGService wires the /api/kg routes (nil keeps them 503ing).
func (s *Server) SetKGService(svc KGService) { s.kgSvc = svc }

// minMentionsDefault is the L8-§6 stability floor: entities below 2 distinct
// chunks are 71% one-hit noise. Configurable per request, never below 1.
const minMentionsDefault = 2

func kgQueryInt(r *http.Request, key string, def, min, max int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (s *Server) handleKGEntities(w http.ResponseWriter, r *http.Request) {
	if s.kgSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "kg not configured"})
		return
	}
	res, err := s.kgSvc.SearchKGEntities(r.Context(),
		r.URL.Query().Get("q"),
		kgQueryInt(r, "min_mentions", minMentionsDefault, 1, 100),
		kgQueryInt(r, "limit", 50, 1, 200))
	writeKGResult(w, res, err)
}

func (s *Server) handleKGNeighbors(w http.ResponseWriter, r *http.Request) {
	if s.kgSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "kg not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	if !isUUID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity id must be a uuid"})
		return
	}
	res, err := s.kgSvc.KGNeighbors(r.Context(), id,
		kgQueryInt(r, "min_mentions", minMentionsDefault, 1, 100),
		kgQueryInt(r, "limit", 50, 1, 200))
	writeKGResult(w, res, err)
}

func (s *Server) handleKGRelations(w http.ResponseWriter, r *http.Request) {
	if s.kgSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "kg not configured"})
		return
	}
	entity := r.URL.Query().Get("entity")
	if entity != "" && !isUUID(entity) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity filter must be a uuid"})
		return
	}
	res, err := s.kgSvc.KGRelations(r.Context(),
		r.URL.Query().Get("type"),
		entity,
		kgQueryInt(r, "min_mentions", minMentionsDefault, 1, 100),
		kgQueryInt(r, "limit", 50, 1, 200))
	writeKGResult(w, res, err)
}

func writeKGResult(w http.ResponseWriter, res any, err error) {
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
