// harness_test.go — the mirror IT harness (F09 #303): a trimmed copy of
// the repo lease harness (scratch *_test database, migrate, seed the
// Zotero mirror). The duplication is deliberate and bounded: the repo
// harness lives with the Store-owned persistence tests and stays there
// until F12 dissolves the split; these tests moved out with the mirror
// extraction and need the same scratch-DB discipline.
package mirror

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mirrorTestDatabaseName is the persistent mirror IT database (created from
// the AXIOM_TEST_DATABASE_URL host/credentials; truncated between tests).
const mirrorTestDatabaseName = "axiom_ng_mirror_it_test"

func mirrorTestDSN() string {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	u.Path = "/" + mirrorTestDatabaseName
	return u.String()
}

func requireMirrorTestDB(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	if !strings.HasSuffix(u.Path, "_test") {
		t.Fatalf("REFUSING to run against non-_test database %q", u.Path)
	}
}

func swapMirrorDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

func ensureMirrorDatabase(t *testing.T) string {
	t.Helper()
	base := mirrorTestDSN()
	if base == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping mirror integration test")
	}
	requireMirrorTestDB(t, base)
	maintain := swapMirrorDatabase(base, "postgres")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, maintain)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, mirrorTestDatabaseName).Scan(&exists); err != nil {
		t.Fatalf("check db exists: %v", err)
	}
	if !exists {
		if _, err := pool.Exec(ctx, `CREATE DATABASE `+mirrorTestDatabaseName); err != nil {
			t.Fatalf("create db: %v", err)
		}
	}
	return base
}

// mirrorRepo wraps the mirror Repo plus its pool and the store repo the
// legacy apply lane delegates its job effects to.
type mirrorRepo struct {
	pool  *pgxpool.Pool
	rep   *Repo
	store *repo.Repo
}

func openMirrorDB(t *testing.T) *mirrorRepo {
	t.Helper()
	dsn := ensureMirrorDatabase(t)
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(d.Close)
	store := repo.New(d.Pool())
	return &mirrorRepo{pool: d.Pool(), rep: New(store), store: store}
}

// truncateFixtures clears the mirror tables under test (plus the store
// tables the legacy apply lane touches), verifying the truncate session's
// database ends in _test first.
func (mr *mirrorRepo) truncateFixtures(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var dbName string
	if err := mr.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("read current_database: %v", err)
	}
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("REFUSING to truncate: current_database %q does not end in _test", dbName)
	}
	if _, err := mr.pool.Exec(ctx, `
		TRUNCATE ingest_jobs, zotero_attachments, zotero_documents, zotero_items,
		         zotero_item_collections, zotero_collections, zotero_sources,
		         zotero_selections, zotero_collection_selections, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate fixtures: %v", err)
	}
}

type mirrorSeedSpec struct {
	sourceBaseURL string
	libraryID     string
	docKey        string
	attKey        string
	contentHash   *string
	preferred     bool
	deleted       bool
}

// seed inserts a source, document, canonical item, one attachment and a
// job row per spec, returning the fixture ids (same shape as the repo
// harness seed the moved tests were written against).
func (mr *mirrorRepo) seed(t *testing.T, spec mirrorSeedSpec, jobStatus string, maxAttempts int) (attachmentID, jobID string) {
	t.Helper()
	ctx := context.Background()
	var srcID, docID string
	if err := mr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ($1, $2, 'test-server') RETURNING id::text`, spec.sourceBaseURL, spec.libraryID).Scan(&srcID); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if err := mr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, $2, 1, 'book', 'Test Doc') RETURNING id::text`, srcID, spec.docKey).Scan(&docID); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	var itemID string
	if err := mr.pool.QueryRow(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1, $2, 1, 'book', NULL, $3, $4) RETURNING id::text`,
		srcID, spec.docKey,
		`{"key":"`+spec.docKey+`"}`,
		`{"key":"`+spec.docKey+`","version":1,"itemType":"book","title":"Test Doc"}`,
	).Scan(&itemID); err != nil {
		t.Fatalf("insert canonical item: %v", err)
	}
	if _, err := mr.pool.Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docID, itemID); err != nil {
		t.Fatalf("link canonical item: %v", err)
	}
	if err := mr.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path,
			content_hash, preferred, deleted)
		VALUES ($1, $2, $3, 1, $4, 'imported_file', 'application/pdf',
			'x.pdf', '/tmp/x.pdf', $5, $6, $7) RETURNING id::text`,
		srcID, docID, spec.attKey, spec.docKey, spec.contentHash, spec.preferred, spec.deleted).Scan(&attachmentID); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	if err := mr.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
		srcID, docID, attachmentID, spec.contentHash, jobStatus, maxAttempts).Scan(&jobID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return attachmentID, jobID
}
