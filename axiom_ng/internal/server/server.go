// Package server exposes the axiom-ng REST API. Clients talk only to this API
// and never directly to Zotero, Postgres or OpenSearch.
package server

import (
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/version"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
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
	// #197 standing consolidation write surface (nil = route unregistered,
	// the write-route gate like repairRepo).
	consolidateSvc ConsolidateService
	// sourceSecret enables /api/processor/source when non-empty (HMAC,
	// shared with the dispatcher). sourceRepo is the job lookup for it.
	sourceSecret string
	sourceRepo   processorSourceRepo
	// #184 fix-service surface (nil = endpoints stay unwired/404).
	// #168 (B2) live WebSocket surface (nil = /api/ws unwired/404).
	// #169 (B3) runner live view: the state deriver feeding the runners WS
	// topic snapshot and /api/runners/live (nil = REST route unwired/404).
	runnerLive     *RunnerLive
	ws             *wsServer
	repairRepo     *repo.Repo
	zoteroWrite    *zotero.WriteClient
	quarantineRoot string
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
	r.Get("/api/passage/{id}/page", s.handlePassageAt)
	r.Get("/api/kg/entities", s.handleKGEntities)
	r.Get("/api/kg/entities/{id}/neighbors", s.handleKGNeighbors)
	r.Get("/api/kg/relations", s.handleKGRelations)
	// #168 (B2): the live event WebSocket. Registered always; the handler
	// 404s when s.ws is nil (sourceSecret/repair-API pattern — unwired is
	// indistinguishable from absent).
	r.Get("/api/ws", s.handleWS)
	// #169 (B3): the runner live-view REST snapshot. Registered always;
	// 404s when no deriver is wired (same pattern).
	r.Get("/api/runners/live", s.handleRunnersLive)
	// #197: consolidation write route exists only when wired (repair-API
	// pattern — unwired answers 404, the admin gate alongside loopback bind).
	if s.consolidateSvc != nil {
		r.Post("/api/kg/consolidate", s.handleKGConsolidate)
	}
	// Disabled (404 on everything) until SetProcessorSourceSecret wires it.
	r.Get("/api/processor/source/{jobID}", s.handleProcessorSource)
	// #184: repair surface only exists when SetRepairAPI wired it.
	if s.repairRepo != nil {
		r.Get("/api/repair/queue", s.handleRepairQueue)
		r.Get("/api/repair/cases", s.handleRepairCases)
		r.Post("/api/repair/cases/{id}/claim", s.handleRepairClaim)
		r.Post("/api/repair/cases/{id}/verdict", s.handleRepairVerdict)
		r.Get("/api/repair/docs/{documentKey}/locator-stats", s.handleLocatorStats)
	}

	return r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// healthResponse is returned by /api/health.
type healthResponse struct {
	OK     bool           `json:"ok"`
	Build  string         `json:"build"` // version banner — must match `axiom-ng --version` (#205 DoD)
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

	writeJSON(w, http.StatusOK, healthResponse{OK: ok, Build: version.Banner(), Checks: checks})
}
