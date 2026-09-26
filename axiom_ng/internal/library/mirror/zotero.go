// Package mirror is the Zotero-mirror persistence — Library-owned since
// F09 (#303) moved it out of internal/repo so the Store-side packages stay
// Zotero-free in the dependency graph. It owns the zotero_* mirror tables'
// reads/writes, the canonical apply and the selection/contextual rule reads.
//
// Transition note (documented dual-write, abated by F12/DM06): the legacy
// sync lane still writes STORE-owned rows (ingest_jobs, the snapshot
// retire/restore reconciliation) inside the canonical apply transaction —
// via the exported repo methods — until revision intake replaces the
// legacy lane wholesale. internal/repo stays the Store-owned persistence
// and must never import the Zotero adapter again (store-boundary lint).
package mirror

import (
	"context"
	"fmt"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo wraps the pgx pool with the Zotero-mirror methods. It also carries
// the Store repo for the legacy sync lane's job writes (see the package
// transition note) — the composition constructs both from one pool.
type Repo struct {
	pool  *pgxpool.Pool
	store *repo.Repo
}

// New builds a mirror Repo on the same pool as the given Store repo.
// A nil store is tolerated (nil pool, nil store): the degraded sync boot
// path constructs the service before any database exists and must not
// dereference one — mirror calls on such a Repo fail loudly at first use,
// exactly like the pre-split nil *repo.Repo did.
func New(store *repo.Repo) *Repo {
	if store == nil {
		return &Repo{}
	}
	return &Repo{pool: store.Pool(), store: store}
}

// Pool returns the underlying pgx pool (used by canonical sync orchestration).
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

// lockKey derives a stable bigint advisory-lock key from a source UUID. The same
// key coordinates both the canonical sync (session-level pg_advisory_lock) and
// the claim (transaction-level pg_advisory_xact_lock); session and transaction
// advisory locks on the same key share one lock namespace and therefore exclude
// each other, serializing claim vs sync for a source.
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
func (m *Repo) AcquireSourceLock(ctx context.Context, sourceID string) (func(), error) {
	conn, err := m.pool.Acquire(ctx)
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
