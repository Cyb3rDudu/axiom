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
}
