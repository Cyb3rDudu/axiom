package server

// #197: the standing entity-consolidation write surface. The merge itself
// is proven (#193, c1e0e82); this endpoint makes it reachable over REST.
// Admin gate follows the write-route pattern (repair API): the route is
// registered only when the service is wired — an unwired server answers
// 404, and the API's default loopback bind is the admin boundary (same
// posture as POST /api/zotero/sync). POST-only, no parameters: the merge
// is idempotent, a second call answers merged=0 / duplicate forms 0->0.

import (
	"context"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// ConsolidateService is the consolidation surface behind
// POST /api/kg/consolidate (implemented by repo.Repo).
type ConsolidateService interface {
	ConsolidateEntitiesReport(ctx context.Context) (repo.ConsolidationReport, error)
}

// SetConsolidateService wires the consolidation write route (nil keeps it
// unregistered — 404, the write-route gate).
func (s *Server) SetConsolidateService(svc ConsolidateService) { s.consolidateSvc = svc }

func (s *Server) handleKGConsolidate(w http.ResponseWriter, r *http.Request) {
	if s.consolidateSvc == nil {
		// Defensive: the route is only registered when wired (Handler()).
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "consolidate not configured"})
		return
	}
	rep, err := s.consolidateSvc.ConsolidateEntitiesReport(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.log != nil {
		s.log.Printf("kg: entity consolidation (#197): merged=%d duplicate_forms %d->%d",
			rep.Merged, rep.DuplicateFormsBefore, rep.DuplicateFormsAfter)
	}
	writeJSON(w, http.StatusOK, rep)
}
