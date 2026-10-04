// roles_drill_it_test.go — the DM07 #316 dev-migration drill against
// real PostgreSQL (scratch databases, AXIOM_TEST_DATABASE_URL-gated —
// the freshScratch convention: never against a non-_test DSN). This is
// the PROBE surface for the credential split; the reference/production
// database is DM09 window work with the administrator.
//
// The drill executes deploy/postgres/roles.sql ITSELF (the very file
// the cutover window runs), so the scripted grants and the tested
// grants cannot drift. Witness chain:
//
//   - the script applies cleanly, TWICE (idempotent);
//   - both component pools connect as their roles and boot their
//     migration ledgers no-op (DML-only roles boot on a current schema);
//   - the least-privilege matrix: own-component DML allowed,
//     cross-component SELECT denied in BOTH directions (including the
//     Library-owned mirror and repair tables the store role must not
//     read);
//   - the wrong-role guard fires on the KNOWN swap (library pool fed
//     the store DSN) and passes on the correct assignment;
//   - cross-database CONNECT denial after the cutover-window REVOKE.
package composition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/pglib"
	storemigrations "github.com/Cyb3rDudu/axiom/axiom/internal/store/migrations"
)

// rolesSQLPath resolves deploy/postgres/roles.sql from this package's
// directory (axiom/internal/composition → repo root three levels up).
func rolesSQLPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "deploy", "postgres", "roles.sql")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("roles.sql not found at %s: %v", p, err)
	}
	return p
}

// roleDSN rewrites the admin DSN onto one of the DM07 component roles
// (keyword/value form — pgconn-normalized, input-spelling-agnostic).
func roleDSN(t *testing.T, adminDSN, role, password string) string {
	t.Helper()
	cfg, err := pgconn.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	sslmode := "prefer"
	if cfg.TLSConfig == nil {
		sslmode = "disable"
	}
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.Database, role, password, sslmode)
}

// isPermissionDenied classifies a pg error as the role-level denial the
// matrix expects (SQLSTATE 42501; the string fallback keeps the drill
// robust across pool-wrapped errors).
func isPermissionDenied(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "42501" {
		return true
	}
	return strings.Contains(err.Error(), "permission denied")
}

// TestIT_Dm07RoleDrill — the whole witness chain in one ordered drill
// (the steps mirror the migration-guide section: migrate as admin,
// apply roles, boot as roles, deny cross-component, tighten CONNECT).
func TestIT_Dm07RoleDrill(t *testing.T) {
	ctx := context.Background()
	admin, scratchDSN, cleanup := freshScratch(t, "dm07")
	defer cleanup()

	// (1) migrations as ADMIN (the window/deployer shape; runtime roles
	// are DML-only): all three ledgers current.
	d, err := db.Open(ctx, scratchDSN)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	if err := storemigrations.Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("store migrate: %v", err)
	}
	if err := pglib.Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("library migrate: %v", err)
	}

	// (2) the scripted roles+grants, applied TWICE (idempotency).
	// PRE-EXISTENCE GUARD: roles are cluster-global. If either already
	// exists (a shared dev cluster also serving a reference database),
	// the drill would reset its password mid-run and DROP it in cleanup
	// — foreign state. Refuse instead; drill on a disposable mirror.
	// AXIOM_REQUIRE_DRILL=1 (set in CI) turns every skip into a failure:
	// a skip is never silently green where the drill is a gate.
	drillRefuse := func(format string, args ...any) {
		if os.Getenv("AXIOM_REQUIRE_DRILL") == "1" {
			t.Fatalf("drill required (AXIOM_REQUIRE_DRILL=1) but refused: "+format, args...)
		}
		t.Skipf(format, args...)
	}
	var preexist int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_roles WHERE rolname IN ('axiom_library','axiom_store')`).Scan(&preexist); err != nil {
		t.Fatalf("role pre-existence probe: %v", err)
	}
	if preexist > 0 {
		drillRefuse("axiom_library/axiom_store already exist on this cluster (%d found) — refusing to touch foreign roles; run the drill on a disposable mirror", preexist)
	}
	sqlBytes, err := os.ReadFile(rolesSQLPath(t))
	if err != nil {
		t.Fatalf("read roles.sql: %v", err)
	}
	rolesSQL := string(sqlBytes)
	for i := 1; i <= 2; i++ {
		if _, err := admin.Exec(ctx, rolesSQL); err != nil {
			t.Fatalf("roles.sql apply #%d: %v", i, err)
		}
	}
	// drill passwords (throwaway; the ALTER never echoes them anywhere)
	const libPW, storePW = "dm07-drill-lib", "dm07-drill-store"
	if _, err := admin.Exec(ctx, fmt.Sprintf(`ALTER ROLE axiom_library PASSWORD '%s'`, libPW)); err != nil {
		drillRefuse("cannot set drill passwords on this server (role management needs admin rights — CI runs it as superuser): %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`ALTER ROLE axiom_store PASSWORD '%s'`, storePW)); err != nil {
		drillRefuse("cannot set drill passwords on this server (role management needs admin rights — CI runs it as superuser): %v", err)
	}
	// the drill's roles are CLUSTER-GLOBAL (throwaway passwords): best-effort
	// drop so shared dev servers stay clean — CI containers are disposable.
	// t.Cleanup runs AFTER the test defers closed freshScratch's pools, so
	// the drop needs its own fresh admin connection on the base DSN.
	t.Cleanup(func() {
		base := os.Getenv("AXIOM_TEST_DATABASE_URL")
		if base == "" {
			return
		}
		if p, err := pgxpool.New(context.Background(), base); err == nil {
			_, _ = p.Exec(context.Background(), `DROP ROLE IF EXISTS axiom_library, axiom_store`)
			p.Close()
		}
	})

	libDSN := roleDSN(t, scratchDSN, "axiom_library", libPW)
	storeDSN := roleDSN(t, scratchDSN, "axiom_store", storePW)

	// (3) both pools connect as their roles, and their boot-time ledger
	// checks no-op on the current schema (DML-only boot witness).
	libPool, err := pgxpool.New(ctx, libDSN)
	if err != nil {
		t.Fatalf("library pool as axiom_library: %v", err)
	}
	defer libPool.Close()
	if err := pglib.Migrate(ctx, libPool); err != nil {
		t.Fatalf("library ledger check as axiom_library (must no-op on current schema): %v", err)
	}
	storePool, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		t.Fatalf("store pool as axiom_store: %v", err)
	}
	defer storePool.Close()
	if err := storemigrations.Migrate(ctx, storePool); err != nil {
		t.Fatalf("store ledger check as axiom_store (must no-op on current schema): %v", err)
	}
	// the CORE ledger boots as axiom_store too — the store component's
	// first boot step is database.Migrate, so the catalog short-circuit
	// in db/migrate.go is load-bearing for the DML-only operating model.
	coreDB, err := db.Open(ctx, storeDSN)
	if err != nil {
		t.Fatalf("core open as axiom_store: %v", err)
	}
	defer coreDB.Close()
	if err := coreDB.Migrate(ctx); err != nil {
		t.Fatalf("core ledger check as axiom_store (must no-op on current schema): %v", err)
	}

	// (4) the least-privilege matrix.
	// own-component reads + DML roundtrip (insert/rollback) work:
	var n int
	if err := libPool.QueryRow(ctx, `SELECT count(*) FROM library_imports`).Scan(&n); err != nil {
		t.Fatalf("library role read on library_imports: %v", err)
	}
	if err := libPool.QueryRow(ctx, `SELECT count(*) FROM zotero_items`).Scan(&n); err != nil {
		t.Fatalf("library role read on the Zotero mirror: %v", err)
	}
	if err := libRoundtrip(ctx, libPool); err != nil {
		t.Fatalf("library role DML roundtrip: %v", err)
	}
	if err := storePool.QueryRow(ctx, `SELECT count(*) FROM ingest_jobs`).Scan(&n); err != nil {
		t.Fatalf("store role read on ingest_jobs: %v", err)
	}
	// cross-component reads denied, BOTH directions, including the
	// Library-owned mirror/repair tables:
	for _, tc := range []struct {
		pool *pgxpool.Pool
		from string
		tbl  string
	}{
		{libPool, "axiom_library", "ingest_jobs"},
		{libPool, "axiom_library", "processing_chunks"},
		{libPool, "axiom_library", "opensearch_outbox"},
		{storePool, "axiom_store", "library_imports"},
		{storePool, "axiom_store", "zotero_items"},
		{storePool, "axiom_store", "repair_cases"},
		{storePool, "axiom_store", "zotero_write_audit"},
	} {
		if err := tc.pool.QueryRow(ctx, "SELECT count(*) FROM "+tc.tbl).Scan(&n); !isPermissionDenied(t, err) {
			t.Fatalf("cross-component read %s → %s must be DENIED by role, got %v", tc.from, tc.tbl, err)
		}
	}

	// (4b) grant completeness — the matrix above spot-checks; this closes
	// the typo hole: EVERY table the roles.sql contract lists must carry
	// the owning role's DML grant (existence-tolerant grant loops would
	// silently skip a mistyped name).
	for table, role := range rolesSQLContractGrants(t, rolesSQL) {
		var got int
		if err := admin.QueryRow(ctx,
			`SELECT count(DISTINCT privilege_type) FROM information_schema.role_table_grants
			 WHERE grantee = $1 AND table_name = $2
			   AND privilege_type IN ('SELECT','INSERT','UPDATE','DELETE')`, role, table).Scan(&got); err != nil {
			t.Fatalf("grant probe %s/%s: %v", role, table, err)
		}
		if got != 4 {
			t.Fatalf("roles.sql contract table %s must carry all four DML grants for %s, got %d", table, role, got)
		}
	}

	// (5) the wrong-role guard: correct assignment passes, the known
	// swap (library component fed the store DSN) fails loudly.
	if err := checkPoolRole(ctx, libPool, "library", roleAxiomStore); err != nil {
		t.Fatalf("correct assignment must pass the guard: %v", err)
	}
	if err := checkPoolRole(ctx, storePool, "library", roleAxiomStore); err == nil {
		t.Fatal("the known swap (library pool over the store role DSN) must fail the guard")
	}

	// (6) cross-database CONNECT denial — the cutover-window tightening
	// (per-DB REVOKE of the non-owning role, documented in the guide):
	// a SECOND scratch database that revokes axiom_library's CONNECT.
	admin2, scratch2DSN, cleanup2 := freshScratch(t, "dm07b")
	defer cleanup2()
	defer admin2.Close()
	// the cutover shape: the script applies (PUBLIC loses CONNECT), then
	// the window REVOKEs the non-owning role's own grant.
	if _, err := admin2.Exec(ctx, rolesSQL); err != nil {
		t.Fatalf("roles.sql on the second database: %v", err)
	}
	if _, err := admin2.Exec(ctx, `REVOKE CONNECT ON DATABASE `+dbNameOf(t, scratch2DSN)+` FROM axiom_library`); err != nil {
		t.Fatalf("cutover-style REVOKE: %v", err)
	}
	if denied, err := pgxpool.New(ctx, roleDSN(t, scratch2DSN, "axiom_library", libPW)); err == nil {
		defer denied.Close()
		// pgxpool.New is LAZY — the denial only surfaces on first use.
		if perr := denied.Ping(ctx); perr == nil {
			t.Fatal("axiom_library must not connect to a database its CONNECT was revoked from")
		}
	}
	ok, err := pgxpool.New(ctx, roleDSN(t, scratch2DSN, "axiom_store", storePW))
	if err != nil {
		t.Fatalf("axiom_store pool: %v", err)
	}
	defer ok.Close()
	if err := ok.Ping(ctx); err != nil {
		t.Fatalf("axiom_store connects to the tightened database: %v", err)
	}
}

// libRoundtrip proves DML (not just SELECT) works as the role: insert
// one ledger-fake row and roll back (leaves no trace).
func libRoundtrip(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO library_schema_migrations (version) VALUES ('dm07-drill-probe')`); err != nil {
		return err
	}
	return nil
}

// dbNameOf extracts the database name from a DSN (drill-local helper).
func dbNameOf(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	return cfg.Database
}

// rolesSQLContractGrants parses deploy/postgres/roles.sql's two grant
// DO blocks into (table → owning role) pairs — the completeness probe's
// truth comes from the script itself, so a Go-side list can never drift
// from what the window will actually run.
func rolesSQLContractGrants(t *testing.T, sql string) map[string]string {
	t.Helper()
	out := map[string]string{}
	blockRe := regexp.MustCompile(`(?s)ARRAY\[(.*?)\].*?ON %I TO (axiom_[a-z]+)`)
	for _, m := range blockRe.FindAllStringSubmatch(sql, -1) {
		role := m[2]
		for _, name := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(m[1], -1) {
			if _, dup := out[name[1]]; dup {
				t.Fatalf("table %s listed twice in roles.sql contract", name[1])
			}
			out[name[1]] = role
		}
	}
	if len(out) < 20 {
		t.Fatalf("roles.sql parse found only %d contract tables — the parser drifted from the script shape", len(out))
	}
	// The floor alone cannot detect a silently-lost BLOCK (the library
	// list alone carries 20 tables): both component roles must own tables.
	roles := map[string]bool{}
	for _, r := range out {
		roles[r] = true
	}
	if !roles["axiom_library"] || !roles["axiom_store"] {
		t.Fatalf("roles.sql parse must find contract tables for BOTH component roles, saw %v", roles)
	}
	return out
}

// TestRolesSQLContractParserPinsBothRoles — the DB-free half of the
// parser witness: the contract lists stay complete and both blocks stay
// attributable even on machines without a drill database (the IT itself
// is DSN-gated; this parse runs everywhere).
func TestRolesSQLContractParserPinsBothRoles(t *testing.T) {
	sqlBytes, err := os.ReadFile(rolesSQLPath(t))
	if err != nil {
		t.Fatal(err)
	}
	grants := rolesSQLContractGrants(t, string(sqlBytes))
	byRole := map[string]int{}
	for _, r := range grants {
		byRole[r]++
	}
	if byRole["axiom_library"] < 20 || byRole["axiom_store"] < 17 {
		t.Fatalf("contract lists must keep both full blocks, saw %v", byRole)
	}
}
