package db

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
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

	// Minimal attachment fixture (source → document → attachment). Run-unique
	// keys: this test is idempotent across runs against the same test DB
	// (review W — fixed keys collided on every second run).
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var attID string
	err = d.pool.QueryRow(ctx, `
		WITH src AS (
			INSERT INTO zotero_sources (base_url, library_id, server_id)
			VALUES ('https://dbit.example', 'lib-c-' || $1, 'srv') RETURNING id
		), doc AS (
			INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
			SELECT id, 'DOCC' || $1, 1, 'book', 'Constraint Doc' FROM src RETURNING id
		)
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path, preferred, deleted)
		SELECT src.id, doc.id, 'ATTC' || $1, 1, 'DOCC' || $1, 'imported_file',
			'application/pdf', 'c.pdf', '/tmp/c.pdf', false, false
		FROM src, doc RETURNING id`, suffix).Scan(&attID)
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

// TestMigrateQualityStateEnglishKeys pins #219 migration 0020: stored
// quality_state / repair-case analysis rows carrying the German keys
// (verdacht/grund) are renamed in place to the canonical English names
// (finding/reason). Idempotent on re-migration; unknown keys and rows
// without the German keys stay untouched.
func TestMigrateQualityStateEnglishKeys(t *testing.T) {
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

	var jobID string
	german := `{"verdict":"fail","verdacht":"🔴 unpaginiert","grund":"kein Tier-1","pages":12,"text_layer":true}`
	if err := d.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (quality_state) VALUES ($1::jsonb) RETURNING id::text`,
		german).Scan(&jobID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	var caseID string
	if err := d.pool.QueryRow(ctx, `
		INSERT INTO repair_cases (status, analysis) VALUES ('rejected', $1::jsonb) RETURNING id::text`,
		german).Scan(&caseID); err != nil {
		t.Fatalf("seed repair case: %v", err)
	}

	// Re-running Migrate re-executes nothing (already recorded), so exercise
	// the REAL 0020 artifact from the embedded schema FS — reverting or
	// gutting the SQL file must fail this test, not a hand-copied string.
	mig, err := schemaFS.ReadFile("schema/0020_quality_state_english.sql")
	if err != nil {
		t.Fatalf("read 0020 artifact: %v", err)
	}
	if _, err := d.pool.Exec(ctx, string(mig)); err != nil {
		t.Fatalf("apply 0020: %v", err)
	}

	assertRenamed := func(got []byte, where string) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("json %s (%s): %v", got, where, err)
		}
		if m["verdacht"] != nil || m["grund"] != nil {
			t.Fatalf("%s: German keys must be gone: %s", where, got)
		}
		if m["finding"] != "🔴 unpaginiert" || m["reason"] != "kein Tier-1" {
			t.Fatalf("%s: English keys must carry the values: %s", where, got)
		}
		// unknown keys preserved
		if m["pages"] != float64(12) || m["text_layer"] != true {
			t.Fatalf("%s: unknown keys must be preserved: %s", where, got)
		}
	}

	var qs, an []byte
	if err := d.pool.QueryRow(ctx,
		`SELECT quality_state FROM ingest_jobs WHERE id=$1`, jobID).Scan(&qs); err != nil {
		t.Fatalf("read job quality_state: %v", err)
	}
	assertRenamed(qs, "ingest_jobs.quality_state")
	if err := d.pool.QueryRow(ctx,
		`SELECT analysis FROM repair_cases WHERE id=$1::uuid`, caseID).Scan(&an); err != nil {
		t.Fatalf("read repair analysis: %v", err)
	}
	assertRenamed(an, "repair_cases.analysis")

	// Idempotence: applying the real artifact again changes nothing on
	// EITHER column.
	if _, err := d.pool.Exec(ctx, string(mig)); err != nil {
		t.Fatalf("re-apply 0020: %v", err)
	}
	var qs2, an2 []byte
	if err := d.pool.QueryRow(ctx,
		`SELECT quality_state FROM ingest_jobs WHERE id=$1`, jobID).Scan(&qs2); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx,
		`SELECT analysis FROM repair_cases WHERE id=$1::uuid`, caseID).Scan(&an2); err != nil {
		t.Fatal(err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(qs2, &m2); err != nil {
		t.Fatal(err)
	}
	if m2["finding"] != "🔴 unpaginiert" || m2["reason"] != "kein Tier-1" || len(m2) != 5 {
		t.Fatalf("second run must be a no-op (ingest_jobs): %s", qs2)
	}
	var m3 map[string]any
	if err := json.Unmarshal(an2, &m3); err != nil {
		t.Fatal(err)
	}
	if m3["finding"] != "🔴 unpaginiert" || m3["reason"] != "kein Tier-1" || len(m3) != 5 {
		t.Fatalf("second run must be a no-op (repair_cases): %s", an2)
	}
}
