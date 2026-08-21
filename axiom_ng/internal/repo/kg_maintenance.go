package repo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// kgMaintenanceLockKey is a stable project-local advisory lock key for KG
// maintenance. The lock is transaction-scoped, so crash/cancel releases it
// with the rollback.
const kgMaintenanceLockKey int64 = 0x4158494f4d4b4701 // "AXIOMKG\x01"

type kgMaintenanceHookKey string

var kgMaintenanceTestHook func(kgMaintenanceHookKey)

func kgHook(k kgMaintenanceHookKey) {
	if kgMaintenanceTestHook != nil {
		kgMaintenanceTestHook(k)
	}
}

func (r *Repo) withKGMaintenanceTx(ctx context.Context, label string, fn func(pgx.Tx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("%s begin: %w", label, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, kgMaintenanceLockKey); err != nil {
		return fmt.Errorf("%s lock: %w", label, err)
	}
	kgHook(kgMaintenanceHookKey(label + ":after_lock"))
	if err := fn(tx); err != nil {
		return err
	}
	kgHook(kgMaintenanceHookKey(label + ":before_commit"))
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s commit: %w", label, err)
	}
	return nil
}
