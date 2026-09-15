package server

// /api/processor/source/{job_id} — HMAC-signed source download for remote
// processors (contract §3 remote transport). The dispatcher signs
// jobID|leaseUnix with AXIOM_PROCESSOR_SOURCE_SECRET; this endpoint
// verifies signature, job status (claimed/processing) and lease
// freshness before streaming the attachment bytes in place (read-only;
// Zotero stays the source of truth). Every failure is a 404 — the endpoint
// must not act as an existence oracle for jobs, secrets or files. The
// rejection REASON is logged internally (#271 P1): operators get the branch
// without an external existence oracle.
//
// #264 freshness model: the signed exp is an AUTHENTICITY input (covered by
// the HMAC), not the freshness clock. Freshness is the DB lease fence below —
// renewal (running from claim since #264) keeps that lease alive while the
// runner works, so a runner whose single-lane queue wait exceeds one lease
// window still downloads successfully. The pre-#264 wall-clock exp rejection
// killed healthy jobs whose only fault was queueing behind model warmup.
//
// #271 P0 clock-domain fix: the freshness predicate is evaluated inside the
// SQL lookup against the DB clock and arrives as a boolean. The handler never
// compares the host wall clock against the DB lease_until — a host/VM clock step
// (sleep, NTP jump) can no longer 404 a healthy download.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sourceurl"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
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

// SetProcessorSourceStatFn overrides the file-stat call (test seam for the
// stat_failed 404 branch, #273; nil = the real (*os.File).Stat).
func (s *Server) SetProcessorSourceStatFn(f func(*os.File) (os.FileInfo, error)) {
	s.sourceStatFn = f
}

// statFile runs the (injectable) stat used for the regular-file check and
// ServeContent timestamps.
func (s *Server) statFile(f *os.File) (os.FileInfo, error) {
	if s.sourceStatFn != nil {
		return s.sourceStatFn(f)
	}
	return f.Stat()
}

// sourceReject logs the internal 404 branch while keeping the wire response a
// uniform 404 (no existence oracle). #271 P1: the pre-fix endpoint masked
// nine distinct causes as one identical 404 — the clock-domain outage was
// therefore misclassified as retryable SOURCE_URL_STALE for two hours.
func (s *Server) sourceReject(w http.ResponseWriter, r *http.Request, jobID, reason string) {
	if s.log != nil {
		s.log.Printf("processor source 404: job=%s reason=%s", jobID, reason)
	}
	http.NotFound(w, r)
}

// handleProcessorSource streams a claimed job's source file.
func (s *Server) handleProcessorSource(w http.ResponseWriter, r *http.Request) {
	if s.sourceSecret == "" || s.sourceRepo == nil {
		s.sourceReject(w, r, "", "disabled_no_secret")
		return
	}
	jobID := chi.URLParam(r, "jobID")

	// Signature first (cheap, no DB): wrong => 404. The exp value is part of
	// the signed material; freshness is enforced by the DB lease fence below
	// (#264 — see the freshness note above).
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil {
		s.sourceReject(w, r, jobID, "bad_exp")
		return
	}
	if !sourceurl.Verify(s.sourceSecret, jobID, exp, r.URL.Query().Get("sig")) {
		s.sourceReject(w, r, jobID, "bad_signature")
		return
	}

	src, err := s.sourceRepo.ProcessorSource(r.Context(), jobID)
	if err != nil {
		// Unknown job (no rows) is the common case; any other lookup failure
		// is a backing-store fault. Both stay 404 externally, but the branch
		// must be readable internally.
		if errors.Is(err, pgx.ErrNoRows) {
			s.sourceReject(w, r, jobID, "unknown_job")
		} else {
			s.sourceReject(w, r, jobID, "lookup_error")
		}
		return
	}
	if src.LocalPath == "" {
		s.sourceReject(w, r, jobID, "empty_path")
		return
	}
	if src.Status != "claimed" && src.Status != "processing" {
		s.sourceReject(w, r, jobID, "status_"+src.Status)
		return
	}
	// Freshness was decided by the DB (`lease_until > now()`); no host clock
	// comparison here (#271 P0).
	if !src.LeaseFresh {
		s.sourceReject(w, r, jobID, "lease_expired")
		return
	}

	f, err := os.Open(src.LocalPath) // read in place, no staging copy
	if err != nil {
		s.sourceReject(w, r, jobID, "open_failed")
		return
	}
	defer f.Close()
	fi, err := s.statFile(f)
	if err != nil {
		s.sourceReject(w, r, jobID, "stat_failed")
		return
	}
	if fi.IsDir() {
		s.sourceReject(w, r, jobID, "not_regular")
		return
	}
	if src.ContentType != "" {
		w.Header().Set("Content-Type", src.ContentType)
	}
	// ServeContent handles Range/If-Modified-Since and sets Content-Length.
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
