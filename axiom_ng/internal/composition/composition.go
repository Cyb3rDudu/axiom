// Package composition is the axiom sidecar's Composition Root (#298, F04).
//
// Exactly ONE place decides which components are active, in which order
// they start / go ready / shut down, and how their ports are bound. The
// axiom-ng entrypoint became a thin caller of this package; F05's role CLI
// (`axiom serve api|library|store`) sits on top of Select, and F06/F09
// extract the real component implementations behind these seams.
//
// Design invariants (issue #298 design comment carries the full rationale):
//
//   - Baseline protection: F04 rewires, it does not renegotiate. /api/health
//     and every route stay byte-identical — readiness is an INTERNAL signal
//     per component, aggregated only inside this package. Public health
//     aggregation is F05 scope, landed via a deliberate BASELINE_UPDATE.
//   - Ordered shutdown honors in-flight leases: HTTP stops accepting first
//     (WS connections closed explicitly — hijacked sockets are invisible to
//     http.Server.Shutdown), then the root context cancels and the
//     dispatcher / fixer-invoker drains are JOINED before the database
//     pool closes. A drain that outruns the stop budget is abandoned the
//     way the #271/#264 shutdown guard semantics prescribe: leases expire
//     and the claim scans' expired-recovery owns the rows — never lost,
//     never terminalized by the shutdown itself.
//   - Env reads live in the config packages and HERE (the one os.Getenv
//     Erlaubnisraum, enforced by the usage lint in this package's tests);
//     domain packages stay env-free.
package composition

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/dispatcher"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/fixerinvoker"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/server"
	axsync "github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

// Role names one lifecycle unit the composition can activate. The names are
// the F05 role-CLI vocabulary; today's axiom-ng derives them from config
// (RolesFromConfig) so the full stack starts exactly as before.
type Role string

const (
	// RoleAPI is the HTTP API server (chi routes, health, listener). It is
	// the base role — today's binary always serves it.
	RoleAPI Role = "api"
	// RoleStore is Postgres (open + migrate + close) and the repo wiring.
	RoleStore Role = "store"
	// RoleEvents is the in-process event broker + the runner live view
	// (WS topics jobs/outbox/runners, /api/runners/live).
	RoleEvents Role = "events"
	// RoleSync is the Zotero sync service: contextual rules at start,
	// consolidation debounce cancel at stop.
	RoleSync Role = "sync"
	// RoleSearch is the retrieval wiring (query runner client, search
	// service, query-runner health check + role probe).
	RoleSearch Role = "search"
	// RoleIngest is the ordered ingest-runner failover chain and its
	// health monitor.
	RoleIngest Role = "ingest"
	// RoleRepair is the repair track: repair API wiring (write gateway)
	// and the opt-in fixer invoker loop.
	RoleRepair Role = "repair"
	// RoleDispatcher is the claim/process loop plus the OpenSearch outbox
	// drainer.
	RoleDispatcher Role = "dispatcher"
)

// roleDeps is the dependency table: a role may only start when its
// dependencies are selected too. This is the loud-failure map — a selective
// start with a missing port fails with a diagnosis instead of silently
// half-booting (#298 DoD).
var roleDeps = map[Role][]Role{
	RoleEvents:     {RoleStore},
	RoleSync:       {RoleStore},
	RoleSearch:     {RoleStore},
	RoleIngest:     {RoleStore},
	RoleRepair:     {RoleStore, RoleSync},
	RoleDispatcher: {RoleStore, RoleIngest},
	RoleAPI:        {},
	RoleStore:      {},
}

// startOrder is the documented startup sequence (and the reverse of the
// shutdown sequence). It preserves the pre-F04 axiom-ng order exactly:
// storage first, event bus + live view before anything can publish, sync
// rules before search wiring, HTTP listener LAST (nothing serves until
// every component that can fail loudly has started).
var startOrder = []Role{RoleStore, RoleEvents, RoleSync, RoleRepair, RoleSearch, RoleIngest, RoleDispatcher, RoleAPI}

// Component is one lifecycle unit of the composition. Start is synchronous
// and complete (a component that blocks until observable — e.g. the live
// view's WaitReady — does so inside Start); Ready reports the INTERNAL
// readiness signal without blocking startup; Stop performs the component's
// slice of the ordered shutdown within the stop budget.
type Component interface {
	Name() string
	Role() Role
	Start(ctx context.Context) error
	// Ready blocks (bounded by ctx) until the component's INTERNAL
	// readiness signal fires. Startup does NOT depend on it — components
	// that are synchronous are ready when Start returns; the dispatcher's
	// negotiation-bounded readiness is what makes the ctx meaningful.
	Ready(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Root is the composition root: the ordered component set plus the wiring
// state the components share. Build it via Full or Select; run Start, wait
// on Fatal or the caller's signal, then Stop.
type Root struct {
	cfg    config.Config
	logger *log.Logger
	ports  Ports
	roles  map[Role]bool

	components []Component

	rootCtx context.Context
	cancel  context.CancelFunc
	fatal   chan error
	started atomic.Bool
	stopped atomic.Bool

	// readiness: per selected role, flips true when the component's
	// INTERNAL Ready signal fired (tracked by post-Start goroutines; read
	// by readinessSnapshot from HTTP handlers — atomic by necessity).
	ready map[Role]*atomic.Bool

	// wiring state, built progressively during Start (nil until then —
	// exactly like the pre-F04 main.go locals).
	database *db.DB
	rep      *repo.Repo
	// libStore is the F06 Library persistence (nil without the store
	// role) — the revision Mits-Schrieb + import service sit on it.
	libStore *library.Store
	// libProvider is the F07 Zotero adapter (nil without the zotero
	// provider wiring); its Close releases the writer lease.
	libProvider  *zoteroprovider.Provider
	broker       *events.Broker
	syncSvc      *axsync.Service
	httpSrv      *http.Server
	ln           net.Listener
	srv          *server.Server
	src          *zoteroprovider.LocalAPI
	disp         *dispatcher.Dispatcher
	inv          *fixerinvoker.Invoker
	ingestClient RunnerClient
}

// RolesFromConfig derives today's axiom-ng role set: the full stack on a
// configured database, API-only (with the documented warning) without one,
// dispatcher and fixer invoker strictly opt-in like today.
func RolesFromConfig(cfg config.Config) []Role {
	if cfg.DatabaseURL == "" {
		return []Role{RoleAPI}
	}
	roles := []Role{RoleAPI, RoleStore, RoleEvents, RoleSync, RoleSearch, RoleIngest}
	// Repair rides along whenever the store does (pre-F04 main wired
	// SetRepairAPI on DB + readable write key, independent of the invoker
	// env): the component no-ops without a key, and its inner
	// FixerInvokerEnabled gate keeps the invoker loop opt-in. Gating the
	// ROLE on the env would silently drop the /api/repair/* surface for the
	// key-present-invoker-off shape the F01 repair probe exercises.
	roles = append(roles, RoleRepair)
	if cfg.DispatcherEnabled {
		roles = append(roles, RoleDispatcher)
	}
	return roles
}

// Full builds the complete sidecar composition — the exact pre-F04 axiom-ng
// wiring, now in one place. It never renegotiates behavior: same components,
// same order, same log lines.
func Full(cfg config.Config, logger *log.Logger, ports Ports) (*Root, error) {
	return Select(cfg, logger, ports, RolesFromConfig(cfg)...)
}

// Select builds a composition for an explicit role set (F05's
// `axiom serve …` consumer and the lifecycle tests). Missing dependencies
// fail HERE with a diagnosis; nothing has started yet at that point.
func Select(cfg config.Config, logger *log.Logger, ports Ports, roles ...Role) (*Root, error) {
	if logger == nil {
		logger = log.Default()
	}
	ports.fillLocal()
	set := make(map[Role]bool, len(roles))
	for _, r := range roles {
		if _, known := roleDeps[r]; !known {
			return nil, fmt.Errorf("composition: unknown role %q (known: api store events sync search ingest repair dispatcher)", r)
		}
		set[r] = true
	}
	for r := range set {
		for _, dep := range roleDeps[r] {
			if !set[dep] {
				return nil, fmt.Errorf("composition: role %q requires the %q port (select %q too, or drop %q)", r, dep, dep, r)
			}
		}
	}
	if set[RoleStore] && cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("composition: role %q selected but AXIOM_DATABASE_URL is empty — the store port is not startable; set the DSN or serve api-only", RoleStore)
	}
	// The loud half-wiring guard (deliberate F04 delta, documented in #298):
	// an enabled worker without its storage used to degrade silently
	// (dispatcher env on, no DB → claims silently never happen). Refusing to
	// start half-wired is the operator-facing contract for role starts.
	if !set[RoleStore] {
		if cfg.DispatcherEnabled {
			return nil, fmt.Errorf("composition: AXIOM_DISPATCHER_ENABLED is set but the store port is not startable (AXIOM_DATABASE_URL empty) — refusing to start half-wired: set the DSN or disable the worker")
		}
		if cfg.FixerInvokerEnabled {
			return nil, fmt.Errorf("composition: AXIOM_FIXER_INVOKER_ENABLED is set but the store port is not startable (AXIOM_DATABASE_URL empty) — refusing to start half-wired: set the DSN or disable the worker")
		}
	}
	// Precedence (documented for F05's role CLI): an EXPLICIT role selection
	// wins over worker opt-in envs — but never silently. Full derives its
	// roles from the config, so this can only fire for explicit Select calls
	// that drop an env-enabled worker while keeping the store it needs.
	if set[RoleStore] {
		if cfg.DispatcherEnabled && !set[RoleDispatcher] {
			logger.Printf("note: AXIOM_DISPATCHER_ENABLED=1 but the dispatcher role is not selected — explicit role selection wins (no claim loop in this process)")
		}
		if cfg.FixerInvokerEnabled && !set[RoleRepair] {
			logger.Printf("note: AXIOM_FIXER_INVOKER_ENABLED=1 but the repair role is not selected — explicit role selection wins (no fixer loop in this process)")
		}
	}

	r := &Root{
		cfg:    cfg,
		logger: logger,
		ports:  ports,
		roles:  set,
		ready:  make(map[Role]*atomic.Bool, len(set)),
		fatal:  make(chan error, 1),
	}
	for role := range set {
		r.ready[role] = &atomic.Bool{}
	}
	r.buildComponents()
	return r, nil
}

// Roles returns the selected role set (reporting/diagnosis aid).
func (r *Root) Roles() []string {
	var out []string
	for _, role := range startOrder {
		if r.roles[role] {
			out = append(out, string(role))
		}
	}
	return out
}

// Start starts every selected component in the documented order. A failing
// component aborts the whole start: everything already started is stopped
// (bounded) and the error is returned — the caller exits non-zero on it, and
// no half-started goroutine survives in-process.
func (r *Root) Start(ctx context.Context) error {
	if r.started.Load() {
		return fmt.Errorf("composition: Start called twice")
	}
	r.rootCtx, r.cancel = context.WithCancel(ctx)
	var started []Component
	for _, c := range r.components {
		if !r.roles[c.Role()] {
			continue
		}
		if err := c.Start(r.rootCtx); err != nil {
			// Abort: undo the partial start in reverse order so a failed
			// start leaves no zombie behind (in-process proof of the
			// exit-≠-0-without-zombies contract).
			r.cancel()
			for i := len(started) - 1; i >= 0; i-- {
				undoCtx, ucancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = started[i].Stop(undoCtx)
				ucancel()
			}
			return fmt.Errorf("composition: component %s failed to start: %w", c.Name(), err)
		}
		started = append(started, c)
	}
	r.started.Store(true)
	// F05 #299: track every component's INTERNAL readiness signal in the
	// background so /api/health can serve a non-blocking snapshot
	// (Root.Ready stays the blocking aggregation for callers/tests).
	for _, c := range r.components {
		if !r.roles[c.Role()] {
			continue
		}
		comp := c
		go func() {
			if err := comp.Ready(r.rootCtx); err == nil {
				r.ready[comp.Role()].Store(true)
			}
		}()
	}
	return nil
}

// Ready waits (bounded by ctx) for every selected component's INTERNAL
// readiness signal. Since F05 the same signals feed the public
// readinessSnapshot (the /api/health readiness map) — this method stays
// the BLOCKING aggregation for callers and tests; the snapshot is the
// non-blocking projection of the same per-role flags.
func (r *Root) Ready(ctx context.Context) error {
	for _, c := range r.components {
		if !r.roles[c.Role()] {
			continue
		}
		if err := c.Ready(ctx); err != nil {
			return fmt.Errorf("composition: component %s not ready: %w", c.Name(), err)
		}
	}
	return nil
}

// readinessSnapshot is the /api/health readiness provider: role →
// "ready" (internal signal fired) | "starting" (selected, signal pending)
// | "stopping" (shutdown begun). Wired into the server in buildComponents.
func (r *Root) readinessSnapshot() map[string]string {
	out := make(map[string]string, len(r.roles))
	stopping := r.stopped.Load()
	for _, role := range startOrder {
		if !r.roles[role] {
			continue
		}
		switch {
		case stopping:
			out[string(role)] = "stopping"
		case r.ready[role].Load():
			out[string(role)] = "ready"
		default:
			out[string(role)] = "starting"
		}
	}
	return out
}

// Fatal delivers at most one process-fatal error: the #214 dispatcher fatality
// (runner unreachable past MaxStartupWait — the supervisor must restart the
// process) or an HTTP serve error. A graceful Stop never sends here.
func (r *Root) Fatal() <-chan error { return r.fatal }

// Stop performs the shutdown within the stop budget: the CALLER's
// deadline is authoritative (its cancellation propagates into every
// join); the 15s window is only the fallback for a caller without a
// deadline. Order and rationale:
//
//  1. stop accepting — the listener closes IMMEDIATELY (before the
//     cancel, so no new request can race into a half-drained process);
//     a Serve error after this point is expected, never fatal.
//  2. cancel the root context — dispatcher workers, outbox drainer, fixer
//     invoker, health monitors begin draining their in-flight work.
//  3. JOIN the drains IN PARALLEL, each with its OWN sub-budget (the F04
//     deferral, landed in F05): API/WS drain, dispatcher, fixer invoker
//     and sync stops run concurrently — a slow drain bounded by its own
//     budget can no longer starve the others. A drain outrunning its
//     budget is abandoned the way the #271/#264 shutdown guard semantics
//     prescribe: leases expire and the claim scans' expired-recovery
//     owns the rows — never lost, never terminalized by the shutdown
//     itself.
//  4. store LAST — the database pool closes only after every other join
//     settled or expired its budget (the reverse-start-order invariant:
//     every component may touch the pool during its drain).
func (r *Root) Stop(ctx context.Context) {
	if !r.started.Load() || r.stopped.Load() {
		return
	}
	r.stopped.Store(true)
	// Test-and-set, not CAS: Stop is single-caller by contract today
	// (main stops exactly once; a concurrent second Stop would at worst
	// double-run a component stop — no caller exists).
	// 1. The edge stops accepting FIRST — before the cancel. Closing the
	//    listener outright (instead of waiting for Shutdown inside the
	//    api component) makes "no new connections" immediate; in-flight
	//    requests and late WS upgrades are drained by the api component's
	//    stop (CloseLiveWebSockets sweeps pre-subscribe connections too).
	if r.ln != nil {
		_ = r.ln.Close()
	}
	// 2. Cancel: every ctx-bound goroutine (dispatcher workers, outbox
	//    drainer, fixer invoker, health monitors, runner view) begins
	//    draining its in-flight work — leases are honored, never
	//    terminalized (#271/#264 guard semantics).
	r.cancel()
	// 3. Parallel joins with per-join sub-budgets DERIVED FROM THE CALLER:
	//    cancellation propagates into every join, and each join may use the
	//    whole remaining window — concurrency (not unequal division) is the
	//    fix: none of them blocks another.
	const stopBudgetFallback = 15 * time.Second
	budget := stopBudgetFallback
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 {
			budget = rem
		}
	}
	var wg sync.WaitGroup
	for i := len(r.components) - 1; i >= 0; i-- {
		c := r.components[i]
		if !r.roles[c.Role()] || c.Role() == RoleStore {
			continue
		}
		wg.Add(1)
		go func(c Component) {
			defer wg.Done()
			subCtx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			_ = c.Stop(subCtx)
		}(c)
	}
	wg.Wait()
	// 4. The store pool closes LAST — after every join settled or expired
	//    its budget. The store's stop is not deadline-bound work today
	//    (the pool close runs unconditionally); the sub-context keeps the
	//    uniform shape for a future ctx-bound store drain.
	for i := len(r.components) - 1; i >= 0; i-- {
		c := r.components[i]
		if !r.roles[c.Role()] || c.Role() != RoleStore {
			continue
		}
		subCtx, cancel := context.WithTimeout(ctx, budget)
		_ = c.Stop(subCtx)
		cancel()
	}
}

// --- components ------------------------------------------------------------
//
// Each component is a thin adapter over an existing package's lifecycle
// surface; the wiring inside Start/Stop is the pre-F04 main.go body, moved
// verbatim (same log lines, same order). funcComponent adapts closures so
// the components stay small and local to buildComponents.

type funcComponent struct {
	name  string
	role  Role
	start func(ctx context.Context) error
	ready func(ctx context.Context) error
	stop  func(ctx context.Context) error
}

func (f funcComponent) Name() string                    { return f.name }
func (f funcComponent) Role() Role                      { return f.role }
func (f funcComponent) Start(ctx context.Context) error { return f.start(ctx) }

// Ready treats a nil ready closure as ready: components whose Start is
// synchronous have no separate readiness phase (only the dispatcher's
// negotiation-bounded WaitReady is a real closure).
func (f funcComponent) Ready(ctx context.Context) error {
	if f.ready == nil {
		return nil
	}
	return f.ready(ctx)
}

func (f funcComponent) Stop(ctx context.Context) error { return f.stop(ctx) }

func (r *Root) buildComponents() {
	// Construction shared by every component: the Zotero source and the
	// server object (route registration is wiring, not lifecycle — the
	// listener starts last as the api component).
	r.src = zoteroprovider.NewLocalAPI(r.cfg.ZoteroBaseURL, r.cfg.ZoteroLibraryID)
	if id := r.src.ServerID(); id == "" {
		r.logger.Printf("WARNING: Zotero local API not reachable at %s (is Zotero running and the local API enabled?)", r.cfg.ZoteroBaseURL)
	} else {
		r.logger.Printf("Zotero local API reachable: server-id=%s", id)
	}
	if r.cfg.DatabaseURL == "" {
		r.logger.Printf("WARNING: AXIOM_DATABASE_URL not set; running without Postgres")
	}
	r.srv = server.New(net.JoinHostPort(r.cfg.BindAddr, strconv.Itoa(r.cfg.APIPort)), r.logger)
	r.srv.RegisterCheck("zotero", server.CheckZotero(r.src))
	// F05 #299: the aggregated internal readiness (per selected role)
	// becomes publicly visible in /api/health — the composition root is
	// the only place that knows the role set, so it owns the provider.
	r.srv.SetReadinessState(r.readinessSnapshot)

	for _, c := range r.componentsFor() {
		r.components = append(r.components, c)
	}
}

// componentsFor returns the component adapters in startOrder. Everything
// below is the moved main.go body; comments preserved where they carry the
// WHY of an ordering or a guard.
func (r *Root) componentsFor() []Component {
	var comps []Component

	// store: Postgres + repo + the repo-backed API surfaces. The pool
	// closes LAST (reverse order) — every later component may touch it.
	comps = append(comps, funcComponent{
		name: "postgres",
		role: RoleStore,
		start: func(ctx context.Context) error {
			database, err := db.Open(ctx, r.cfg.DatabaseURL)
			if err != nil {
				return fmt.Errorf("postgres: %w", err)
			}
			r.database = database
			if err := database.Migrate(ctx); err != nil {
				return fmt.Errorf("postgres migrate: %w", err)
			}
			r.logger.Printf("postgres ready and migrated")
			r.srv.RegisterCheck("postgres", server.CheckDB(database.Pool()))
			r.rep = repo.New(database.Pool())
			r.srv.SetJobRepo(r.rep)
			r.srv.SetForceRebuildAPI(r.rep)
			r.srv.SetKGService(r.rep)
			r.srv.SetConsolidateService(r.rep)
			r.srv.SetSelectionRepo(r.rep)
			// F06 #300: the Library component's own migration set (own
			// ledger, same physical DB — additive; the F01 fingerprint
			// derives from the core set alone).
			if err := library.Migrate(ctx, database.Pool()); err != nil {
				return fmt.Errorf("library migrate: %w", err)
			}
			r.libStore = library.NewStore(database.Pool())
			// Source-revision Mits-Schrieb: sync completion and heal/
			// custody publish through the same store (F09 turns the Store
			// onto these rows).
			r.srv.SetRevisionPublisher(r.libStore)
			// The import contract: fake providers until F07 ports Zotero
			// behind the ports (capability-honest — unwired stays 404).
			switch r.cfg.LibraryImportProviders {
			case "fake":
				prov := library.NewFakeProvider()
				libSvc := library.NewService(library.Config{
					SourceID:       "src-library-fake",
					Provider:       "fake",
					LibraryID:      r.cfg.ZoteroLibraryID,
					MaxImportBytes: r.cfg.LibraryImportMaxBytes,
				}, r.libStore, library.NewStaging(r.cfg.ArtifactRoot), library.Ports{
					Catalog:     prov,
					Records:     prov,
					Renditions:  prov,
					Collections: prov,
					Resolvers: []library.BibliographicResolver{
						library.NewFakeResolver("crossref", "fake-v1", library.StandardCrossrefFixtures()),
						library.NewFakeResolver("open_library", "fake-v1", library.StandardOpenLibraryFixtures()),
					},
					Documents: library.FakeDocumentInspector{},
				})
				if ids, err := libSvc.ResumeInflight(ctx); err != nil {
					r.logger.Printf("WARNING: library inflight resume: %v", err)
				} else if len(ids) > 0 {
					r.logger.Printf("library: resumed %d inflight import(s) after restart", len(ids))
				}
				r.srv.SetLibraryAPI(libSvc)
				r.logger.Printf("library: import routes wired with FAKE providers (deterministic fixtures; F07 replaces them with Zotero)")
			case "":
				r.logger.Printf("library: import providers not configured (AXIOM_LIBRARY_IMPORT_PROVIDERS) — /api/v1/library/imports answers 404")
			case "zotero":
				// The REAL provider (F07 #301): everything Zotero flows through
				// the adapter package; this wiring is the only place the Library
				// meets it. The mirror source identity is ensured on demand (the
				// first sync would create it too) so revisions/GetSource bind to
				// the same source id the sync mirror uses.
				serverID := r.src.ServerID()
				sourceID, serr := r.rep.EnsureSource(ctx, r.cfg.ZoteroBaseURL, r.cfg.ZoteroLibraryID, serverID)
				if serr != nil {
					return fmt.Errorf("library zotero source: %w", serr)
				}
				// Write key optional: without it the provider is read-only and
				// the write ports report Unavailable (capability-honest).
				apiKey := ""
				// len > 8: a real Zotero local-API key is far longer; shorter
				// file content is a placeholder/absent — same heuristic the
				// repair component applies to its key file.
				if b, kerr := os.ReadFile(r.cfg.ZoteroWriteKeyFile); kerr == nil && len(b) > 8 {
					apiKey = strings.TrimSpace(string(b))
				} else {
					r.logger.Printf("library: no zotero write key under %s — zotero provider is READ-ONLY (imports report unavailable at the first write step)", r.cfg.ZoteroWriteKeyFile)
				}
				// New ACQUIRES the provider-scoped writer lease: a second
				// write-capable Library instance against the same Zotero scope
				// aborts THIS start (single-writer declaration, #301).
				prov, perr := zoteroprovider.New(ctx, zoteroprovider.Options{
					BaseURL:   r.cfg.ZoteroBaseURL,
					LibraryID: r.cfg.ZoteroLibraryID,
					APIKey:    apiKey,
					Store:     r.libStore,
				})
				if perr != nil {
					return fmt.Errorf("library zotero provider: %w", perr)
				}
				r.libProvider = prov
				libSvc := library.NewService(library.Config{
					SourceID:       sourceID,
					Provider:       "zotero",
					LibraryID:      r.cfg.ZoteroLibraryID,
					MaxImportBytes: r.cfg.LibraryImportMaxBytes,
				}, r.libStore, library.NewStaging(r.cfg.ArtifactRoot), library.Ports{
					Catalog:     prov,
					Records:     prov,
					Renditions:  prov,
					Collections: prov,
					Resolvers: []library.BibliographicResolver{
						zoteroprovider.NewCrossref("", nil),
						zoteroprovider.NewOpenLibrary("", nil),
					},
					Documents: zoteroprovider.NewPDFInspector(),
				})
				if ids, err := libSvc.ResumeInflight(ctx); err != nil {
					r.logger.Printf("WARNING: library inflight resume: %v", err)
				} else if len(ids) > 0 {
					r.logger.Printf("library: resumed %d inflight import(s) after restart", len(ids))
				}
				r.srv.SetLibraryAPI(libSvc)
				r.logger.Printf("library: import routes wired with the ZOTERO provider (single-writer lease %s)", prov.LeaseScopeLabel())
			default:
				return fmt.Errorf("library: unknown AXIOM_LIBRARY_IMPORT_PROVIDERS %q (known: fake, zotero)", r.cfg.LibraryImportProviders)
			}
			// Remote source delivery (endpoint verify, dispatcher sign): wired
			// on the STORE component, not sync — the dispatcher's source fetch
			// reads it, so Select(api, store, ingest, dispatcher) must boot a
			// dispatcher whose source can fetch work (role-table honesty).
			// Same secret on both sides; empty secret disables the endpoint.
			r.srv.SetProcessorSourceSecret(r.cfg.ProcessorSourceSecret)
			r.srv.SetProcessorSourceRepo(r.rep)
			return nil
		},
		stop: func(ctx context.Context) error {
			if r.libProvider != nil {
				// Release the writer lease BEFORE the pool closes (graceful;
				// the lease TTL covers the ungraceful exits).
				_ = r.libProvider.Close()
			}
			if r.database != nil {
				r.database.Close()
			}
			return nil
		},
	})

	// events: broker + runner live view. WaitReady INSIDE Start preserves
	// the #169 review invariant: the deriver's bus subscription is live
	// BEFORE the dispatcher (started later) can publish its first event.
	comps = append(comps, funcComponent{
		name: "events",
		role: RoleEvents,
		start: func(ctx context.Context) error {
			r.broker = events.NewBroker()
			r.srv.SetWSAPI(r.broker, r.rep, r.cfg.WSSecret)
			runnerView := server.NewRunnerLive(r.broker, r.logger)
			r.srv.SetRunnerLive(runnerView)
			if !r.roles[RoleDispatcher] {
				// #249: the event bus is process-local. Without a dispatcher
				// in THIS process, /api/runners/live and the ws job topics see
				// no claims at all — the empty-list shape of the 2026-09-04
				// incident.
				r.logger.Printf("#249: no dispatcher in this process — /api/runners/live sees no claims (the bus is process-local); serve the dispatcher here or query the agent's port")
			}
			go runnerView.Start(ctx.Done())
			runnerView.WaitReady()
			return nil
		},
		stop: func(ctx context.Context) error { return nil },
	})

	// sync: contextual rules at start, consolidation debounce at stop.
	comps = append(comps, funcComponent{
		name: "sync",
		role: RoleSync,
		start: func(ctx context.Context) error {
			r.syncSvc = axsync.New(r.src, r.rep, r.cfg.ZoteroBaseURL, r.cfg.ZoteroLibraryID, r.logger)
			// #255/#262 contextual source class: resolve + validate the
			// configured rule inputs against the SYNCED canonical state. Boot
			// ALWAYS succeeds (#262 owner ruling): a never-synced DB degrades
			// (rules inactive until the first sync converges them, health
			// shows degraded_no_sync); with sync state present an unknown
			// path/tag stays a loud start error.
			if err := r.syncSvc.InitContextual(ctx, r.cfg.ContextualCollectionPaths, r.cfg.ContextualTags); err != nil {
				return fmt.Errorf("contextual rules: %w", err)
			}
			r.srv.SetContextualState(r.syncSvc.ContextualState)
			r.srv.SetSyncAPI(r.syncSvc)
			// #197 standing entity consolidation: every successful sync hooks
			// a debounced consolidation run (one run per sync burst).
			r.syncSvc.SetConsolidator(r.rep)
			// F06 #300: sync completion publishes source revisions.
			if r.libStore != nil {
				r.syncSvc.SetRevisionSink(r.libStore)
			}
			return nil
		},
		stop: func(ctx context.Context) error {
			if r.syncSvc != nil {
				// Cancel a still-debounced consolidation hook BEFORE the pool
				// closes — a late hook run would only log an error.
				r.syncSvc.StopConsolidation()
			}
			return nil
		},
	})

	// repair: write gateway + fixer invoker (the repair loop).
	comps = append(comps, funcComponent{
		name: "repair",
		role: RoleRepair,
		start: func(ctx context.Context) error {
			keyBytes, kerr := os.ReadFile(r.cfg.ZoteroWriteKeyFile)
			if kerr != nil || len(keyBytes) <= 8 {
				// External optional secret: absence degrades with a log
				// (today's behavior), unlike a missing PORT which fails the
				// start loudly.
				r.logger.Printf("repair API disabled (kein zotero write key unter %s)", r.cfg.ZoteroWriteKeyFile)
				return nil
			}
			writeBase := strings.TrimSuffix(strings.TrimSuffix(r.cfg.ZoteroBaseURL, "/api"), "/")
			zoteroWrite := zoteroprovider.NewWriteClient(writeBase, r.src.ServerID(), strings.TrimSpace(string(keyBytes)))
			r.srv.SetRepairAPI(r.rep, zoteroWrite, r.cfg.QuarantineRoot)
			r.logger.Printf("repair API enabled (zotero write gateway, quarantine under %s)", r.cfg.QuarantineRoot)
			// #206 fixer invoker: the mail-ingest side of the repair queue.
			if !r.cfg.FixerInvokerEnabled {
				return nil
			}
			r.inv = fixerinvoker.New(fixerinvoker.Config{
				Command:     r.cfg.FixerCommand,
				Concurrency: r.cfg.FixerConcurrency,
				Interval:    r.cfg.FixerInterval,   // F05: the F04 test-seam deferral — visible via config get --effective
				OCRTimeout:  r.cfg.FixerOCRTimeout, // #293: OCR wedge-guard (0 = invoker default 24h)
			}, fixerinvoker.Deps{
				Rep:            r.rep,
				Apply:          fixerinvoker.LiveApplyDeps(r.rep, zoteroWrite),
				QuarantineRoot: r.cfg.QuarantineRoot,
				// #282 post-heal auto-sync: every successful heal runs a
				// targeted sync (include = healed document).
				Sync: r.syncSvc,
				// #298 FixerExec port: nil = local (process-group exec).
				Exec: r.ports.FixerExec,
			}, r.logger)
			go func() {
				if err := r.inv.Run(ctx); err != nil {
					r.logger.Printf("fixer invoker stopped: %v", err)
				}
			}()
			return nil
		},
		stop: func(ctx context.Context) error {
			if r.inv == nil {
				return nil
			}
			// Join the in-flight case drain (Run waits for claimed cases to
			// settle under the cancelled context), bounded by the stop budget.
			select {
			case <-r.inv.Stopped():
			case <-ctx.Done():
				r.logger.Printf("repair: fixer invoker drain exceeded the stop budget — in_repair cases go to the stale-reaper")
			}
			return nil
		},
	})

	// search: query runner client + retrieval wiring + role probe.
	comps = append(comps, funcComponent{
		name: "search",
		role: RoleSearch,
		start: func(ctx context.Context) error {
			// R3 (#133) + R4 (#134): retrieval API. Hybrid recall + rerank
			// over the QUERY runner's endpoints (R1/R2).
			queryClient, qerr := r.ports.QueryRunner(r.cfg)
			if qerr != nil {
				return fmt.Errorf("processor client (search): %w", qerr)
			}
			searchSvc := search.New(r.cfg.OpenSearchURL, r.cfg.OpenSearchUsername, r.cfg.OpenSearchPassword, queryClient, r.rep, r.logger)
			searchSvc.SparseArm = r.cfg.SearchSparseArm
			searchSvc.Rerank = r.cfg.SearchRerank
			searchSvc.FrontmatterFilter = r.cfg.SearchFrontmatterFilter
			searchSvc.MaxPerBook = r.cfg.SearchMaxPerBook
			if r.cfg.SearchGraphArm {
				searchSvc.GraphArm = true
				searchSvc.SetGraphSource(r.rep)
			}
			r.srv.SetSearchService(searchSvc)
			r.srv.SetPassageService(searchSvc) // A1 #165: same service, passage surface
			// Role probe (R4 Ziel 1/3): capability check of the query runner
			// at start. Best-effort: an unreachable query runner keeps search
			// degraded-but-up (R3 fallback).
			go probeQueryRunnerRole(ctx, queryClient, r.cfg.QueryRunnerURL, r.logger)
			r.srv.RegisterCheck("query-runner", runnerCheck(queryClient))
			return nil
		},
		stop: func(ctx context.Context) error { return nil },
	})

	// ingest: ordered failover chain + health monitor.
	comps = append(comps, funcComponent{
		name: "ingest",
		role: RoleIngest,
		start: func(ctx context.Context) error {
			ingestClient, ierr := r.ports.IngestRunner(r.cfg, r.logger)
			if ierr != nil {
				return fmt.Errorf("ingest client: %w", ierr)
			}
			r.srv.RegisterCheck("ingest-runner", runnerCheck(ingestClient))
			// Health-based candidate selection (#207): periodic probe keeps
			// dead candidates out of the submit path front. Optional port
			// surface: the local FailoverClient has it, monitor-less
			// bindings (fakes, direct drives) skip the probe.
			if hm, ok := ingestClient.(interface {
				StartHealthMonitor(ctx context.Context, interval time.Duration)
			}); ok {
				hm.StartHealthMonitor(ctx, r.cfg.RunnerHealthInterval)
			}
			r.logger.Printf("runner roles: query=%s ingest=%v (health probe every %s)",
				r.cfg.QueryRunnerURL, r.cfg.IngestCandidates(), r.cfg.RunnerHealthInterval)
			r.ingestClient = ingestClient
			return nil
		},
		stop: func(ctx context.Context) error { return nil },
	})

	// dispatcher: the claim loop + outbox drainer. Ready is the post-
	// negotiation signal (INTERNAL — aggregated by Root.Ready, never a
	// health field).
	comps = append(comps, funcComponent{
		name: "dispatcher",
		role: RoleDispatcher,
		start: func(ctx context.Context) error {
			// Gate 4: wire the REAL persistence boundary
			// (repo.PersistResult fence-completes the job atomically in its
			// single TX).
			r.disp = dispatcher.NewWithPersister(r.rep, r.ingestClient, r.rep, dispatcher.Config{
				WorkerID:               r.cfg.DispatcherWorkerID,
				RunnerName:             r.cfg.ProcessorRunnerName,
				Concurrency:            r.cfg.DispatcherConcurrency,
				APIPort:                r.cfg.APIPort,
				Profile:                json.RawMessage(r.cfg.DispatcherProfile),
				LeaseDuration:          r.cfg.DispatcherLeaseDuration,
				ArtifactRoot:           r.cfg.ArtifactRoot,
				OpenSearchURL:          r.cfg.OpenSearchURL,
				OpenSearchUsername:     r.cfg.OpenSearchUsername,
				OpenSearchPassword:     r.cfg.OpenSearchPassword,
				ProcessorSourceBaseURL: r.cfg.ProcessorSourceBaseURL,
				ProcessorSourceSecret:  r.cfg.ProcessorSourceSecret,
				PreflightEnabled:       r.cfg.DispatcherPreflightEnabled, // #175
			}, r.logger)
			// #168 (B2): let the dispatcher EMIT lifecycle events onto the
			// shared broker (observer-only passenger).
			r.disp.SetEventBroker(r.broker)
			// #214: a fatal dispatcher error must exit the process non-zero
			// so launchd/KeepAlive restarts it. A graceful shutdown
			// (rootCtx cancelled) returns nil and never lands here.
			go func() {
				if err := r.disp.Run(ctx); err != nil {
					r.logger.Printf("dispatcher stopped: %v", err)
					select {
					case r.fatal <- fmt.Errorf("dispatcher fatal: process must restart when the runner is available: %w", err):
					default:
					}
				}
			}()
			return nil
		},
		ready: func(ctx context.Context) error {
			return r.disp.WaitReady(ctx)
		},
		stop: func(ctx context.Context) error {
			if r.disp == nil {
				return nil
			}
			// JOIN the drain: Run returns when all workers have settled
			// their current claims (leases left intact for expiry-recovery).
			select {
			case <-r.disp.Stopped():
			case <-ctx.Done():
				r.logger.Printf("dispatcher drain exceeded the stop budget — in-flight leases go to expiry-recovery")
			}
			return nil
		},
	})

	// api: the HTTP listener, LAST — nothing serves until every
	// fail-loudly-later component has started.
	comps = append(comps, funcComponent{
		name: "http",
		role: RoleAPI,
		start: func(ctx context.Context) error {
			r.httpSrv = &http.Server{
				Addr:              r.srvAddr(),
				Handler:           r.srv,
				ReadHeaderTimeout: 10 * time.Second,
			}
			ln, err := net.Listen("tcp", r.httpSrv.Addr)
			if err != nil {
				return fmt.Errorf("http server: %w", err)
			}
			r.ln = ln
			// Pre-F04 identity: the CONFIGURED address is logged (BindAddr:port),
			// not the resolved listener address — identical for the deployed
			// 127.0.0.1, and log-identical for localhost/IPv6 shapes too.
			r.logger.Printf("listening on %s", r.httpSrv.Addr)
			go func() {
				if err := r.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed && !r.stopped.Load() {
					// A serve error is fatal ONLY while running: Stop closes
					// the listener deliberately (phase 1 of the shutdown) —
					// that close-error is the expected path, never fatal.
					select {
					case r.fatal <- fmt.Errorf("http server: %w", err):
					default:
					}
				}
			}()
			return nil
		},
		stop: func(ctx context.Context) error {
			if r.httpSrv == nil {
				return nil
			}
			// Live WS connections first: hijacked sockets are invisible to
			// Shutdown and would otherwise only die with the process.
			r.srv.CloseLiveWebSockets()
			if err := r.httpSrv.Shutdown(ctx); err != nil {
				r.logger.Printf("http shutdown: %v", err)
			}
			return nil
		},
	})

	return comps
}

// srvAddr is the api bind address (host:port from config).
func (r *Root) srvAddr() string {
	return net.JoinHostPort(r.cfg.BindAddr, strconv.Itoa(r.cfg.APIPort))
}

// Addr returns the bound listener address ("" before Start) — the test
// surface for dialing the composed stack.
func (r *Root) Addr() string {
	if r.ln == nil {
		return ""
	}
	return r.ln.Addr().String()
}
