// migratecmd_it_test.go — the #362 DoD probes against real PostgreSQL
// (AXIOM_TEST_DATABASE_URL-gated, fresh scratch database — never a
// non-_test DSN):
//
//   - fresh scratch: every component pending → applied, exit 0, one
//     before → after ledger line per component;
//   - the v0.2.3 rollout shape: exactly TWO component migrations
//     pending (store 0004 + library 0004, ledger rows wiped — the
//     bootstrap-repair convention; both files are re-apply-idempotent)
//     → both applied, report names them;
//   - idempotent re-run: nothing pending → "up to date", exit 0.
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i362ScratchDB creates a disposable scratch database (the freshScratch
// convention, drill-local) and returns its DSN plus a cleanup.
func i362ScratchDB(t *testing.T) (string, func()) {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping migrate IT")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	if !strings.HasSuffix(cfg.Database, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", cfg.Database)
	}
	dbName := fmt.Sprintf("axiom_i362_%d_test", os.Getpid())
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName); err != nil {
		t.Fatalf("drop old scratch: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	admin.Close()
	sslmode := "prefer"
	if cfg.TLSConfig == nil {
		sslmode = "disable"
	}
	scratch := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		cfg.Host, cfg.Port, dbName, cfg.User, cfg.Password, sslmode)
	return scratch, func() {
		if p, err := pgxpool.New(ctx, dsn); err == nil {
			_, _ = p.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName)
			p.Close()
		}
	}
}

func TestIT_MigrateAppliesPendingWithLedgerReport(t *testing.T) {
	scratch, cleanup := i362ScratchDB(t)
	defer cleanup()
	t.Setenv("AXIOM_STORE_DATABASE_URL", scratch) // canonical spelling
	t.Setenv("AXIOM_DATABASE_URL", "")
	t.Setenv("AXIOM_LIBRARY_DATABASE_URL", "") // single-database shape
	ctx := context.Background()

	// (1) fresh scratch: everything pending → applied, exit 0, one
	// ledger line per component.
	var errBuf strings.Builder
	exit, out := runTo(&errBuf, []string{"migrate"})
	if exit != exitOK {
		t.Fatalf("migrate on fresh scratch exit %d (err %s)", exit, errBuf.String())
	}
	for _, comp := range []string{"core:", "store:", "library:"} {
		if !strings.Contains(out, comp) {
			t.Fatalf("report must carry the %s component line, got %q (err %s)", comp, out, errBuf.String())
		}
	}
	if !strings.Contains(out, "(none) -> ") {
		t.Fatalf("fresh scratch must report the empty ledgers, got %q", out)
	}

	// (2) the v0.2.3 rollout shape: exactly TWO pending component
	// migrations (the newest store + library release migrations, ledger
	// rows wiped — both files re-apply idempotently). NOTE the ledger
	// spellings: core/library record the glob path (schema/NNNN_*.sql),
	// the store ledger records the bare file name.
	pool, err := pgxpool.New(ctx, scratch)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	defer pool.Close()
	for _, wipe := range []string{
		`DELETE FROM store_schema_migrations WHERE version = '0004_store_documents.sql'`,
		`DELETE FROM library_schema_migrations WHERE version = 'schema/0004_zotero_mirror.sql'`,
	} {
		if _, err := pool.Exec(ctx, wipe); err != nil {
			t.Fatalf("ledger wipe %q: %v", wipe, err)
		}
	}
	errBuf.Reset()
	exit, out = runTo(&errBuf, []string{"migrate"})
	if exit != exitOK {
		t.Fatalf("migrate with two pending exit %d (err %s)", exit, errBuf.String())
	}
	if want := "store: 0003_drop_cross_component_fks.sql -> 0004_store_documents.sql (+1"; !strings.Contains(out, want) {
		t.Fatalf("store report line must name the before → after ledger state, got %q", out)
	}
	if want := "library: schema/0003_library_collection_anchor.sql -> schema/0004_zotero_mirror.sql (+1"; !strings.Contains(out, want) {
		t.Fatalf("library report line must name the before → after ledger state, got %q", out)
	}
	if !strings.Contains(out, "core: up to date (") {
		t.Fatalf("core must report up to date, got %q", out)
	}

	// (3) the idempotency sonde: nothing pending anywhere → exit 0,
	// "up to date", no applied arrows.
	errBuf.Reset()
	exit, out = runTo(&errBuf, []string{"migrate"})
	if exit != exitOK {
		t.Fatalf("idempotent re-run exit %d (err %s)", exit, errBuf.String())
	}
	if !strings.Contains(out, "up to date") || strings.Contains(out, " -> ") {
		t.Fatalf("re-run with nothing pending must report up to date and apply nothing, got %q", out)
	}
}
