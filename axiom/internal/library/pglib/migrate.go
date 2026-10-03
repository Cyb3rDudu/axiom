// migrate.go — the Library component's PostgreSQL migration runner
// (F06 #300, F12 #306).
//
// Own set (schema/*.sql), own ledger (library_schema_migrations), same
// physical database as the core schema. The core runner (internal/db)
// never sees these files — the F01 fingerprint stays derived from the
// core set alone, and library migrations apply to both fresh and
// bestands databases (additive tables only).
package pglib

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// librarySchemaFS embeds the ordered Library SQL migrations.
//
//go:embed schema/*.sql
var librarySchemaFS embed.FS

func (s *Store) ensureLibraryMigrationsTable(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS library_schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	return err
}

func (s *Store) libraryMigrationApplied(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM library_schema_migrations WHERE version = $1)`, name,
	).Scan(&exists)
	return exists, err
}

func (s *Store) applyLibraryMigration(ctx context.Context, name, sqlText string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, sqlText); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO library_schema_migrations (version) VALUES ($1)`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Migrate applies every embedded Library migration not yet in the Library
// ledger. Each migration runs in its own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	if err := s.ensureLibraryMigrationsTable(ctx); err != nil {
		return err
	}
	names, err := fs.Glob(librarySchemaFS, "schema/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		applied, err := s.libraryMigrationApplied(ctx, name)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		sqlBytes, err := librarySchemaFS.ReadFile(name)
		if err != nil {
			return err
		}
		if err := s.applyLibraryMigration(ctx, name, string(sqlBytes)); err != nil {
			return fmt.Errorf("library migration %s: %w", name, err)
		}
	}
	return nil
}

// Migrate is the standalone entry for callers holding a pool but no Store
// (the composition root wires it right after the core db.Migrate).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return NewStore(pool).Migrate(ctx)
}

// ---------------------------------------------------------------------------
// Legacy adoption (read-only check — F12 #306)

// AdoptionReport is the verdict of VerifyAdoption over a Bestands
// database: which Library tables exist, which ledger versions are
// recorded, and whether Migrate may run. READ-ONLY — the actual cutover
// is DM09's, this check only refuses to silently adopt a drifted state.
type AdoptionReport struct {
	// LibraryTables lists the library_* tables found (empty on a fresh
	// database — adoptable, Migrate creates everything).
	LibraryTables []string
	// LedgerVersions lists the recorded library_schema_migrations rows.
	LedgerVersions []string
	// Missing lists ledger versions the migration set expects but the
	// database does not carry (the drift that blocks adoption).
	Missing []string
	// Adoptable is true when Migrate may run: fresh database (no library
	// tables), or complete ledger (idempotent re-run).
	Adoptable bool
	// Reason names the verdict (diagnostics, one line).
	Reason string
}

// VerifyAdoption inspects an existing database and decides whether the
// Library engine may migrate onto it. Three verdicts:
//
//   - fresh (no library_* tables): adoptable — the normal fresh install;
//   - ledgered and complete: adoptable — Migrate is idempotent;
//   - tables WITHOUT a complete ledger: NOT adoptable — the database
//     carries a Library state nobody ledgered (restored partial dump,
//     hand-built tables). Silently migrating would mask the drift; the
//     DM track (DM09) decides such adoptions case by case.
func VerifyAdoption(ctx context.Context, pool *pgxpool.Pool) (AdoptionReport, error) {
	var rep AdoptionReport
	rows, err := pool.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename LIKE 'library\_%' ESCAPE '\'
		ORDER BY tablename`)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return rep, err
		}
		rep.LibraryTables = append(rep.LibraryTables, name)
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}
	if len(rep.LibraryTables) == 0 {
		rep.Adoptable = true
		rep.Reason = "fresh database — no Library tables (fresh install)"
		return rep, nil
	}
	lrows, err := pool.Query(ctx, `SELECT version FROM library_schema_migrations ORDER BY version`)
	if err != nil {
		return rep, fmt.Errorf("library tables exist but no ledger: %w", err)
	}
	defer lrows.Close()
	for lrows.Next() {
		var v string
		if err := lrows.Scan(&v); err != nil {
			return rep, err
		}
		rep.LedgerVersions = append(rep.LedgerVersions, v)
	}
	if err := lrows.Err(); err != nil {
		return rep, err
	}
	have := map[string]bool{}
	for _, v := range rep.LedgerVersions {
		have[v] = true
	}
	names, err := fs.Glob(librarySchemaFS, "schema/*.sql")
	if err != nil {
		return rep, err
	}
	sort.Strings(names)
	for _, name := range names {
		if !have[name] {
			rep.Missing = append(rep.Missing, name)
		}
	}
	if len(rep.Missing) > 0 {
		rep.Adoptable = false
		rep.Reason = fmt.Sprintf("Library tables exist but the ledger misses %v — refusing silent adoption (the DM track decides)", rep.Missing)
		return rep, nil
	}
	rep.Adoptable = true
	rep.Reason = "ledgered and complete — Migrate is idempotent"
	return rep, nil
}
