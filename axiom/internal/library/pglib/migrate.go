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
	// DM07 #316: privilege-free when the ledger already exists — CREATE
	// TABLE IF NOT EXISTS still demands CREATE-on-schema even as a no-op,
	// and the DML-only runtime role boots on a current schema without
	// attempting any DDL. The catalog read is world-readable.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'library_schema_migrations')`,
	).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
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

// Pending returns the embedded Library migrations not yet recorded in the
// ledger, in apply order. READ-ONLY (catalog + ledger reads, no DDL):
// works under a DML-only role — the #362 boot fail-fast and the
// `axiom migrate` report both key off it. A missing ledger table reads
// as an empty ledger (fresh database: everything pending).
func Pending(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	var ledgerExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'library_schema_migrations')`,
	).Scan(&ledgerExists); err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	if ledgerExists {
		rows, err := pool.Query(ctx, `SELECT version FROM library_schema_migrations`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			applied[v] = true
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	names, err := fs.Glob(librarySchemaFS, "schema/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var pending []string
	for _, name := range names {
		if !applied[name] {
			pending = append(pending, name)
		}
	}
	return pending, nil
}
