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
	KGRelations(ctx context.Context, relType, entityID, documentID string, minMentions, limit int) ([]repo.KGRelationView, error)
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
	if err != nil {
		// A malformed value falls back to the DEFAULT — never the floor,
		// which would silently weaken the stability filter below it
		// (min_mentions=abc must not read as min_mentions=1).
		return def
	}
	if n < min {
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
	s.writeKG(w, r, "entities", res, err)
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
	s.writeKG(w, r, "neighbors", res, err)
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
	// document_id (#185): optional scope — only relations with evidence in
	// this document's active snapshot. Same validation strictness as entity.
	document := r.URL.Query().Get("document_id")
	if document != "" && !isUUID(document) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "document_id filter must be a uuid"})
		return
	}
	res, err := s.kgSvc.KGRelations(r.Context(),
		r.URL.Query().Get("type"),
		entity,
		document,
		kgQueryInt(r, "min_mentions", minMentionsDefault, 1, 100),
		kgQueryInt(r, "limit", 50, 1, 200))
	s.writeKG(w, r, "relations", res, err)
}

// KGSourceHydrator is the optional search-service capability that enriches
// KG evidence with unified SourceView blocks (A1 #165): a top-level
// "sources" map keyed by evidence chunk id. Absent capability (nil or
// unwired search service) keeps KG responses as they were.
type KGSourceHydrator interface {
	KGSources(ctx context.Context, chunkIDs []string) (map[string]repo.SourceView, error)
}

func writeKGResult(w http.ResponseWriter, res any, err error) {
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// kgSources collects evidence chunk ids from the KG result and hydrates
// them via the search service. The second return says whether the result
// shape is envelope-eligible AT ALL (neighbors/relations carry evidence ids;
// entities don't) — the envelope decision is per-configuration, so an empty
// or failed hydration still yields {"sources":{}}, keeping the top-level
// shape deterministic instead of flapping with request health.
func (s *Server) kgSources(r *http.Request, res any) (map[string]repo.SourceView, bool) {
	h, ok := s.searchSvc.(KGSourceHydrator)
	if !ok || h == nil {
		return nil, false
	}
	var ids []string
	switch v := res.(type) {
	case []repo.KGNeighbor:
		for _, e := range v {
			ids = append(ids, e.EvidenceChunks...)
		}
	case []repo.KGRelationView:
		for _, e := range v {
			ids = append(ids, e.EvidenceChunks...)
		}
	default:
		return nil, false
	}
	src, err := h.KGSources(r.Context(), ids)
	if err != nil {
		s.log.Printf("kg: source hydration failed (serving without): %v", err)
		src = map[string]repo.SourceView{} // degrade to empty envelope, not a shape flip
	}
	return src, true
}

// writeKG writes a KG result. When the search service provides hydration and
// the shape carries evidence ids (neighbors/relations), the response is
// ALWAYS the envelope {"<kind>": <raw list>, "sources": {chunk_id:
// SourceView}} — even when sources is empty, so the top-level shape is a
// property of the configuration, not of request health. Without a hydrator
// the response stays the bare raw list.
func (s *Server) writeKG(w http.ResponseWriter, r *http.Request, kind string, res any, err error) {
	if err != nil {
		writeKGResult(w, res, err)
		return
	}
	if src, envelope := s.kgSources(r, res); envelope {
		if src == nil {
			src = map[string]repo.SourceView{}
		}
		writeJSON(w, http.StatusOK, map[string]any{kind: res, "sources": src})
		return
	}
	writeKGResult(w, res, err)
}
