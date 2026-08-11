// Package repo provides database access for the axiom-ng orchestrator: the
// Zotero mirror tables and the ingest queue.
package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo wraps the pgx pool with methods for the Zotero mirror and ingest queue.
type Repo struct {
	pool *pgxpool.Pool
}

// New builds a Repo from an existing pool.
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Pool returns the underlying pgx pool (used by canonical sync orchestration).
func (r *Repo) Pool() *pgxpool.Pool { return r.pool }

// EnsureSource returns the id of a zotero_sources row for the given base URL
// and library, creating it if absent (upsert on the unique pair).
func (r *Repo) EnsureSource(ctx context.Context, baseURL, libraryID, serverID string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, `
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

// lockKey derives a stable bigint advisory-lock key from a source UUID so two
// concurrent syncs of the same source are serialised.
func lockKey(sourceID string) int64 {
	// Use the last 8 bytes of the UUID string's hash-like value; good enough
	// for a per-source lock and avoids any collision-sensitive hashing lib.
	var acc int64
	for i := 0; i < len(sourceID); i++ {
		acc = acc*31 + int64(sourceID[i])
	}
	return acc & 0x7FFFFFFFFFFFFFFF
}

// AcquireSourceLock acquires a session-level advisory lock for a source on a
// dedicated connection and returns a release function. The lock serialises a
// whole sync (cursor read, reconciliation, cursor commit) per source_id across
// pool connections, so a slow stale delta cannot overwrite a newer
// reconciliation.
//
// The returned release function explicitly runs pg_advisory_unlock on the same
// connection (with its own timeout context) before returning the connection to
// the pool; a plain conn.Release() would not end the session-level lock. If the
// unlock fails the physical connection is closed instead of being reused.
func (r *Repo) AcquireSourceLock(ctx context.Context, sourceID string) (func(), error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lock conn: %w", err)
	}
	key := lockKey(sourceID)
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
