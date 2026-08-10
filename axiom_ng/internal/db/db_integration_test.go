package db

import (
	"context"
	"os"
	"testing"
)

// testDSN returns the integration-test Postgres DSN, or "" if not configured.
// Tests skip when no DSN is available so `go test ./...` works without a DB.
func testDSN() string {
	return os.Getenv("AXIOMNG_TEST_DATABASE_URL")
}

func TestMigrateCreatesIngestJobs(t *testing.T) {
	dsn := testDSN()
	if dsn == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping integration test")
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

	for _, table := range []string{"zotero_sources", "zotero_documents", "zotero_attachments"} {
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
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping integration test")
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
