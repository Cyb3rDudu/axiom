// migrate_it_test.go — the Store ledger's idempotency witness (review
// R2-5): a second Migrate run against an already-migrated database (the
// ledger row wiped — the bootstrap-repair scenario) must succeed, proving
// the constraint guard agrees with the ADD for both the corrected and the
// original (typo'd) constraint names. Gated: AXIOM_TEST_DATABASE_URL.
package migrations

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
)

func TestStoreMigrateIdempotentAfterLedgerWipe(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping migrations IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %q", dsn)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}

	// First apply (may already be applied by sibling tests — idempotent).
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// A DB ledgered at the FIRST strand build carries the OLD identity
	// index shape (rendition+hash, status-blind): the migration must
	// replace it with the active-scoped three-column shape.
	if _, err := d.Pool().Exec(ctx, `DROP INDEX IF EXISTS ingest_jobs_revision_identity_uq`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `CREATE UNIQUE INDEX ingest_jobs_revision_identity_uq
		ON ingest_jobs (revision_rendition_id, content_hash)
		WHERE intake_kind='revision' AND force_rebuild=false`); err != nil {
		t.Fatal(err)
	}

	// The original (typo'd) constraint name must satisfy the guard too:
	// pre-landing dev DBs carry intake_jobs_intake_kind_chk (and not the
	// corrected name) — simulate exactly that shape.
	for _, name := range []string{"ingest_jobs_intake_kind_chk", "intake_jobs_intake_kind_chk"} {
		if _, err := d.Pool().Exec(ctx, `ALTER TABLE ingest_jobs DROP CONSTRAINT IF EXISTS `+name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool().Exec(ctx, `ALTER TABLE ingest_jobs ADD CONSTRAINT intake_jobs_intake_kind_chk CHECK (intake_kind IN ('zotero','revision'))`); err != nil {
		t.Fatal(err)
	}
	// Simulate the bootstrap repair: the ledger forgets, the schema stays.
	if _, err := d.Pool().Exec(ctx, `DELETE FROM store_schema_migrations`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Migrate(context.Background(), d.Pool()) })
	// The re-run must not error (the pre-fix guard/ADD mismatch failed
	// here with "duplicate constraint").
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("re-run after ledger wipe must be idempotent: %v", err)
	}
	var n int
	if err := d.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='ingest_jobs'::regclass AND conname IN ('ingest_jobs_intake_kind_chk','intake_jobs_intake_kind_chk')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("exactly one intake-kind CHECK expected (typo or corrected), got %d", n)
	}
	// The identity index must now carry the ACTIVE-status, source-scoped
	// predicate (the ON CONFLICT arbiter needs exactly this shape).
	var idxdef string
	if err := d.Pool().QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname='ingest_jobs_revision_identity_uq'`).Scan(&idxdef); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idxdef, "revision_source_id, revision_rendition_id, content_hash") ||
		!strings.Contains(idxdef, "status = ANY") {
		t.Fatalf("identity index not migrated to the active-scoped source shape: %s", idxdef)
	}
}

// TestStoreMigrate0002FixesLedgeredOldIndex — the review-R5 witness: a
// DB ledgered at the FIRST strand build (0001 recorded, 0002 not — it
// carries the old status-blind identity index) must be repaired by a
// plain Migrate run, no ledger wipe: 0002 always runs on such DBs.
func TestStoreMigrate0002FixesLedgeredOldIndex(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping migrations IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %q", dsn)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// Simulate the ledgered-at-first-build state WITH the ledger intact:
	// the old index shape, and 0002's row absent (0001's stays).
	if _, err := d.Pool().Exec(ctx, `DROP INDEX IF EXISTS ingest_jobs_revision_identity_uq`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `CREATE UNIQUE INDEX ingest_jobs_revision_identity_uq
		ON ingest_jobs (revision_rendition_id, content_hash)
		WHERE intake_kind='revision' AND force_rebuild=false`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `DELETE FROM store_schema_migrations WHERE version='0002_revision_identity_active_scope.sql'`); err != nil {
		t.Fatal(err)
	}

	// The plain run must repair: 0002 executes on the ledgered DB.
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("migrate on ledgered DB: %v", err)
	}
	var idxdef string
	if err := d.Pool().QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname='ingest_jobs_revision_identity_uq'`).Scan(&idxdef); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idxdef, "revision_source_id, revision_rendition_id, content_hash") ||
		!strings.Contains(idxdef, "status = ANY") {
		t.Fatalf("0002 must replace the old identity index on a ledgered DB, got: %s", idxdef)
	}
}

// TestStoreMigrate0003DropsExactlyTheFiveCrossFKs — the DM06 #315
// witnesses on an ISOLATED scratch database (sibling ITs share the
// _test DSN and call Migrate at startup; the ledger-wipe and orphan
// injection below must never cross-talk with them):
//  1. exactly-five: the store migration removes exactly the five
//     allowlisted cross-component FKs — full FK inventory diff, nothing
//     else falls, nothing appears;
//  2. postcondition: zero FKs remain from the two Store tables onto the
//     three Library tables;
//  3. orphan-guard teeth: a fake orphan (post-drop, injectable exactly
//     because the FK is gone) makes the re-run ABORT with the reference
//     named, and the ledger row stays absent (transaction rollback);
//  4. bootstrap-repair idempotency: clean re-run after a ledger wipe is
//     a no-op (constraints already gone, guard passes).
func TestStoreMigrate0003DropsExactlyTheFiveCrossFKs(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping migrations IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %q", dsn)
	}
	const v0003 = "0003_drop_cross_component_fks.sql"
	ctx := context.Background()

	// Isolated scratch DB (the baseline fingerprint's fixed-name pattern:
	// fails loudly on concurrent runs instead of cross-talking).
	admin, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	scratch := fmt.Sprintf("axiom_dm06_%d_test", os.Getpid())
	if _, err := admin.Pool().Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, scratch); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Pool().Exec(ctx, "DROP DATABASE IF EXISTS "+scratch)
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		admin.Close()
		t.Fatalf("create scratch: %v", err)
	}
	admin.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + scratch
	scratchDSN := u.String()
	t.Cleanup(func() {
		c, err := db.Open(context.Background(), dsn)
		if err == nil {
			c.Pool().Exec(context.Background(),
				`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, scratch)
			c.Pool().Exec(context.Background(), "DROP DATABASE IF EXISTS "+scratch)
			c.Close()
		}
	})
	d, err := db.Open(ctx, scratchDSN)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}

	fkInventory := func() map[string]string {
		rows, err := d.Pool().Query(ctx, `
			SELECT conrelid::regclass::text, conname
			FROM pg_constraint WHERE contype='f'
			ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var tbl, name string
			if err := rows.Scan(&tbl, &name); err != nil {
				t.Fatal(err)
			}
			out[tbl+"."+name] = ""
		}
		return out
	}
	dropped := []string{
		"ingest_jobs.fk_ingest_jobs_source",
		"ingest_jobs.fk_ingest_jobs_document",
		"ingest_jobs.fk_ingest_jobs_attachment",
		"processing_snapshots.processing_snapshots_document_id_fkey",
		"processing_snapshots.processing_snapshots_attachment_id_fkey",
	}

	// (1) exactly five — full inventory diff.
	before := fkInventory()
	for _, fk := range dropped {
		if _, ok := before[fk]; !ok {
			t.Fatalf("precondition failed: %s absent before the store migrate (core schema drifted?)", fk)
		}
	}
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("store migrate: %v", err)
	}
	after := fkInventory()
	var fell []string
	for fk := range before {
		if _, ok := after[fk]; !ok {
			fell = append(fell, fk)
		}
	}
	sort.Strings(fell)
	sort.Strings(dropped)
	if len(fell) != len(dropped) {
		t.Fatalf("exactly five FKs must fall, got %d: %v", len(fell), fell)
	}
	for i := range fell {
		if fell[i] != dropped[i] {
			t.Fatalf("the five dropped FKs drifted: got %v", fell)
		}
	}
	for fk := range after {
		if _, ok := before[fk]; !ok {
			t.Fatalf("the store migration must not ADD constraints, found new %s", fk)
		}
	}

	// (2) postcondition: no Store→Library FK remains.
	var n int
	if err := d.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE contype='f'
		  AND conrelid IN ('ingest_jobs'::regclass, 'processing_snapshots'::regclass)
		  AND confrelid IN ('zotero_sources'::regclass, 'zotero_documents'::regclass, 'zotero_attachments'::regclass)`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cross-component FKs remain after the drop: %d", n)
	}

	// (3) orphan-guard teeth: inject a fake orphan (possible exactly
	// BECAUSE the FK is gone), wipe 0003's ledger row, re-run — the
	// guard must abort naming the reference, and the aborted
	// transaction must leave no ledger row.
	if _, err := d.Pool().Exec(ctx, `DELETE FROM store_schema_migrations WHERE version=$1`, v0003); err != nil {
		t.Fatal(err)
	}
	var orphanID string
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO ingest_jobs (source_id) VALUES (gen_random_uuid()) RETURNING id::text`).Scan(&orphanID); err != nil {
		t.Fatalf("inject fake orphan: %v", err)
	}
	err = Migrate(ctx, d.Pool())
	if err == nil || !strings.Contains(err.Error(), "orphan guard") || !strings.Contains(err.Error(), "ingest_jobs.source_id") {
		t.Fatalf("orphan guard must abort naming the broken reference, got: %v", err)
	}
	var ledgered bool
	if err := d.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM store_schema_migrations WHERE version=$1)`, v0003).Scan(&ledgered); err != nil {
		t.Fatal(err)
	}
	if ledgered {
		t.Fatal("the aborted 0003 must not be ledgered (transaction rollback)")
	}

	// (4) bootstrap-repair no-op: clean data, wiped ledger row — the
	// re-run succeeds, drops nothing, restores the ledger row.
	if _, err := d.Pool().Exec(ctx, `DELETE FROM ingest_jobs WHERE id=$1`, orphanID); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("re-run after ledger wipe must be a no-op: %v", err)
	}
	if err := d.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM store_schema_migrations WHERE version=$1)`, v0003).Scan(&ledgered); err != nil {
		t.Fatal(err)
	}
	if !ledgered {
		t.Fatal("0003 must be ledgered after the clean re-run")
	}
	if got := fkInventory(); len(got) != len(after) {
		t.Fatalf("the no-op re-run changed the FK inventory: %d → %d", len(after), len(got))
	}
}
