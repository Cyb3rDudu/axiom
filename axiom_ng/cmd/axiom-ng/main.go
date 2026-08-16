// Command axiom-ng runs the Zotero RAG sidecar. It orchestrates indexing of a
// Zotero library and exposes the REST API for retrieval and chat. It runs on
// the same host as Zotero and resolves attachments to local files.
package main

import (
	"context"
	"encoding/json"
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
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

func main() {
	ctx := context.Background()
	cfg := config.Load()
	logger := log.New(os.Stderr, "axiom-ng: ", log.LstdFlags)

	src := zotero.NewLocalAPI(cfg.ZoteroBaseURL, cfg.ZoteroLibraryID)
	if id := src.ServerID(); id == "" {
		logger.Printf("WARNING: Zotero local API not reachable at %s (is Zotero running and the local API enabled?)", cfg.ZoteroBaseURL)
	} else {
		logger.Printf("Zotero local API reachable: server-id=%s", id)
	}

	var database *db.DB
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

	if database != nil {
		srv.RegisterCheck("postgres", server.CheckDB(database.Pool()))
		// Wire the sync service and ingest-job listing into the API.
		rep := repo.New(database.Pool())
		syncSvc := sync.New(src, rep, cfg.ZoteroBaseURL, cfg.ZoteroLibraryID, logger)
		srv.SetSyncAPI(syncSvc)
		srv.SetJobRepo(rep)
		// Remote source delivery: same secret on both sides (endpoint verify,
		// dispatcher sign). Empty secret disables the endpoint (404 on all).
		srv.SetProcessorSourceSecret(cfg.ProcessorSourceSecret)
		srv.SetProcessorSourceRepo(rep)

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
		// R6 (#136): knowledge-graph read API over the L6 data.
		srv.SetKGService(rep)
		// Role probe (R4 Ziel 1/3): capability check of the query runner at
		// start — verifies query_embedding/reranking and logs the role map.
		// Best-effort: an unreachable query runner keeps search degraded-but-
		// up (R3 fallback), it must not kill the sidecar.
		go probeQueryRunnerRole(sigCtx, queryClient, cfg.QueryRunnerURL, logger)
		srv.RegisterCheck("query-runner", runnerCheck(queryClient))

		// Ingest chain (R4 #134): primary = AXIOM_PROCESSOR_URL (best
		// available), fallback = AXIOM_INGEST_FALLBACK_URL (default local).
		// The failover client satisfies the same dispatcher interface; without
		// a distinct fallback URL it is a plain primary client.
		newIngestClient := func() (*processor.FailoverClient, error) {
			primary, err := processor.New(processor.Options{
				BaseURL:       cfg.ProcessorURL,
				ResultTimeout: cfg.ProcessorRequestTimeout,
			})
			if err != nil {
				return nil, err
			}
			// Normalize trailing slashes so "http://host:8012/" does not build
			// a failover chain onto the same URL as "http://host:8012".
			sameURL := strings.TrimRight(cfg.IngestFallbackURL, "/") == strings.TrimRight(cfg.ProcessorURL, "/")
			if cfg.IngestFallbackURL == "" || sameURL {
				return processor.NewFailover(primary, nil, logger), nil
			}
			fb, err := processor.New(processor.Options{
				BaseURL: cfg.IngestFallbackURL,
			})
			if err != nil {
				return nil, err
			}
			return processor.NewFailover(primary, fb, logger), nil
		}
		ingestClient, ierr := newIngestClient()
		if ierr != nil {
			logger.Fatalf("ingest client: %v", ierr)
		}
		srv.RegisterCheck("ingest-runner", runnerCheck(ingestClient))
		logger.Printf("runner roles: query=%s ingest=%s (fallback=%s)",
			cfg.QueryRunnerURL, cfg.ProcessorURL, cfg.IngestFallbackURL)

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
			go func() {
				if err := disp.Run(sigCtx); err != nil {
					logger.Printf("dispatcher stopped: %v", err)
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
