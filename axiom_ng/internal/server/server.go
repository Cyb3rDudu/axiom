// Package server exposes the axiom-ng REST API. Clients talk only to this API
// and never directly to Zotero, Postgres or OpenSearch.
package server

import (
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/deprecate"
	axlibrary "github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/version"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"

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
	// sourceStatFn overrides (*os.File).Stat for tests (#273): nil = real stat.
	sourceSecret string
	sourceRepo   processorSourceRepo
	sourceStatFn func(*os.File) (os.FileInfo, error)
	// #259 force-rebuild enqueue surface (nil = route unwired/503).
	forceRebuildRepo ForceRebuildRepo
	// #184 fix-service surface (nil = endpoints stay unwired/404).
	// #168 (B2) live WebSocket surface (nil = /api/ws unwired/404).
	// #169 (B3) runner live view: the state deriver feeding the runners WS
	// topic snapshot and /api/runners/live (nil = REST route unwired/404).
	runnerLive     *events.RunnerLive
	ws             *wsServer
	repairRepo     *repo.Repo
	zoteroWrite    *zoteroprovider.WriteClient
	quarantineRoot string
	// #262 contextual health state: returns "active",
	// "degraded_no_sync" or "" (no rules configured). nil = omitted.
	contextualState func() string
	// F05 #299 aggregated component readiness: role → "ready" | "starting"
	// | "stopping". Wired by the composition root (the one place that
	// knows the selected role set); nil = omitted (bare-server shapes,
	// the F01 baseline scaffolding wires its own).
	readinessState func() map[string]string
	// componentRoles: the composition's selected role set (empty keeps the
	// legacy full-stack vocabulary - pre-F04 callers).
	componentRoles []string
	// #298: the live /api/ws CONNECTIONS plus the one-way draining flag,
	// for CloseLiveWebSockets during the composition root's ordered shutdown.
	wsLive *wsLiveConns
	// F06 #300: the Library import surface (nil = routes answer 404 —
	// the sourceSecret pattern; the composition root wires the service,
	// fake providers until F07 ports Zotero).
	librarySvc            libraryAPI
	storeAPI              StoreAPI
	libraryImportMaxBytes int64
	// F06 #300: the source-revision Mits-Schrieb publisher for the heal/
	// custody points (nil = hooks skip).
	revisionPublisher axlibrary.RevisionPublisher
}

// SetRevisionPublisher wires the F06 (#300) revision Mits-Schrieb for the
// repair/custody points (the syncer gets its own SetRevisionSink).
func (s *Server) SetRevisionPublisher(p axlibrary.RevisionPublisher) { s.revisionPublisher = p }

// New builds a Server with no backing-dependency checkers yet. Register them
// via RegisterCheck so /api/health reports their reachability.
func New(addr string, log *log.Logger) *Server {
	return &Server{addr: addr, checkers: map[string]Checker{}, log: log, wsLive: newWSLiveConns()}
}

// RegisterCheck adds a named dependency checker reported by /api/health.
func (s *Server) RegisterCheck(name string, c Checker) { s.checkers[name] = c }

// SetContextualState wires the #262 /api/health contextual field: "active"
// or "degraded_no_sync" while rules are configured (omitted otherwise) — a
// permanently degraded deployment must be observable, not silent.
func (s *Server) SetContextualState(f func() string) { s.contextualState = f }

// serviceClassOf maps selected composition roles onto the ADR-0001
// component vocabulary: api (always), library (sync = the Zotero/Library
// runtime), store (any Store-owned role). The full stack therefore stays
// "api+library+store"; a store slice reports "api+store".
func serviceClassOf(roles []string) string {
	if len(roles) == 0 {
		return "api+library+store" // legacy full-stack default (pre-F04 callers)
	}
	has := func(sub string) bool {
		for _, r := range roles {
			if strings.Contains(string(r), sub) {
				return true
			}
		}
		return false
	}
	out := []string{"api"}
	if has("sync") {
		out = append(out, "library")
	}
	if has("store") || has("search") || has("ingest") || has("dispatcher") || has("events") {
		out = append(out, "store")
	}
	return strings.Join(out, "+")
}

// componentRolesOrDefault keeps the frozen legacy shape when no
// composition wired a role set.
func componentRolesOrDefault(roles []string) []string {
	if len(roles) == 0 {
		return []string{"api", "library", "store"}
	}
	return roles
}

// SetComponentRoles wires the composition's selected role set (the
// health surface derives service_class from it; empty keeps the legacy
// full-stack vocabulary).
func (s *Server) SetComponentRoles(roles []string) { s.componentRoles = roles }

// SetReadinessState wires the F05 /api/health readiness field: the
// aggregated per-role component readiness of THIS process (composition
// root is the caller; values "ready" | "starting" | "stopping").
func (s *Server) SetReadinessState(f func() map[string]string) { s.readinessState = f }

// Handler returns the chi router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/api/health", s.handleHealth)
	// Canonical /api/v1 namespace (F05, #299): the same handlers as the
	// compat routes — byte-identical responses by construction (one
	// handler, two registrations; the parity test witnesses it). Start
	// set: health, search, passage; further routes arrive with the
	// component extraction (F06–F10). The unversioned routes above stay
	// untouched through 0.2.x (F01 baseline guards them byte-for-byte).
	r.Get("/api/v1/health", s.handleHealth)
	r.Post("/api/zotero/sync", s.handleSync)
	r.Get("/api/ingest/jobs", s.handleJobs)
	r.Post("/api/ingest/documents/{documentID}/force-rebuild", s.handleForceRebuild)
	r.Get("/api/zotero/selection", s.handleGetSelection)
	r.Get("/api/zotero/selection/resolved", s.handleSelectionResolved)
	r.Put("/api/zotero/selection", s.handlePutSelection)
	r.Get("/api/zotero/documents", s.handleZoteroDocuments)
	r.Post("/api/search", s.handleSearch)
	r.Post("/api/v1/search", s.handleSearch)
	r.Get("/api/passage/{id}", s.handlePassage)
	r.Get("/api/v1/passage/{id}", s.handlePassage)
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
	// F06 #300: the public import contract (normative in issue #300).
	// Registered always; unwired answers 404 (sourceSecret pattern).
	r.Post("/api/v1/library/imports", s.handleLibraryImport)
	r.Post("/api/v1/store/ingest", s.handleStoreIngest)
	r.Get("/api/v1/library/imports/{id}", s.handleLibraryImportStatus)
	r.Post("/api/v1/library/imports/{id}/confirm", s.handleLibraryImportConfirm)
	r.Post("/api/v1/library/imports/{id}/retry", s.handleLibraryImportRetry)
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
		r.Post("/api/repair/cases/{id}/requeue", s.handleRepairRequeue)
		r.Post("/api/repair/cases/{id}/verdict", s.handleRepairVerdict)
		r.Post("/api/repair/custody", s.handleRepairCustody)
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
	// #262: "active" | "degraded_no_sync" (omitted when no contextual
	// rules are configured).
	Contextual string `json:"contextual,omitempty"`
	// Canonical identity (ADR 0001 §5, #296) — additive fields naming the
	// target identity of this process. All three component roles are
	// compiled in today; real role resolution per process arrives with
	// F04/F05 (`axiom serve all|api|library|store`). Froze in the baseline
	// via fixtures/canonical_identity.json.
	CanonicalName  string   `json:"canonical_name"`  // "axiom"
	ServiceClass   string   `json:"service_class"`   // compact role identity; narrows with F04/F05
	ComponentRoles []string `json:"component_roles"` // ADR 0001 component roles served
	// Legacy-alias usage counters (ADR 0001 §6, #296): warn-once/count-
	// always witness, empty map until a legacy name is used. Data basis
	// for the 0.3.x+ removal decision.
	Deprecations map[string]int `json:"deprecations"`
	// F05 #299: aggregated component readiness (role → "ready" |
	// "starting" | "stopping"), wired by the composition root. Omitted
	// when no provider is wired (bare server shapes). Taken up in the
	// working-tree identity fixture via BASELINE_UPDATE (F05).
	Readiness map[string]string `json:"readiness,omitempty"`
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

	// ADR 0001 §5: the compact service_class derives from the SELECTED
	// roles (F09 #303 made the store slice real — a store-only process
	// must not claim the library). The full stack maps to exactly the
	// frozen "api+library+store" (F01 goldens stay byte-identical);
	// componentRoles carries the raw role names.
	class := serviceClassOf(s.componentRoles)
	hr := healthResponse{
		OK:             ok,
		Build:          version.Banner(),
		Checks:         checks,
		CanonicalName:  "axiom",
		ServiceClass:   class,
		ComponentRoles: componentRolesOrDefault(s.componentRoles),
		Deprecations:   deprecate.Counts(),
	}
	if s.contextualState != nil {
		hr.Contextual = s.contextualState()
	}
	if s.readinessState != nil {
		hr.Readiness = s.readinessState()
	}
	writeJSON(w, http.StatusOK, hr)
}
