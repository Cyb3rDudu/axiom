package repair

// harness_test.go — the store IT harness (moved with the state machine
// from internal/repo, F08 #302). Owns a DEDICATED per-package test
// database (axiom_ng_repair_test) derived from AXIOM_TEST_DATABASE_URL —
// same isolation rule as the repo/dispatcher suites: parallel `go test
// ./...` packages never share a dataset. Every destructive statement is
// behind the _test-suffix guard.

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const repairTestDatabaseName = "axiom_ng_repair_test"

type storeEnv struct {
	pool  *pgxpool.Pool
	store *Store
}

func openStoreDB(t *testing.T) *storeEnv {
	t.Helper()
	base := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping repair store integration test")
	}
	if name := dsnDatabaseName(base); !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to run against non-test database %q (must end in _test)", name)
	}
	ctx := context.Background()
	repairDSN := swapDatabase(base, repairTestDatabaseName)
	maintDSN := swapDatabase(base, "postgres")
	mp, err := pgxpool.New(ctx, maintDSN)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	defer mp.Close()
	var exists bool
	if err := mp.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, repairTestDatabaseName).Scan(&exists); err != nil {
		t.Fatalf("check db exists: %v", err)
	}
	if !exists {
		if _, err := mp.Exec(ctx, `CREATE DATABASE `+repairTestDatabaseName); err != nil {
			t.Fatalf("create %s: %v", repairTestDatabaseName, err)
		}
	}
	d, err := db.Open(ctx, repairDSN)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(d.Close)
	return &storeEnv{pool: d.Pool(), store: NewStore(d.Pool())}
}

func dsnDatabaseName(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
			if k := strings.Index(rest, "?"); k >= 0 {
				rest = rest[:k]
			}
			return rest
		}
	}
	return ""
}

func swapDatabase(dsn, database string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + database
	return u.String()
}

// truncateFixtures clears the tables under test (repair_cases and
// zotero_write_audit fall via CASCADE). The _test-suffix guard runs
// against the ACTUAL database of this session immediately before the
// TRUNCATE — a mis-wired pool can never wipe a non-test database.
func (e *storeEnv) truncateFixtures(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var dbName string
	if err := e.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("read current_database: %v", err)
	}
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("REFUSING to truncate: current_database %q does not end in _test", dbName)
	}
	if _, err := e.pool.Exec(ctx, `
		TRUNCATE kg_superseded_entities,
		         ingest_jobs, zotero_attachments, zotero_documents, zotero_items,
		         zotero_item_collections, zotero_collections, zotero_sources,
		         zotero_selections, zotero_collection_selections
		CASCADE`); err != nil {
		t.Fatalf("truncate fixtures: %v", err)
	}
}

type seedSpec struct {
	sourceBaseURL string
	libraryID     string
	docKey        string
	attKey        string
	contentHash   *string
	preferred     bool
	deleted       bool
}

// seed inserts a source, document and one attachment per spec and returns
// the fixture ids (SQL moved verbatim from the repo lease harness).
func (e *storeEnv) seed(t *testing.T, spec seedSpec, jobStatus string, maxAttempts int) (attachmentID, jobID string) {
	t.Helper()
	ctx := context.Background()
	var srcID, docID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ($1, $2, 'test-server') RETURNING id::text`, spec.sourceBaseURL, spec.libraryID).Scan(&srcID); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, $2, 1, 'book', 'Test Doc') RETURNING id::text`, srcID, spec.docKey).Scan(&docID); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	// A canonical zotero_items row for the document (Zotero is the source of
	// truth for metadata).
	var itemID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1, $2, 1, 'book', NULL, $3, $4) RETURNING id::text`,
		srcID, spec.docKey,
		`{"key":"`+spec.docKey+`"}`,
		`{"key":"`+spec.docKey+`","version":1,"itemType":"book","title":"Test Doc"}`,
	).Scan(&itemID); err != nil {
		t.Fatalf("insert canonical item: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docID, itemID); err != nil {
		t.Fatalf("link canonical item: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path,
			content_hash, preferred, deleted)
		VALUES ($1, $2, $3, 1, $4, 'imported_file', 'application/pdf',
			'x.pdf', '/tmp/x.pdf', $5, $6, $7) RETURNING id::text`,
		srcID, docID, spec.attKey, spec.docKey, spec.contentHash, spec.preferred, spec.deleted).Scan(&attachmentID); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
		srcID, docID, attachmentID, spec.contentHash, jobStatus, maxAttempts).Scan(&jobID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return attachmentID, jobID
}
