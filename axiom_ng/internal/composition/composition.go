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
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/dispatcher"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/fixerinvoker"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/server"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
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
	started bool
	stopped bool

	// wiring state, built progressively during Start (nil until then —
	// exactly like the pre-F04 main.go locals).
	database     *db.DB
	rep          *repo.Repo
	broker       *events.Broker
	syncSvc      *sync.Service
	httpSrv      *http.Server
	ln           net.Listener
	srv          *server.Server
	src          *zotero.LocalAPI
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
	if cfg.FixerInvokerEnabled {
		roles = append(roles, RoleRepair)
	}
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
	if err := ports.fillLocal(); err != nil {
		return nil, err
	}
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

	r := &Root{
		cfg:    cfg,
		logger: logger,
		ports:  ports,
		roles:  set,
		fatal:  make(chan error, 1),
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
	if r.started {
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
	r.started = true
	return nil
}

// Ready waits (bounded by ctx) for every selected component's INTERNAL
// readiness signal. Aggregation stays inside the composition — this is not
// a health endpoint and never feeds one (F05 changes that, deliberately).
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

// Fatal delivers at most one process-fatal error: the #214 dispatcher fatality
// (runner unreachable past MaxStartupWait — the supervisor must restart the
// process) or an HTTP serve error. A graceful Stop never sends here.
func (r *Root) Fatal() <-chan error { return r.fatal }

// Stop performs the ordered shutdown within the stop budget (today's 15s
// window). Order and rationale (reverse of start):
//
//  1. api — stop accepting: close live WS connections (hijacked sockets are
//     invisible to http.Server.Shutdown), then drain in-flight HTTP requests.
//  2. cancel the root context — dispatcher workers, outbox drainer, fixer
//     invoker, health monitors begin draining their in-flight work.
//  3. dispatcher / repair — JOIN the drains: Run returns only when every
//     lease-holding goroutine has stopped. A drain outrunning the budget is
//     abandoned to the lease-expiry recovery (#271/#264 guard semantics).
//  4. sync — cancel a still-debounced consolidation hook before the pool
//     can vanish under it.
//  5. store — close the database pool, last.
func (r *Root) Stop(ctx context.Context) {
	if !r.started || r.stopped {
		return
	}
	r.stopped = true
	// 1. The edge stops accepting FIRST — before the cancel, so no new
	// request races into a half-drained process (WS connections close
	// explicitly; hijacked sockets are invisible to http.Server.Shutdown).
	for i := len(r.components) - 1; i >= 0; i-- {
		c := r.components[i]
		if c.Role() == RoleAPI && r.roles[RoleAPI] {
			_ = c.Stop(ctx)
		}
	}
	// 2. Cancel: every ctx-bound goroutine (dispatcher workers, outbox
	// drainer, fixer invoker, health monitors, runner view) begins
	// draining its in-flight work — leases are honored, never
	// terminalized (#271/#264 guard semantics).
	r.cancel()
	// 3. JOIN the drains (reverse start order): dispatcher and fixer
	// returns mean every lease-holding goroutine settled; sync cancels the
	// consolidation debounce; the store pool closes LAST.
	for i := len(r.components) - 1; i >= 0; i-- {
		c := r.components[i]
		if c.Role() == RoleAPI || !r.roles[c.Role()] {
			continue
		}
		_ = c.Stop(ctx)
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
func (f funcComponent) Ready(ctx context.Context) error { return f.ready(ctx) }
func (f funcComponent) Stop(ctx context.Context) error  { return f.stop(ctx) }

func (r *Root) buildComponents() {
	// Construction shared by every component: the Zotero source and the
	// server object (route registration is wiring, not lifecycle — the
	// listener starts last as the api component).
	r.src = zotero.NewLocalAPI(r.cfg.ZoteroBaseURL, r.cfg.ZoteroLibraryID)
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
			return nil
		},
		ready: func(ctx context.Context) error { return nil },
		stop: func(ctx context.Context) error {
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
		ready: func(ctx context.Context) error { return nil },
		stop:  func(ctx context.Context) error { return nil },
	})

	// sync: contextual rules at start, consolidation debounce at stop.
	comps = append(comps, funcComponent{
		name: "sync",
		role: RoleSync,
		start: func(ctx context.Context) error {
			r.syncSvc = sync.New(r.src, r.rep, r.cfg.ZoteroBaseURL, r.cfg.ZoteroLibraryID, r.logger)
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
			// Remote source delivery: same secret on both sides.
			r.srv.SetProcessorSourceSecret(r.cfg.ProcessorSourceSecret)
			r.srv.SetProcessorSourceRepo(r.rep)
			return nil
		},
		ready: func(ctx context.Context) error { return nil },
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
			zoteroWrite := zotero.NewWriteClient(writeBase, r.src.ServerID(), strings.TrimSpace(string(keyBytes)))
			r.srv.SetRepairAPI(r.rep, zoteroWrite, r.cfg.QuarantineRoot)
			r.logger.Printf("repair API enabled (zotero write gateway, quarantine under %s)", r.cfg.QuarantineRoot)
			// #206 fixer invoker: the mail-ingest side of the repair queue.
			if !r.cfg.FixerInvokerEnabled {
				return nil
			}
			r.inv = fixerinvoker.New(fixerinvoker.Config{
				Command:     r.cfg.FixerCommand,
				Concurrency: r.cfg.FixerConcurrency,
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
		ready: func(ctx context.Context) error { return nil },
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
		ready: func(ctx context.Context) error { return nil },
		stop:  func(ctx context.Context) error { return nil },
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
		ready: func(ctx context.Context) error { return nil },
		stop:  func(ctx context.Context) error { return nil },
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
			r.logger.Printf("listening on %s", ln.Addr().String())
			go func() {
				if err := r.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
					select {
					case r.fatal <- fmt.Errorf("http server: %w", err):
					default:
					}
				}
			}()
			return nil
		},
		ready: func(ctx context.Context) error { return nil },
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
