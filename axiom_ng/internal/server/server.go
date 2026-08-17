// Package server exposes the axiom-ng REST API. Clients talk only to this API
// and never directly to Zotero, Postgres or OpenSearch.
package server

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Checker reports reachability of a backing dependency (e.g. Zotero, Postgres).
type Checker interface {
	// Ready returns nil if the dependency is healthy, else an error describing
	// why it is not.
	Ready() error
}

// Server is the axiom-ng HTTP API.
type Server struct {
	addr          string
	checkers      map[string]Checker
	jobsSvc       SyncService
	repo          JobRepo
	log           *log.Logger
	searchSvc     SearchService
	passageSvc    PassageService
	kgSvc         KGService
	selectionRepo SelectionRepo
	// sourceSecret enables /api/processor/source when non-empty (HMAC,
	// shared with the dispatcher). sourceRepo is the job lookup for it.
	sourceSecret string
	sourceRepo   processorSourceRepo
}

// New builds a Server with no backing-dependency checkers yet. Register them
// via RegisterCheck so /api/health reports their reachability.
func New(addr string, log *log.Logger) *Server {
	return &Server{addr: addr, checkers: map[string]Checker{}, log: log}
}

// RegisterCheck adds a named dependency checker reported by /api/health.
func (s *Server) RegisterCheck(name string, c Checker) { s.checkers[name] = c }

// Handler returns the chi router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/api/health", s.handleHealth)
	r.Post("/api/zotero/sync", s.handleSync)
	r.Get("/api/ingest/jobs", s.handleJobs)
	r.Get("/api/zotero/selection", s.handleGetSelection)
	r.Get("/api/zotero/selection/resolved", s.handleSelectionResolved)
	r.Put("/api/zotero/selection", s.handlePutSelection)
	r.Get("/api/zotero/documents", s.handleZoteroDocuments)
	r.Post("/api/search", s.handleSearch)
	r.Get("/api/passage/{id}", s.handlePassage)
	r.Get("/api/kg/entities", s.handleKGEntities)
	r.Get("/api/kg/entities/{id}/neighbors", s.handleKGNeighbors)
	r.Get("/api/kg/relations", s.handleKGRelations)
	// Disabled (404 on everything) until SetProcessorSourceSecret wires it.
	r.Get("/api/processor/source/{jobID}", s.handleProcessorSource)

	return r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// healthResponse is returned by /api/health.
type healthResponse struct {
	OK     bool           `json:"ok"`
	Checks map[string]any `json:"checks"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	checks := map[string]any{}
	ok := true

	for name, checker := range s.checkers {
		if checker == nil {
			checks[name] = "unknown"
			ok = false
			continue
		}
		if err := checker.Ready(); err != nil {
			checks[name] = err.Error()
			ok = false
			continue
		}
		checks[name] = "ok"
	}

	writeJSON(w, http.StatusOK, healthResponse{OK: ok, Checks: checks})
}
