package server

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
)

// SyncService is what the server needs to trigger a Zotero sync.
type SyncService interface {
	Run(ctx context.Context) (sync.Result, error)
}

// JobRepo is what the server needs to list ingest jobs.
type JobRepo interface {
	ListJobs(ctx context.Context, limit int) ([]repo.Job, error)
}

// SetSyncAPI wires the Zotero sync trigger handler (nil disables the route).
func (s *Server) SetSyncAPI(svc SyncService) { s.jobsSvc = svc }

// SetJobRepo wires the ingest-job listing (nil disables the route).
func (s *Server) SetJobRepo(r JobRepo) { s.repo = r }

// handleSync triggers a Zotero sync and returns the result summary.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if s.jobsSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sync not configured"})
		return
	}
	res, err := s.jobsSvc.Run(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleJobs returns the most recent ingest jobs.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.repo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "jobs not configured"})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	jobs, err := s.repo.ListJobs(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}
