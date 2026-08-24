package repo

import (
	"context"
	"fmt"
	"time"

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

// kgProgressSink is the heartbeat sink for long-running mutating KG passes
// (#202): the mutating CLIs wire their logger here so a supervised run (screen,
// launchd agent) emits a one-line liveness signal while it works. nil (the
// default) disables heartbeats — server/sync paths stay silent as before.
// Same set-once-at-start discipline as kgMaintenanceTestHook.
var kgProgressSink func(format string, args ...any)

// SetKGProgressLogger wires the KG heartbeat sink (CLI main passes its
// logger.Printf; tests pass a recorder). Passing nil disables heartbeats.
func SetKGProgressLogger(f func(format string, args ...any)) {
	kgProgressSink = f
}

// kgHeartbeatInterval is the deliberately coarse progress cadence (#202):
// liveness, not per-item spam.
const kgHeartbeatInterval = 30 * time.Second

// kgHeartbeat paces progress lines for one mutating loop. A nil *kgHeartbeat
// is valid and fully functional as a no-op, so call sites need no sink check:
// newKGHeartbeat returns nil unless a sink is set and there is work to report.
type kgHeartbeat struct {
	unit     string
	total    int
	started  time.Time
	lastBeat time.Time
}

// newKGHeartbeat prepares a beater for a loop over total items of unit
// (e.g. "entities", "relation pairs"). The first beat fires immediately
// (start signal), then at most once per kgHeartbeatInterval.
func newKGHeartbeat(unit string, total int) *kgHeartbeat {
	if kgProgressSink == nil || total <= 0 {
		return nil
	}
	return &kgHeartbeat{unit: unit, total: total, started: time.Now()}
}

// beat reports progress for item done of total with current as the position
// marker (an entity/relation id). No-op on a nil beater.
func (h *kgHeartbeat) beat(done int, current string) {
	if h == nil {
		return
	}
	now := time.Now()
	if now.Sub(h.lastBeat) < kgHeartbeatInterval {
		return
	}
	h.lastBeat = now
	kgProgressSink("kg heartbeat: %s %d/%d, elapsed %s, current %s",
		h.unit, done, h.total, now.Sub(h.started).Round(time.Second), current)
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
