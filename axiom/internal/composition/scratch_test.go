// scratch_test.go — the freshScratch convention shared by the
// persistence ITs (roles drill, split persistence): a session-unique
// EMPTY scratch database (the cli-doctor convention: refuses non-_test
// base DSNs, dropped in cleanup). No migrations — the caller applies
// exactly the component sets under test.
package composition

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
