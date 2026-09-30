// repository_pg_it_test.go — the Library repository contract suite
// against the PostgreSQL engine (F12 #306): the same suite, the same
// fixtures as the SQLite leg (internal/library/sqlite). Gated on
// AXIOM_TEST_DATABASE_URL (scratch-DB convention) — the availability
// skip lives in the runner BEFORE any test body; CI's matrix leg
// selects this engine explicitly.
package pglib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/reposuite"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRepositoryContractSuitePostgres(t *testing.T) {
	reposuite.Run(t, "postgres", func(t *testing.T) (library.Repository, func(), bool) {
		dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
		if dsn == "" {
			return nil, nil, false
		}
		base := dbOf(dsn)
		if !strings.HasSuffix(base, "_test") {
			t.Fatalf("refusing to run against non-_test database %q", base)
		}
		dbName := strings.TrimSuffix(base, "_test") + fmt.Sprintf("_librepo%d_test", os.Getpid())
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
		pool, err := pgxpool.New(ctx, withDB(dsn, dbName))
		if err != nil {
			t.Fatalf("open scratch pool: %v", err)
		}
		st := NewStore(pool)
		if err := st.Migrate(ctx); err != nil {
			pool.Close()
			t.Fatalf("library migrate: %v", err)
		}
		return st, func() {
			stools := context.Background()
			_, _ = pool.Exec(stools, fmt.Sprintf(
				`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, dbName))
			pool.Close()
			if a, err := pgxpool.New(stools, dsn); err == nil {
				_, _ = a.Exec(stools, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
				a.Close()
			}
		}, true
	})
}

// TestMitschriebLaneOnLibraryOnlyDatabaseFoldsToAbsence — a library-only
// database (own DSN, the F12 split shape) has NO Zotero mirror: the
// legacy Mits-Schreib reads fold 42P01 to absence (0/nil), never an
// internal error — the composition unwires the lane loudly, this pins
// the engine's defensive half.
func TestMitschriebLaneOnLibraryOnlyDatabaseFoldsToAbsence(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping library IT")
	}
	base := dbOf(dsn)
	if !strings.HasSuffix(base, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", base)
	}
	dbName := strings.TrimSuffix(base, "_test") + fmt.Sprintf("_libonly%d_test", os.Getpid())
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	pool, err := pgxpool.New(ctx, withDB(dsn, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if a, err := pgxpool.New(context.Background(), dsn); err == nil {
			_, _ = a.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
			a.Close()
		}
	})
	st := NewStore(pool)
	if err := st.Migrate(ctx); err != nil { // library migrations ONLY — no core schema, no mirror
		t.Fatalf("library migrate: %v", err)
	}
	if n, err := st.RecordSyncRevisions(ctx, "11111111-1111-4111-8111-111111111111"); err != nil || n != 0 {
		t.Fatalf("sync mitschrieb on a mirrorless DB must fold to (0, nil), got (%d, %v)", n, err)
	}
	if err := st.RecordAttachmentRevision(ctx, "11111111-1111-4111-8111-111111111111", "DOC1", "ATT1", "sha", "application/pdf"); err != nil {
		t.Fatalf("heal mitschrieb on a mirrorless DB must fold to nil, got %v", err)
	}
	// The NEGATIVE half of the fold split: a missing LIBRARY-OWNED
	// relation is a schema fault and stays a RAW error — absent() must
	// never fold 42P01 for library rows (the widening the F12 review
	// reverted would turn this into a silent 404).
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS library_imports CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetImport(ctx, "11111111-1111-4111-8111-111111111111"); err == nil || errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("a missing library_* relation must surface as a raw error, not absence: %v", err)
	}
}

// TestMalformedImportIDIgnoredAsAbsent — a syntactically invalid uuid is
// an UNKNOWN import (22P02 folded to ErrRowAbsent), not an internal
// error — the PG engine's trust-boundary folding. Self-contained: own
// scratch DB with library migrations.
func TestMalformedImportIDIgnoredAsAbsent(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping library IT")
	}
	base := dbOf(dsn)
	if !strings.HasSuffix(base, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", base)
	}
	dbName := strings.TrimSuffix(base, "_test") + fmt.Sprintf("_libmalformed%d_test", os.Getpid())
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	pool, err := pgxpool.New(ctx, withDB(dsn, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if a, err := pgxpool.New(context.Background(), dsn); err == nil {
			_, _ = a.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
			a.Close()
		}
	})
	st := NewStore(pool)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("library migrate: %v", err)
	}
	if _, err := st.GetImport(ctx, "deadbeef-not-a-uuid"); !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("malformed import id must read as absent, got %v", err)
	}
}
