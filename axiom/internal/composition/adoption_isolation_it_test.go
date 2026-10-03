// adoption_isolation_it_test.go — the F12 #306 adoption and isolation
// witnesses against real PostgreSQL (scratch DBs):
//
//   - VerifyAdoption: fresh database adoptable; ledgered database
//     adoptable; tables-without-complete-ledger REFUSED (no silent
//     adoption of a drifted Bestands state — the read-only check the DM
//     track's real cutover builds on; DM09 stays out of here);
//   - migration isolation: the Library engine's migrations create NO
//     Store ledger/namespace, the Store engine's migrations create NO
//     Library ledger/namespace — the component split is structural, not
//     conventional (the SQLite side is pinned in the sqlite package:
//     one file, only library_* tables).
package composition

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/pglib"
	"github.com/Cyb3rDudu/axiom/axiom/internal/store/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// freshScratch provisions a session-unique EMPTY scratch database (the
// cli-doctor convention: refuses non-_test base DSNs, dropped in
// cleanup). No migrations — the caller applies exactly the component
// sets under test.
func freshScratch(t *testing.T, suffix string) (*pgxpool.Pool, string, func()) {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping persistence IT")
	}
	base := dsn
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.Index(base, "?"); i >= 0 {
		base = base[:i]
	}
	if !strings.HasSuffix(base, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", base)
	}
	dbName := strings.TrimSuffix(base, "_test") + fmt.Sprintf("_f12%s%d_test", suffix, os.Getpid())
	for _, r := range dbName {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			t.Fatalf("scratch db name %q is not a safe identifier", dbName)
		}
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, dbName)); err != nil {
		t.Fatalf("kill connections: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName)); err != nil {
		t.Fatalf("drop old scratch: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	admin.Close()
	scratchDSN := withTestDB(dsn, dbName)
	pool, err := pgxpool.New(ctx, scratchDSN)
	if err != nil {
		t.Fatalf("open scratch pool: %v", err)
	}
	t.Cleanup(func() {}) // pool cleanup below
	// scratchDSN rides along for the core runner (db.Open owns the core
	// ledger — pre-applying SQL by hand would desync it).
	return pool, scratchDSN, func() {
		c := context.Background()
		_, _ = pool.Exec(c, fmt.Sprintf(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, dbName))
		pool.Close()
		if a, err := pgxpool.New(c, dsn); err == nil {
			_, _ = a.Exec(c, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
			a.Close()
		}
	}
}

func withTestDB(dsn, newDB string) string {
	i := strings.LastIndex(dsn, "/")
	head, tail := dsn[:i+1], dsn[i+1:]
	if j := strings.Index(tail, "?"); j >= 0 {
		return head + newDB + "?" + tail[j+1:]
	}
	return head + newDB
}

// TestLibraryLegacyAdoptionCheck — the read-only adoption verdict over
// the three Bestands shapes.
func TestLibraryLegacyAdoptionCheck(t *testing.T) {
	ctx := context.Background()
	pool, scratchDSN, cleanup := freshScratch(t, "adopt")
	defer cleanup()

	// Shape 1: fresh — adoptable, no tables.
	rep, err := pglib.VerifyAdoption(ctx, pool)
	if err != nil || !rep.Adoptable || len(rep.LibraryTables) != 0 {
		t.Fatalf("fresh verdict: %+v %v", rep, err)
	}

	// Shape 2: ledgered and complete — adoptable (idempotent).
	// The core (0.1.x substrate) a Bestands DB carries — applied
	// through the core runner (it owns the core ledger).
	core, err := db.Open(ctx, scratchDSN)
	if err != nil {
		t.Fatalf("core open: %v", err)
	}
	if err := core.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	core.Close()
	if err := pglib.Migrate(ctx, pool); err != nil {
		t.Fatalf("library migrate: %v", err)
	}
	rep, err = pglib.VerifyAdoption(ctx, pool)
	if err != nil || !rep.Adoptable || len(rep.Missing) != 0 {
		t.Fatalf("ledgered verdict: %+v %v", rep, err)
	}

	// Shape 3: tables but an incomplete ledger — REFUSED (the drift the
	// DM cutover must decide, never a silent adoption). Simulated by
	// wiping one ledger row — the read-only check itself never writes.
	if _, err := pool.Exec(ctx, `DELETE FROM library_schema_migrations WHERE version = 'schema/0001_library.sql'`); err != nil {
		t.Fatal(err)
	}
	rep, err = pglib.VerifyAdoption(ctx, pool)
	if err != nil || rep.Adoptable || len(rep.Missing) != 1 {
		t.Fatalf("drifted verdict must refuse adoption: %+v %v", rep, err)
	}
	if rep.Adoptable || !strings.Contains(rep.Reason, "refusing silent adoption") {
		t.Fatalf("refusal must say why: %+v", rep)
	}
}

// TestMigrationIsolationLibraryVsStore — each component's migration set
// creates ONLY its own namespace and ledger (the physical split's
// structural witness; the shared core schema is the 0.1.x substrate both
// build on today).
func TestMigrationIsolationLibraryVsStore(t *testing.T) {
	ctx := context.Background()

	libPool, libDSN, libCleanup := freshScratch(t, "libonly")
	defer libCleanup()
	if core, err := db.Open(ctx, libDSN); err != nil {
		t.Fatalf("core open (library side): %v", err)
	} else if err := core.Migrate(ctx); err != nil {
		t.Fatalf("core migrate (library side): %v", err)
	} else {
		core.Close()
	}
	if err := pglib.Migrate(ctx, libPool); err != nil {
		t.Fatalf("library migrate: %v", err)
	}
	// The Library-only database carries NO Store ledger …
	var n int
	if err := libPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_tables WHERE schemaname='public' AND tablename='store_schema_migrations'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the Library engine created the STORE ledger — isolation broken")
	}
	// … and NO Store tables at all (the whole store_* namespace, not
	// just the ledger).
	if err := libPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'store\_%' ESCAPE '\'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the Library engine created STORE tables — isolation broken")
	}

	storePool, storeDSN, storeCleanup := freshScratch(t, "storeonly")
	defer storeCleanup()
	if core, err := db.Open(ctx, storeDSN); err != nil {
		t.Fatalf("core open (store side): %v", err)
	} else if err := core.Migrate(ctx); err != nil {
		t.Fatalf("core migrate (store side): %v", err)
	} else {
		core.Close()
	}
	if err := migrations.Migrate(ctx, storePool); err != nil {
		t.Fatalf("store migrate: %v", err)
	}
	// The Store-only database carries NO Library tables at all.
	if err := storePool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'library\_%' ESCAPE '\'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the Store engine created LIBRARY tables — isolation broken")
	}
}
