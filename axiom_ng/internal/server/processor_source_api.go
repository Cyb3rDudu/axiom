package server

// /api/processor/source/{job_id} — HMAC-signed source download for remote
// processors (contract §3 remote transport). The dispatcher signs
// jobID|leaseUnix with AXIOMNG_PROCESSOR_SOURCE_SECRET; this endpoint
// verifies signature, expiry, job status (claimed/processing) and lease
// freshness before streaming the attachment bytes in place (read-only;
// Zotero stays the source of truth). Every failure is a 404 — the endpoint
// must not act as an existence oracle for jobs, secrets or files.

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sourceurl"
	"github.com/go-chi/chi/v5"
)

// processorSourceRepo is the lookup the endpoint needs (implemented by
// *repo.Repo.ProcessorSource).
type processorSourceRepo interface {
	ProcessorSource(ctx context.Context, jobID string) (repo.ProcessorSource, error)
}

// SetProcessorSourceSecret enables the route when non-empty (shared HMAC
// secret with the dispatcher). Empty = endpoint disabled (404 on everything).
func (s *Server) SetProcessorSourceSecret(secret string) { s.sourceSecret = secret }

// SetProcessorSourceRepo wires the job lookup (nil keeps the route 404ing).
func (s *Server) SetProcessorSourceRepo(r processorSourceRepo) { s.sourceRepo = r }

// handleProcessorSource streams a claimed job's source file.
func (s *Server) handleProcessorSource(w http.ResponseWriter, r *http.Request) {
	if s.sourceSecret == "" || s.sourceRepo == nil {
		http.NotFound(w, r)
		return
	}
	jobID := chi.URLParam(r, "jobID")

	// Signature + expiry first (cheap, no DB): wrong or stale => 404.
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil || time.Now().After(time.Unix(exp, 0)) {
		http.NotFound(w, r)
		return
	}
	if !sourceurl.Verify(s.sourceSecret, jobID, exp, r.URL.Query().Get("sig")) {
		http.NotFound(w, r)
		return
	}

	src, err := s.sourceRepo.ProcessorSource(r.Context(), jobID)
	if err != nil || src.LocalPath == "" {
		// Unknown job (or lookup failure) is indistinguishable from missing.
		http.NotFound(w, r)
		return
	}
	if src.Status != "claimed" && src.Status != "processing" {
		http.NotFound(w, r)
		return
	}
	if time.Now().After(src.LeaseUntil) {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(src.LocalPath) // read in place, no staging copy
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	if src.ContentType != "" {
		w.Header().Set("Content-Type", src.ContentType)
	}
	// ServeContent handles Range/If-Modified-Since and sets Content-Length.
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
