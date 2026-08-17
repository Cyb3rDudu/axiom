package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
)

// SyncService is what the server needs to trigger a Zotero sync.
// SyncOverrideBody is the optional POST /api/zotero/sync body (#166):
// one-run include/exclude document lists on top of the persisted selection.
type SyncOverrideBody struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type SyncService interface {
	Run(ctx context.Context, override *sync.SyncOverride) (sync.Result, error)
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
	// Optional one-run selection override (#166). An empty or absent body is
	// a plain sync (persisted selection only). UUID-validated: a typo'd id
	// would silently gate nothing.
	var body SyncOverrideBody
	if r.Body != nil {
		raw, rerr := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if rerr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sync override body too large or unreadable"})
			return
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid sync override body"})
				return
			}
			for _, id := range body.Include {
				if !isUUID(id) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "override ids must be document UUIDs"})
					return
				}
			}
			for _, id := range body.Exclude {
				if !isUUID(id) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "override ids must be document UUIDs"})
					return
				}
			}
		}
	}
	var override *sync.SyncOverride
	if len(body.Include) > 0 || len(body.Exclude) > 0 {
		override = &sync.SyncOverride{Include: body.Include, Exclude: body.Exclude}
	}
	res, err := s.jobsSvc.Run(r.Context(), override)
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
