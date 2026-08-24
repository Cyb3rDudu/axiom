// Command axiom-ng runs the Zotero RAG sidecar. It orchestrates indexing of a
// Zotero library and exposes the REST API for retrieval and chat. It runs on
// the same host as Zotero and resolves attachments to local files.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/dispatcher"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/server"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/version"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

func main() {
	// #205 §5: version stamp. Release builds inject Version/Commit/BuildType
	// via -ldflags; a bare `go build` reports the debug default.
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version.Banner())
		return
	}

	// #198 item 1 — frontmatter cleanup pass: KG relations/entities whose
	// evidence sits in gated frontmatter sections (TOC / author lists /
	// preface / bibliography / index / title lines) leave the active graph.
	// Dry-run by default (candidate report); --apply executes the drop.
	if len(os.Args) > 1 && os.Args[1] == "-cleanup-frontmatter-kg" {
		apply := false
		for _, a := range os.Args[2:] {
			if a == "--apply" {
				apply = true
			}
		}
		cfg := config.Load()
		logger := log.New(os.Stderr, "fmgate: ", log.LstdFlags)
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer d.Close()
		rep, err := repo.New(d.Pool()).CleanupFrontmatterKG(context.Background(), apply)
		if err != nil {
			logger.Fatalf("cleanup: %v", err)
		}
		out, _ := json.MarshalIndent(rep, "", "  ")
		if apply {
			logger.Printf("frontmatter cleanup APPLIED: %+v", rep.Totals)
		} else {
			logger.Printf("frontmatter cleanup DRY RUN (pass --apply to execute): %+v", rep.Totals)
		}
		fmt.Println(string(out))
		return
	}

	// Wave epilogue mode (#193): consolidation of same-canonical-form
	// entities across active snapshots. Runs ONCE and exits — the wave
	// runbook calls it after the drain (peer of the OS==PG parity check).
	if len(os.Args) > 1 && os.Args[1] == "-consolidate-relations" {
		// #198-2: one aggregated edge per (source,target) pair among active
		// snapshots. Dry-run by default; --apply mutates.
		cfg := config.Load()
		logger := log.New(os.Stderr, "relations: ", log.LstdFlags)
		apply := false
		for _, a := range os.Args[2:] {
			if a == "--apply" {
				apply = true
			}
		}
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer d.Close()
		rep := repo.New(d.Pool())
		if !apply {
			_, pairs, err := rep.RelationsConsolidationDryRun(context.Background())
			if err != nil {
				logger.Fatalf("dry-run: %v", err)
			}
			logger.Printf("dry-run: %d multi-edge pairs would collapse (use --apply)", pairs)
			return
		}
		rep2, err := rep.ConsolidateRelationsReport(context.Background())
		if err != nil {
			logger.Fatalf("consolidate: %v", err)
		}
		logger.Printf("relations consolidation complete: %+v", rep2)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-normalize-entity-types" {
		// #198-3: deterministic typing rules over active entities.
		// Dry-run by default; --apply mutates.
		cfg := config.Load()
		logger := log.New(os.Stderr, "typing: ", log.LstdFlags)
		apply := false
		for _, a := range os.Args[2:] {
			if a == "--apply" {
				apply = true
			}
		}
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer d.Close()
		rp := repo.New(d.Pool())
		if !apply {
			c, err := rp.EntityTypingCounts(context.Background())
			if err != nil {
				logger.Fatalf("dry-run: %v", err)
			}
			logger.Printf("dry-run: %+v (use --apply)", c)
			return
		}
		tr, err := rp.NormalizeEntityTypes(context.Background())
		if err != nil {
			logger.Fatalf("normalize: %v", err)
		}
		logger.Printf("entity typing complete: %+v", tr)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-bind-all-aliases" {
		// #199 W6: guarded exact+flexion binding in one pass (W3 guards).
		cfg := config.Load()
		logger := log.New(os.Stderr, "aliases: ", log.LstdFlags)
		apply := false
		for _, a := range os.Args[2:] {
			if a == "--apply" {
				apply = true
			}
		}
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer d.Close()
		rp := repo.New(d.Pool())
		if !apply {
			c, err := rp.BindExactFormAliasesDryRun(context.Background())
			if err != nil {
				logger.Fatalf("dry-run exact: %v", err)
			}
			n, err := rp.EntityAliasCounts(context.Background())
			if err != nil {
				logger.Fatalf("dry-run counts: %v", err)
			}
			logger.Printf("dry-run: exact=%+v counts=%+v (use --apply)", c, n)
			return
		}
		ar, err := rp.BindAllAliases(context.Background())
		if err != nil {
			logger.Fatalf("bind-all: %v", err)
		}
		logger.Printf("all aliases complete: %+v", ar)
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "-bind-flexion-aliases" {
		// #198-3: flexion family alias links.
		cfg := config.Load()
		logger := log.New(os.Stderr, "aliases: ", log.LstdFlags)
		apply := false
		for _, a := range os.Args[2:] {
			if a == "--apply" {
				apply = true
			}
		}
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer d.Close()
		rp := repo.New(d.Pool())
		if !apply {
			c, err := rp.EntityAliasCounts(context.Background())
			if err != nil {
				logger.Fatalf("dry-run: %v", err)
			}
			logger.Printf("dry-run: %+v (use --apply)", c)
			return
		}
		ar, err := rp.BindFlexionAliases(context.Background())
		if err != nil {
			logger.Fatalf("bind: %v", err)
		}
		logger.Printf("flexion aliases complete: %+v", ar)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-repoint-alias-edges" {
		// #198-3 Nachzug: re-point variant edges to family survivors,
		// delete intra-family self-loops, then run -consolidate-relations.
		cfg := config.Load()
		logger := log.New(os.Stderr, "repoint: ", log.LstdFlags)
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer d.Close()
		if err := repo.New(d.Pool()).RepointAliasEdges(context.Background()); err != nil {
			logger.Fatalf("repoint: %v", err)
		}
		logger.Printf("alias-variant edges re-pointed to survivors; intra-family self-loops deleted")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-consolidate-entities" {
		// #199 W6 hardening: dry-run by default; --apply mutates. This flag
		// shares the same operator discipline as relation consolidation,
		// typing normalization, and alias binding.
		cfg := config.Load()
		logger := log.New(os.Stderr, "epilogue: ", log.LstdFlags)
		apply := false
		for _, a := range os.Args[2:] {
			if a == "--apply" {
				apply = true
			}
		}
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer d.Close()
		rp := repo.New(d.Pool())
		if !apply {
			report, err := rp.EntityConsolidationDryRun(context.Background())
			if err != nil {
				logger.Fatalf("dry-run: %v", err)
			}
			logger.Printf("dry-run: %d guarded groups / %d entities would merge (use --apply)",
				report.DuplicateFormsBefore, report.Merged)
			return
		}
		report, err := rp.ConsolidateEntitiesReport(context.Background())
		if err != nil {
			logger.Fatalf("consolidate: %v", err)
		}
		logger.Printf("entity consolidation complete: %d entities merged, duplicate forms %d->%d",
			report.Merged, report.DuplicateFormsBefore, report.DuplicateFormsAfter)
		return
	}

	ctx := context.Background()
	cfg := config.Load()
	logger := log.New(os.Stderr, "axiom-ng: ", log.LstdFlags)
	logger.Printf("starting %s", version.Banner())

	// #205 §5: a debug build must never serve production ports. Production is
	// 8011 (API) and 8013–8015 (dispatchers). Opt out explicitly for local
	// dev with AXIOM_ALLOW_DEBUG_BIND=1.
	if version.DebugBindRefused(version.BuildType, cfg.APIPort, os.Getenv) {
		logger.Fatalf("refusing to bind production port %d with %s — build a release artifact (make rag) or set AXIOM_ALLOW_DEBUG_BIND=1 for local dev",
			cfg.APIPort, version.Banner())
	}

	src := zotero.NewLocalAPI(cfg.ZoteroBaseURL, cfg.ZoteroLibraryID)
	if id := src.ServerID(); id == "" {
		logger.Printf("WARNING: Zotero local API not reachable at %s (is Zotero running and the local API enabled?)", cfg.ZoteroBaseURL)
	} else {
		logger.Printf("Zotero local API reachable: server-id=%s", id)
	}

	var database *db.DB
	var syncSvc *sync.Service
	if cfg.DatabaseURL == "" {
		logger.Printf("WARNING: AXIOM_DATABASE_URL not set; running without Postgres")
	} else {
		var err error
		database, err = db.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer database.Close()
		if err := database.Migrate(ctx); err != nil {
			logger.Fatalf("postgres migrate: %v", err)
		}
		logger.Printf("postgres ready and migrated")
	}

	addr := cfg.BindAddr + ":" + strconv.Itoa(cfg.APIPort)
	srv := server.New(addr, logger)
	srv.RegisterCheck("zotero", server.CheckZotero(src))

	// One signal context drives graceful shutdown of BOTH the dispatcher and the
	// HTTP server, so SIGINT/SIGTERM cannot be held off by one half the process.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// #214: carries a fatal dispatcher error (startup capability negotiation
	// still failing after the retry window) to the main select, which must exit
	// non-zero so launchd/KeepAlive restarts the process instead of leaving a
	// living process with a dead claim loop. Buffered: the dispatcher goroutine
	// never blocks on it.
	dispErrCh := make(chan error, 1)

	if database != nil {
		srv.RegisterCheck("postgres", server.CheckDB(database.Pool()))
		// Wire the sync service and ingest-job listing into the API.
		rep := repo.New(database.Pool())
		syncSvc = sync.New(src, rep, cfg.ZoteroBaseURL, cfg.ZoteroLibraryID, logger)
		srv.SetSyncAPI(syncSvc)
		srv.SetJobRepo(rep)
		// #197 standing entity consolidation: every successful sync hooks a
		// debounced consolidation run (one run per sync burst).
		syncSvc.SetConsolidator(rep)
		// Remote source delivery: same secret on both sides (endpoint verify,
		// dispatcher sign). Empty secret disables the endpoint (404 on all).
		srv.SetProcessorSourceSecret(cfg.ProcessorSourceSecret)
		srv.SetProcessorSourceRepo(rep)

		// #184 fix-service surface: the RAG is the ONLY Zotero write
		// gateway. The write key lives outside the repo
		// (AXIOM_ZOTERO_WRITE_KEY_FILE, default ~/.axiom-ng/write-api-key);
		// the DeepSeek key NEVER enters this process — it is fix-service
		// env by design.
		if keyBytes, kerr := os.ReadFile(cfg.ZoteroWriteKeyFile); kerr == nil && len(keyBytes) > 8 {
			writeBase := strings.TrimSuffix(strings.TrimSuffix(cfg.ZoteroBaseURL, "/api"), "/")
			srv.SetRepairAPI(rep, zotero.NewWriteClient(writeBase, src.ServerID(), strings.TrimSpace(string(keyBytes))),
				cfg.QuarantineRoot)
			logger.Printf("repair API enabled (zotero write gateway, quarantine under %s)", cfg.QuarantineRoot)
		} else {
			logger.Printf("repair API disabled (kein zotero write key unter %s)", cfg.ZoteroWriteKeyFile)
		}

		// R3 (#133) + R4 (#134): retrieval API. Hybrid recall + rerank over the
		// QUERY runner's endpoints (R1/R2) — its own client, defaulting to the
		// local always-on runner (AXIOM_QUERY_RUNNER_URL overrides). Query
		// failure degrades search per R3 (BM25-only/unreranked), never fails
		// over to another runner.
		queryClient, qerr := processor.New(processor.Options{
			BaseURL: cfg.QueryRunnerURL,
		})
		if qerr != nil {
			logger.Fatalf("processor client (search): %v", qerr)
		}
		searchSvc := search.New(cfg.OpenSearchURL, cfg.OpenSearchUsername, cfg.OpenSearchPassword, queryClient, rep, logger)
		searchSvc.SparseArm = cfg.SearchSparseArm
		searchSvc.Rerank = cfg.SearchRerank
		searchSvc.FrontmatterFilter = cfg.SearchFrontmatterFilter
		searchSvc.MaxPerBook = cfg.SearchMaxPerBook
		if cfg.SearchGraphArm {
			searchSvc.GraphArm = true
			searchSvc.SetGraphSource(rep)
		}
		srv.SetSearchService(searchSvc)
		srv.SetPassageService(searchSvc) // A1 #165: same service, passage surface
		// R6 (#136): knowledge-graph read API over the L6 data.
		srv.SetKGService(rep)
		// #197: standing consolidation write route (POST /api/kg/consolidate).
		srv.SetConsolidateService(rep)
		srv.SetSelectionRepo(rep) // A2 #166: selection + documents listing
		// Role probe (R4 Ziel 1/3): capability check of the query runner at
		// start — verifies query_embedding/reranking and logs the role map.
		// Best-effort: an unreachable query runner keeps search degraded-but-
		// up (R3 fallback), it must not kill the sidecar.
		go probeQueryRunnerRole(sigCtx, queryClient, cfg.QueryRunnerURL, logger)
		srv.RegisterCheck("query-runner", runnerCheck(queryClient))

		// Ingest chain (#207 generalizes R4 #134): an ORDERED candidate list
		// from AXIOM_PROCESSOR_URLS (plural wins) or the legacy singular pair
		// (AXIOM_PROCESSOR_URL + AXIOM_INGEST_FALLBACK_URL). A periodic health
		// probe keeps dead candidates out of the submit path; submit-time
		// failover stays as the safety net.
		newIngestClient := func() (*processor.FailoverClient, error) {
			var clients []*processor.Client
			for _, url := range cfg.IngestCandidates() {
				c, err := processor.New(processor.Options{
					BaseURL:       url,
					ResultTimeout: cfg.ProcessorRequestTimeout,
				})
				if err != nil {
					return nil, err
				}
				clients = append(clients, c)
			}
			return processor.NewFailoverChain(clients, logger), nil
		}
		ingestClient, ierr := newIngestClient()
		if ierr != nil {
			logger.Fatalf("ingest client: %v", ierr)
		}
		srv.RegisterCheck("ingest-runner", runnerCheck(ingestClient))
		// Health-based candidate selection (#207): periodic probe keeps dead
		// candidates out of the submit path front - a briefly dead Carrier is
		// not asked first, so the per-submit connect timeout disappears.
		ingestClient.StartHealthMonitor(sigCtx, cfg.RunnerHealthInterval)
		logger.Printf("runner roles: query=%s ingest=%v (health probe every %s)",
			cfg.QueryRunnerURL, cfg.IngestCandidates(), cfg.RunnerHealthInterval)

		// The dispatcher is opt-in and runs only when explicitly enabled. It
		// claims jobs, drives them through the processor and back to a terminal
		// state; a broken processor is surfaced on start via capability
		// negotiation, not silently stalling claims.
		if cfg.DispatcherEnabled {
			// Gate 4: wire the REAL persistence boundary (repo.PersistResult
			// fence-completes the job atomically in its single TX). The
			// errPersister default would fail every completion.
			// Runner identity comes pre-derived from config (#122: explicit
			// name, else the processor URL host).
			disp := dispatcher.NewWithPersister(rep, ingestClient, rep, dispatcher.Config{
				WorkerID:               cfg.DispatcherWorkerID,
				RunnerName:             cfg.ProcessorRunnerName,
				Concurrency:            cfg.DispatcherConcurrency,
				Profile:                json.RawMessage(cfg.DispatcherProfile),
				LeaseDuration:          cfg.DispatcherLeaseDuration,
				ArtifactRoot:           cfg.ArtifactRoot,
				OpenSearchURL:          cfg.OpenSearchURL,
				OpenSearchUsername:     cfg.OpenSearchUsername,
				OpenSearchPassword:     cfg.OpenSearchPassword,
				ProcessorSourceBaseURL: cfg.ProcessorSourceBaseURL,
				ProcessorSourceSecret:  cfg.ProcessorSourceSecret,
			}, logger)
			// #214: a fatal dispatcher error (capability negotiation still failing
			// after the startup retry window) must exit the process non-zero so
			// launchd/KeepAlive restarts it. Before the fix the loop died while
			// the process stayed "running" (exit 0) — jobs pending forever with
			// no crash. A graceful shutdown (sigCtx cancelled) returns nil and
			// never lands here, so the select below treats it as a restorable
			// process exit.
			go func() {
				if err := disp.Run(sigCtx); err != nil {
					logger.Printf("dispatcher stopped: %v", err)
					dispErrCh <- err
				}
			}()
		}
	}

	logger.Printf("listening on %s", addr)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-sigCtx.Done():
		logger.Printf("signal received; shutting down")
		stop() // idempotent; release the signal handler
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Fatalf("http server: %v", err)
		}
	case err := <-dispErrCh:
		// #214: the runner stayed unreachable through MaxStartupWait. Exit
		// non-zero (FATAL) so the supervisor restarts the process cleanly when
		// the runner finally boots — before the fix the process survived with a
		// dead claim loop and enqueued jobs stayed pending forever.
		logger.Fatalf("dispatcher fatal: process must restart when the runner is available: %v", err)
	}

	// Cancel a still-debounced consolidation hook BEFORE the drain — the
	// pool closes with the process, a late hook run would only log an error.
	if syncSvc != nil {
		syncSvc.StopConsolidation() // #197
	}
	// Gracefully stop the HTTP server within a bounded window; the dispatcher's
	// own context is the same sigCtx, so it drains and releases its leases in
	// parallel.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("http shutdown: %v", err)
	}
}

// runnerCheck adapts a runner health surface (primary or failover client)
// to the server's Checker interface for /api/health.
type runnerCheckFn struct {
	health func(ctx context.Context) error
}

func (r runnerCheckFn) Ready() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.health(ctx)
}

func runnerCheck(h interface {
	Health(ctx context.Context) error
}) server.Checker {
	return runnerCheckFn{health: h.Health}
}

// probeQueryRunnerRole verifies at startup that the configured query runner
// actually serves the query roles (R4 Ziel 3): capabilities must advertise
// query_embedding and reranking. A capable-but-different runner (e.g. a
// Carrier runner with §7a endpoints) is a valid query runner; a runner
// without them gets a WARNING — search stays up and degrades per R3.
func probeQueryRunnerRole(ctx context.Context, c *processor.Client, url string, logger *log.Logger) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	caps, err := c.Capabilities(probeCtx)
	if err != nil {
		logger.Printf("runner roles: query runner %s not reachable at start (search degrades per R3 until it is): %v", url, err)
		return
	}
	feats := caps.Features
	qe, rk := feats != nil && feats["query_embedding"], feats != nil && feats["reranking"]
	switch {
	case qe && rk:
		logger.Printf("runner roles: query runner %s capable (query_embedding=%v reranking=%v, model=%s)", url, qe, rk, caps.Processor.Name)
	case !qe && !rk:
		logger.Printf("WARNING: runner roles: query runner %s has NEITHER query role (query_embedding/reranking) — retrieval will run degraded (BM25-only, unreranked); point AXIOM_QUERY_RUNNER_URL at a §7a-capable runner", url)
	default:
		logger.Printf("WARNING: runner roles: query runner %s only partially query-capable (query_embedding=%v reranking=%v) — partial R3 degradation expected", url, qe, rk)
	}
}
