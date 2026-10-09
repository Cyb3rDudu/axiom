// split_persistence_it_test.go — the F12 #306 pool-split witnesses at
// the composition level: the all-in-one boots with SEPARATE persistence
// per component (store pool + library pool/file), the components'
// schemas land ONLY in their own database, the public health shape
// stays frozen (no topology detail — the goldens' witness), and the
// SQLite profile boots the Library on one file while the store database
// stays untouched.
package composition

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/pglib"
	"github.com/Cyb3rDudu/axiom/axiom/internal/store/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// splitPersistenceCfg mirrors fullStackCfg over a CALLER-OWNED store
// DSN: these tests assert schema isolation between two databases, so
// they must not ride the shared composition harness (whose sibling
// tests legitimately migrate the library namespace into it).
func splitPersistenceCfg(t *testing.T, storeDSN, queryURL string) config.Config {
	t.Helper()
	cfg := fullStackCfg(t, openCompositionDB(t), queryURL) // shape template
	cfg.DatabaseURL = storeDSN                             // caller-owned store DB
	return cfg
}

// bootSplitPersistence boots the full stack (sync role = Library runs)
// with the given library storage profile over a CALLER-OWNED store
// database; the shared harness pool (storePool) is only the template's
// source.
func bootSplitPersistence(t *testing.T, storeDSN string, mutate func(cfg *config.Config)) *Root {
	t.Helper()
	runner := &fakeRunner{}
	querySrv := fakeQueryRunnerSrv(t)
	cfg := splitPersistenceCfg(t, storeDSN, querySrv.URL)
	cfg.LibraryImportProviders = "fake" // wire the Library import surface
	if mutate != nil {
		mutate(&cfg)
	}
	root, err := Full(cfg, testLogger(), fakePorts(t, runner, querySrv))
	if err != nil {
		t.Fatalf("full composition: %v", err)
	}
	sigCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	if err := root.Start(sigCtx); err != nil {
		root.Stop(context.Background())
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 20*time.Second)
		defer c()
		root.Stop(stopCtx)
		querySrv.Close()
	})
	return root
}

// TestIT_BootAbortClosesPools — the close-on-error witness: a start that
// fails AFTER the store pool opened (unknown library driver at the
// library gate) must close and nil every opened pool — the start undo
// only stops STARTED components, the named-return defer is the seam.
func TestIT_BootAbortClosesPools(t *testing.T) {
	storePool, storeDSN, storeCleanup := freshScratch(t, "abort")
	defer storeCleanup()
	storePool.Close()
	root := bootSplitPersistenceExpectStartError(t, storeDSN, func(cfg *config.Config) {
		cfg.StorageLibraryDriver = "oracle"
	}, "unknown AXIOM_STORAGE_LIBRARY_DRIVER")
	if root.database != nil || root.libDB != nil || root.libSQLite != nil {
		t.Fatalf("failed start must close+nil all pools: database=%v libDB=%v libSQLite=%v", root.database, root.libDB, root.libSQLite)
	}
}

// TestIT_SQLiteZoteroCombinationRefused — the Zotero provider reads the
// mirror on the shared database; a component-local SQLite file must not
// reach into the store repository for its source identity — the
// combination aborts loudly instead of degrading silently.
func TestIT_SQLiteZoteroCombinationRefused(t *testing.T) {
	storePool, storeDSN, storeCleanup := freshScratch(t, "refuse")
	defer storeCleanup()
	storePool.Close()
	root := bootSplitPersistenceExpectStartError(t, storeDSN, func(cfg *config.Config) {
		cfg.StorageLibraryDriver = "sqlite"
		cfg.LibrarySQLitePath = filepath.Join(t.TempDir(), "library.sqlite")
		cfg.LibraryImportProviders = "zotero"
	}, "requires the PostgreSQL profile")
	if root.database != nil || root.libDB != nil || root.libSQLite != nil {
		t.Fatalf("failed start must close+nil all pools: database=%v libDB=%v libSQLite=%v", root.database, root.libDB, root.libSQLite)
	}
}

// TestIT_SeparateLibraryPoolPerComponent — postgres profile with an OWN
// library DSN: the library namespace is migrated ONLY into the library
// database, the store database carries none of it, and the two pools
// are distinct objects (the seam the DM cutover rides: it turns a DSN,
// not code).
func TestIT_SeparateLibraryPoolPerComponent(t *testing.T) {
	storePool, storeDSN, storeCleanup := freshScratch(t, "splitpg")
	defer storeCleanup()
	libPool, libDSN, libCleanup := freshScratch(t, "libpool")
	defer libCleanup()
	libPool.Close()   // the composition opens its OWN pools over the DSNs
	storePool.Close() //

	root := bootSplitPersistence(t, storeDSN, func(cfg *config.Config) {
		cfg.StorageLibraryDriver = "postgres"
		cfg.LibraryDatabaseURL = libDSN
	})
	if root.libDB == nil {
		t.Fatal("postgres profile must wire the library's own pool")
	}
	ctx := context.Background()
	// Re-open read pools over both DSNs for the isolation asserts (the
	// composition's pools close with the root).
	libChk, err := pgxpool.New(ctx, libDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer libChk.Close()
	storeChk, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer storeChk.Close()
	countLib := func(pool *pgxpool.Pool) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'library\_%' ESCAPE '\'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Library tables in the LIBRARY database (its own pool migrated them).
	if n := countLib(libChk); n == 0 {
		t.Fatal("the library pool did not migrate the library namespace into its own database")
	}
	// The STORE database carries NO library tables.
	if n := countLib(storeChk); n != 0 {
		t.Fatalf("the store database carries %d library tables despite the separate library pool", n)
	}
	// The pools are distinct objects.
	if root.libDB.Pool() == root.database.Pool() {
		t.Fatal("library pool and store pool must be distinct objects")
	}
}

// bootSplitPersistenceExpectStartError boots the full stack EXPECTING the
// start to fail with the given message; returns the root for the
// post-abort asserts (pools nil).
func bootSplitPersistenceExpectStartError(t *testing.T, storeDSN string, mutate func(cfg *config.Config), wantErr string) *Root {
	t.Helper()
	h := openCompositionDB(t)
	runner := &fakeRunner{}
	querySrv := fakeQueryRunnerSrv(t)
	cfg := fullStackCfg(t, h, querySrv.URL)
	cfg.DatabaseURL = storeDSN
	if mutate != nil {
		mutate(&cfg)
	}
	root, err := Full(cfg, testLogger(), fakePorts(t, runner, querySrv))
	if err != nil {
		querySrv.Close()
		t.Fatalf("full composition must construct: %v", err)
	}
	if err := root.Start(context.Background()); err == nil || !strings.Contains(err.Error(), wantErr) {
		root.Stop(context.Background())
		querySrv.Close()
		t.Fatalf("start must fail with %q, got %v", wantErr, err)
	}
	querySrv.Close()
	return root
}

// TestIT_SQLiteLibraryProfileWithSyncRefused — #358: the Zotero mirror is
// Library-database-resident and PostgreSQL-only; the SQLite profile has no
// mirror home, so a composition running the SYNC role on SQLite aborts
// loudly at start (the refusal that replaced the old "mirror on the store
// database" compromise).
func TestIT_SQLiteLibraryProfileWithSyncRefused(t *testing.T) {
	storePool, storeDSN, storeCleanup := freshScratch(t, "splitsqlite")
	defer storeCleanup()
	storePool.Close()
	bootSplitPersistenceExpectStartError(t, storeDSN, func(cfg *config.Config) {
		cfg.StorageLibraryDriver = "sqlite"
		cfg.LibrarySQLitePath = filepath.Join(t.TempDir(), "library.sqlite")
	}, "no Zotero mirror")
}

// TestMigrationIsolationLibraryVsStore — each component's migration set
// creates ONLY its own namespace and ledger (the physical split's
// structural witness; the shared core schema is the 0.1.x substrate both
// build on today). The companion TestIT_SeparateLibraryPoolPerComponent
// asserts the store database's library_* freedom; THIS one asserts the
// other direction — a Library-only database carries no Store tables —
// plus the store-only database's library_* freedom.
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
