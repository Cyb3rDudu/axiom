// Package db owns the PostgreSQL connection and migration runner for axiom-ng.
// The ingest queue and, later, the Zotero mirror tables are managed here.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaFS embeds the ordered SQL migrations.
//
//go:embed schema/*.sql
var schemaFS embed.FS

// DB wraps the pgx connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres using the given DSN.
func Open(ctx context.Context, dsn string) (*DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("db: DSN is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases the pool.
func (d *DB) Close() { d.pool.Close() }

// Ping verifies the connection is alive.
func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Migrate applies all embedded schema migrations not yet recorded in
// schema_migrations. Each migration runs in its own transaction.
func (d *DB) Migrate(ctx context.Context) error {
	if err := d.ensureMigrationsTable(ctx); err != nil {
		return err
	}
	names, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	for _, name := range names {
		applied, err := d.migrationApplied(ctx, name)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		sqlBytes, err := schemaFS.ReadFile(name)
		if err != nil {
			return err
		}
		if err := d.applyMigration(ctx, name, string(sqlBytes)); err != nil {
			return err
		}
	}
	return nil
}

// Pool exposes the underlying pool (used by the ingest/repo layers).
func (d *DB) Pool() *pgxpool.Pool { return d.pool }
