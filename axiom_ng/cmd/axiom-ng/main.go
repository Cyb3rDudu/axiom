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
	"syscall"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/dispatcher"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
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
		logger.Printf("WARNING: AXIOMNG_DATABASE_URL not set; running without Postgres")
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

		// The dispatcher is opt-in and runs only when explicitly enabled. It
		// claims jobs, drives them through the processor and back to a terminal
		// state; a broken processor is surfaced on start via capability
		// negotiation, not silently stalling claims.
		if cfg.DispatcherEnabled {
			pclient, perr := processor.New(processor.Options{BaseURL: cfg.ProcessorURL})
			if perr != nil {
				logger.Fatalf("processor client: %v", perr)
			}
			// Gate 4: wire the REAL persistence boundary (repo.PersistResult
			// fence-completes the job atomically in its single TX). The
			// errPersister default would fail every completion.
			disp := dispatcher.NewWithPersister(rep, pclient, rep, dispatcher.Config{
				WorkerID:           cfg.DispatcherWorkerID,
				Concurrency:        cfg.DispatcherConcurrency,
				Profile:            json.RawMessage(cfg.DispatcherProfile),
				LeaseDuration:      cfg.DispatcherLeaseDuration,
				ArtifactRoot:       cfg.ArtifactRoot,
				OpenSearchURL:      cfg.OpenSearchURL,
				OpenSearchUsername: cfg.OpenSearchUsername,
				OpenSearchPassword: cfg.OpenSearchPassword,
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
