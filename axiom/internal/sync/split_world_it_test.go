// split_world_it_test.go — the #358 two-database witness: the sync runs
// the mirror apply on the LIBRARY database and the store-effect phase on
// the STORE database — two separate pools, two separate databases, zero
// cross-database SQL. The mirror rows exist ONLY on the library side;
// the projection + revision intake ONLY on the store side; the claim
// resolves through the projection alone.
package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/mirror"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/pglib"
	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
	storemigrations "github.com/Cyb3rDudu/axiom/axiom/internal/store/migrations"
	"github.com/Cyb3rDudu/axiom/axiom/internal/zoteroprovider"
	"github.com/jackc/pgx/v5/pgxpool"
)

func splitWorldDSN(t *testing.T, name string) string {
	t.Helper()
	base := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping split-world IT")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx := context.Background()
	_, _ = admin.Exec(ctx, fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`), name)
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, err := pgxpool.New(context.Background(), u.String())
		if err == nil {
			_, _ = c.Exec(context.Background(), fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`), name)
			_, _ = c.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name)
			c.Close()
		}
	})
	nu := *u
	nu.Path = "/" + name
	return nu.String()
}

// TestSyncSplitWorldIT — the DoD's architecture probe.
func TestSyncSplitWorldIT(t *testing.T) {
	ctx := context.Background()
	libDSN := splitWorldDSN(t, fmt.Sprintf("axiom_sw_lib_%d_test", os.Getpid()))
	storeDSN := splitWorldDSN(t, fmt.Sprintf("axiom_sw_store_%d_test", os.Getpid()))

	// The LIBRARY database: library migrations (which carry the Zotero
	// mirror schema since #358). NO core schema, NO store tables.
	libDB, err := db.Open(ctx, libDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(libDB.Close)
	if err := pglib.Migrate(ctx, libDB.Pool()); err != nil {
		t.Fatalf("library migrate: %v", err)
	}

	// The STORE database: core schema + the store ledger (store_documents).
	storeDB, err := db.Open(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storeDB.Close)
	if err := storeDB.Migrate(ctx); err != nil {
		t.Fatalf("store migrate: %v", err)
	}
	if err := storemigrations.Migrate(ctx, storeDB.Pool()); err != nil {
		t.Fatalf("store ledger migrate: %v", err)
	}

	pdf := makePdf(t, "splitworld")
	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 3}
	src.items = []zoteroprovider.CanonicalItem{
		mkItemJSON("SWDOC1", "book", "", "Split World Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
		}),
		mkItemJSON("SWATT1", "attachment", "SWDOC1", "sw.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "sw.pdf",
		}),
	}
	env, _ := json.Marshal(map[string]any{
		"key": "SWATT1", "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdf}},
		"data":  map[string]any{"key": "SWATT1", "version": 1, "itemType": "attachment", "parentItem": "SWDOC1", "contentType": "application/pdf", "filename": "sw.pdf"},
	})
	src.items[1].Envelope = env

	rep := repo.New(storeDB.Pool())
	svc := New(src, mirror.New(libDB.Pool()), rep, src.baseURL, "users/0", log.Default())

	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("split-world sync: %v", err)
	}
	if res.Enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", res.Enqueued)
	}

	// Mirror rows live ONLY in the library database.
	var libMirror int
	if err := libDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM zotero_documents WHERE zotero_key='SWDOC1'`).Scan(&libMirror); err != nil {
		t.Fatalf("library mirror read: %v", err)
	}
	if libMirror != 1 {
		t.Fatalf("library mirror rows = %d, want 1", libMirror)
	}

	// The store database has NO mirror truth of its own — the archive
	// tables stay empty; the projection + intake are the store's rows.
	var storeMirror int
	if err := storeDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM zotero_documents`).Scan(&storeMirror); err != nil {
		t.Fatal(err)
	}
	if storeMirror != 0 {
		t.Fatalf("store database carries %d mirror rows — the mirror must be library-side only", storeMirror)
	}
	var proj int
	if err := storeDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM store_documents WHERE rendition_key='SWATT1' AND title='Split World Book'`).Scan(&proj); err != nil {
		t.Fatal(err)
	}
	if proj != 1 {
		t.Fatalf("store projection rows = %d, want 1 (title from the mirror bibliography)", proj)
	}

	// The claim resolves through the projection — no mirror read.
	cj, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{
		WorkerID: "split-world", LeaseDuration: 30 * time.Second,
		Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
	})
	if err != nil || cj == nil {
		t.Fatalf("claim through the projection: %v %v", cj, err)
	}
	if cj.DocumentID == "" || cj.AttachmentID == "" {
		t.Fatalf("claim must resolve the durable identities, got doc=%q att=%q", cj.DocumentID, cj.AttachmentID)
	}
}
