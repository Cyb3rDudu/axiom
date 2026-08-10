// Command axiom-ng runs the Zotero RAG sidecar. It orchestrates indexing of a
// Zotero library and exposes the REST API for retrieval and chat. It runs on
// the same host as Zotero and resolves attachments to local files.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/server"
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

	addr := ":" + strconv.Itoa(cfg.APIPort)
	srv := server.New(addr, logger)
	srv.RegisterCheck("zotero", server.CheckZotero(src))
	if database != nil {
		srv.RegisterCheck("postgres", server.CheckDB(database.Pool()))
	}
	logger.Printf("listening on %s", addr)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		logger.Fatalf("http server: %v", err)
	}
}
