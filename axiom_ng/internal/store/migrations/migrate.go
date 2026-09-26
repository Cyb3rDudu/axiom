// migrate.go — the Store component's migration runner (F09 #303),
// mirroring the Library pattern (F06): own set (schema/*.sql), own ledger
// (store_schema_migrations), same physical database. The core runner
// (internal/db) never sees these files — the F01 fingerprint stays
// derived from the core set alone. Additive only.
//
// Leaf package (pgx + embed only, no repo dependency) so both the
// composition root AND the repo integration harness can apply it without
// an import cycle.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// storeSchemaFS embeds the ordered Store SQL migrations.
//
//go:embed schema/*.sql
var storeSchemaFS embed.FS

// Migrate applies every not-yet-applied Store migration to the pool's
// database. Idempotent; safe to call at every start.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS store_schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("store migrations table: %w", err)
	}
	entries, err := fs.ReadDir(storeSchemaFS, "schema")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".sql" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM store_schema_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, rerr := storeSchemaFS.ReadFile("schema/" + name)
		if rerr != nil {
			return rerr
		}
		tx, terr := pool.Begin(ctx)
		if terr != nil {
			return terr
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("store migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO store_schema_migrations (version) VALUES ($1)`, name); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
