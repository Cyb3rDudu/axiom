package db

import (
	"context"
	"os"
	"testing"
)

// testDSN returns the integration-test Postgres DSN, or "" if not configured.
// Tests skip when no DSN is available so `go test ./...` works without a DB.
func testDSN() string {
	return os.Getenv("AXIOM_TEST_DATABASE_URL")
}

func TestMigrateCreatesIngestJobs(t *testing.T) {
	dsn := testDSN()
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	d, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var exists bool
	err = d.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'ingest_jobs'
		)`).Scan(&exists)
	if err != nil {
		t.Fatalf("check table: %v", err)
	}
	if !exists {
		t.Fatal("ingest_jobs table was not created by migration")
	}

	for _, table := range []string{"zotero_sources", "zotero_documents", "zotero_attachments",
		"zotero_items", "zotero_collections", "zotero_item_collections",
		// #184 repair foundation: state machine + audit tables must exist.
		"repair_cases", "zotero_write_audit"} {
		if err := d.pool.QueryRow(ctx,
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.tables WHERE table_name = $1
			)`, table).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("%s table was not created by migration", table)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	dsn := testDSN()
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	d, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	// Running migrate twice must succeed (each migration runs once).
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate (1st): %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate (2nd): %v", err)
	}
}

// TestRepairCasesOneOpenConstraint pins the #184 partial unique index: two
// OPEN repair_cases (rejected/queued/in_repair) for one attachment must
// violate repair_cases_one_open_per_attachment — the unique re-entry point
// of the state machine. Closed cases never collide with new ones.
func TestRepairCasesOneOpenConstraint(t *testing.T) {
	dsn := testDSN()
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	d, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Minimal attachment fixture (source → document → attachment).
	var attID string
	err = d.pool.QueryRow(ctx, `
		WITH src AS (
			INSERT INTO zotero_sources (base_url, library_id, server_id)
			VALUES ('https://dbit.example', 'lib-constraint', 'srv') RETURNING id
		), doc AS (
			INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
			SELECT id, 'CONSTDOC', 1, 'book', 'Constraint Doc' FROM src RETURNING id
		)
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path, preferred, deleted)
		SELECT src.id, doc.id, 'CONSTATT', 1, 'CONSTDOC', 'imported_file',
			'application/pdf', 'c.pdf', '/tmp/c.pdf', false, false
		FROM src, doc RETURNING id`).Scan(&attID)
	if err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	open := func(status string) error {
		_, err := d.pool.Exec(ctx, `
			INSERT INTO repair_cases (attachment_id, status) VALUES ($1, $2)`, attID, status)
		return err
	}
	if err := open("rejected"); err != nil {
		t.Fatalf("first open case: %v", err)
	}
	for _, status := range []string{"rejected", "queued", "in_repair"} {
		if err := open(status); err == nil {
			t.Fatalf("second OPEN case (%s) must violate the partial unique index", status)
		}
	}
	// A CLOSED case coexists with an open one: the guard is only on open states.
	if _, err := d.pool.Exec(ctx, `
		UPDATE repair_cases SET status='healed' WHERE attachment_id=$1`, attID); err != nil {
		t.Fatal(err)
	}
	if err := open("rejected"); err != nil {
		t.Fatalf("closed case must not block a fresh open one: %v", err)
	}
}
