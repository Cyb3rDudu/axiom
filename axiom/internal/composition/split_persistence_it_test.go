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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/config"
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

// TestIT_SQLiteLibraryProfileBoots — the SQLite profile: full stack
// boots, the Library lives in ONE file (restrictive permissions, only
// the library_* namespace — the sqlite package pins the deeper rules),
// the store database gains no library tables, and the public health
// shape carries no storage-topology detail (the frozen baseline).
func TestIT_SQLiteLibraryProfileBoots(t *testing.T) {
	storePool, storeDSN, storeCleanup := freshScratch(t, "splitsqlite")
	defer storeCleanup()
	storePool.Close()
	libFile := filepath.Join(t.TempDir(), "library.sqlite")
	root := bootSplitPersistence(t, storeDSN, func(cfg *config.Config) {
		cfg.StorageLibraryDriver = "sqlite"
		cfg.LibrarySQLitePath = libFile
	})
	if root.libSQLite == nil || root.libDB != nil {
		t.Fatalf("sqlite profile must wire the file engine, not a pool: libSQLite=%v libDB=%v", root.libSQLite, root.libDB)
	}
	fi, err := os.Stat(libFile)
	if err != nil {
		t.Fatalf("library.sqlite: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("library.sqlite must be 0600, got %o", fi.Mode().Perm())
	}
	ctx := context.Background()
	storeChk, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer storeChk.Close()
	var n int
	if err := storeChk.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'library\_%' ESCAPE '\'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the sqlite profile must not migrate library tables into the store database")
	}

	// Health stays the frozen shape: no storage/topology field appears.
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/api/health", root.cfg.APIPort)
	resp, err := noKeepAliveGet(healthURL)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status: %d body=%s", resp.StatusCode, body)
	}
	var hb map[string]any
	if err := json.Unmarshal(body, &hb); err != nil {
		t.Fatalf("health body: %v", err)
	}
	for k := range hb {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "sqlite") || strings.Contains(lk, "storage") || strings.Contains(lk, "driver") {
			t.Fatalf("health leaked the storage topology (%s) — the public shape is frozen: %s", k, body)
		}
	}
}
