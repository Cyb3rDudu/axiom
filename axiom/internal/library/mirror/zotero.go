// Package mirror is the Zotero-mirror persistence — Library-owned since
// F09 (#303), and Library-DATABASE-resident since #358: the mirror repo
// runs on the Library component's own PostgreSQL pool. It owns the
// zotero_* mirror tables' reads/writes, the canonical apply and the
// selection/contextual rule reads.
//
// The canonical apply is deliberately mirror-only (no Store writes): the
// sync orchestrates the Store-side effects (revision intake, projection
// upserts, failed-file records, snapshot reconciliation) in a separate
// transaction on the Store database after the mirror commits — one
// component, one database, per transaction.
package mirror

import (
	"context"
	"fmt"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo wraps the LIBRARY pool with the Zotero-mirror methods.
type Repo struct {
	pool *pgxpool.Pool
}

// New builds a mirror Repo on the Library component's pool. A nil pool is
// tolerated: the degraded sync boot path constructs the service before
// any database exists and must not dereference one — mirror calls on
// such a Repo fail loudly at first use.
func New(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

// Pool returns the underlying pgx pool.
func (m *Repo) Pool() *pgxpool.Pool { return m.pool }

// EnsureSource returns the id of a zotero_sources row for the given base URL
// and library, creating it if absent (upsert on the unique pair).
func (m *Repo) EnsureSource(ctx context.Context, baseURL, libraryID, serverID string) (string, error) {
	var id string
	err := m.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (base_url, library_id)
		DO UPDATE SET server_id = EXCLUDED.server_id, updated_at = now()
		RETURNING id
	`, baseURL, libraryID, serverID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ensure source: %w", err)
	}
	return id, nil
}

// AcquireSourceLock acquires a session-level advisory lock for a source on
// a dedicated connection and returns a release function. The lock serialises
// a whole sync (cursor read, reconciliation, cursor commit) per source_id across
// pool connections, so a slow stale delta cannot overwrite a newer
// reconciliation.
//
// The returned release function explicitly runs pg_advisory_unlock on the same
// connection (with its own timeout context) before returning the connection to
// the pool; a plain conn.Release() would not end the session-level lock. If the
// unlock fails the physical connection is closed instead of being reused.
//
// #358 note: this lock lives on the LIBRARY database (the mirror's home) and
// serialises syncs against each other; the claim's transaction-scoped twin
// (same key, same definition) lives on the Store database and serializes
// claims against the sync's store-effect phase there.
func (m *Repo) AcquireSourceLock(ctx context.Context, sourceID string) (func(), error) {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lock conn: %w", err)
	}
	key := repo.LockKey(sourceID)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock source: %w", err)
	}

	release := func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key); err == nil {
			conn.Release()
			return
		}
		// Unlock failed: hijack the physical connection out of the pool so the
		// session-level lock is dropped by the session end and the pool slot is
		// not leaked, then close it and return the wrapper to the pool.
		physical := conn.Hijack()
		_ = physical.Close(unlockCtx)
		conn.Release()
	}
	return release, nil
}
