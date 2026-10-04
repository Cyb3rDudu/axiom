package db

import "context"

func (d *DB) ensureMigrationsTable(ctx context.Context) error {
	// DM07 #316: privilege-free when the ledger already exists — CREATE
	// TABLE IF NOT EXISTS still demands CREATE-on-schema even as a no-op,
	// and the DML-only runtime role boots on a current schema without
	// attempting any DDL. The catalog read is world-readable.
	var exists bool
	if err := d.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'schema_migrations')`,
	).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := d.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	return err
}

func (d *DB) migrationApplied(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name,
	).Scan(&exists)
	return exists, err
}

func (d *DB) applyMigration(ctx context.Context, name, sqlText string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, sqlText); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
