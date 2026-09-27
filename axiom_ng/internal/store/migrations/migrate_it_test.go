// migrate_it_test.go — the Store ledger's idempotency witness (review
// R2-5): a second Migrate run against an already-migrated database (the
// ledger row wiped — the bootstrap-repair scenario) must succeed, proving
// the constraint guard agrees with the ADD for both the corrected and the
// original (typo'd) constraint names. Gated: AXIOM_TEST_DATABASE_URL.
package migrations

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
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
