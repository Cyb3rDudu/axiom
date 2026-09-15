package server

// #259: first-class re-ingest after ARTIFACTS_EXPIRED. The endpoint enqueues
// a NEW force-rebuild job for the document's preferred attachment (repo.
// EnqueueForceRebuild — design a, decision documented in the issue): the
// claim path freezes a fresh snapshot and a fresh `…:force-<jobID>`
// idempotency key, so the runner's durable dedup never replays the old
// 409/ARTIFACTS_EXPIRED. Replaces the manual DB surgery recipe from the
// operator runbook.

import (
	"context"
	"errors"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/go-chi/chi/v5"
)

// ForceRebuildRepo is what the server needs to enqueue a force rebuild.
type ForceRebuildRepo interface {
	EnqueueForceRebuild(ctx context.Context, documentID string) (*repo.Job, error)
}

// SetForceRebuildAPI wires the force-rebuild enqueue (nil disables the route).
func (s *Server) SetForceRebuildAPI(r ForceRebuildRepo) { s.forceRebuildRepo = r }

// handleForceRebuild enqueues a force-rebuild job for one document.
func (s *Server) handleForceRebuild(w http.ResponseWriter, r *http.Request) {
	if s.forceRebuildRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "force rebuild not configured"})
		return
	}
	documentID := chi.URLParam(r, "documentID")
	if !isUUID(documentID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "document id must be a UUID"})
		return
	}
	job, err := s.forceRebuildRepo.EnqueueForceRebuild(r.Context(), documentID)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNoPreferredAttachment):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, repo.ErrRebuildInFlight):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "REBUILD_IN_FLIGHT"})
		case errors.Is(err, repo.ErrNoContentHash):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "code": "NO_CONTENT_HASH"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}
