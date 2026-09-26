// store.go — the Store-owned persistence core (F09 #303): the repo.Repo
// wraps the pgx pool for every Store table (ingest_jobs, snapshots,
// chunks, embeddings, KG read models, outbox, retention). The Zotero
// mirror access that used to live here moved to internal/library/mirror
// (Library-owned); this package must stay free of Zotero-adapter imports —
// the store-boundary lint in internal/store proves it transitively.
package repo

import "github.com/jackc/pgx/v5/pgxpool"

// Repo wraps the pgx pool with the Store persistence methods.
type Repo struct {
	pool *pgxpool.Pool
}

// New builds a Repo from an existing pool.
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Pool returns the underlying pgx pool (composition wiring and tests).
func (r *Repo) Pool() *pgxpool.Pool { return r.pool }

// lockKey derives a stable bigint advisory-lock key from a source UUID. The
// same key coordinates the canonical sync (session-level pg_advisory_lock,
// mirror side) and the claim (transaction-level pg_advisory_xact_lock
// here) — session and transaction advisory locks on the same key share one
// namespace and therefore exclude each other, serializing claim vs sync
// for a source. Duplicated from mirror (pre-split single definition); the
// value must stay IDENTICAL in both packages or claim/sync stop excluding
// each other.
func lockKey(sourceID string) int64 {
	var acc int64
	for i := 0; i < len(sourceID); i++ {
		acc = acc*31 + int64(sourceID[i])
	}
	return acc & 0x7FFFFFFFFFFFFFFF
}
