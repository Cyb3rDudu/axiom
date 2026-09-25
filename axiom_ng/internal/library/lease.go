// lease.go — the single-writer declaration's persistence (F07, #301).
// Library is a single-writer component: a SECOND write-capable instance
// against the same provider scope is refused at start. The guard is a
// provider-scoped lease row with a renewed heartbeat — cross-process by
// construction (the row lives in the shared Library persistence), not a
// local mutex. Zotero's If-Unmodified-Since-Version stays the last line
// behind it (a 412 becomes a typed retryable error in the adapter).
//
// Also here: the provider adapter's write-audit rows and its idempotency
// anchor ledger (see schema/0002).
package library

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/jackc/pgx/v5"
)

// WriterLeaseConflict reports a refused lease acquisition: another live
// writer holds the scope. The diagnosis names owner and heartbeat age.
type WriterLeaseConflict struct {
	Scope        string
	Owner        string
	HeartbeatAge time.Duration
}

func (e *WriterLeaseConflict) Error() string {
	return fmt.Sprintf("writer lease %s held by %s (heartbeat %s ago) — Library is single-writer: stop the other instance or wait for the lease TTL",
		e.Scope, e.Owner, e.HeartbeatAge.Truncate(time.Second))
}

// DefaultWriterLeaseTTL bounds how long a silent writer stays trusted: a
// writer renews at TTL/3; a crashed writer's lease is takeable after one
// full TTL. Personal-library scale — a restart waits at most one TTL.
const DefaultWriterLeaseTTL = 30 * time.Second

// AcquireWriterLease takes the provider-scoped writer lease: fresh insert,
// or takeover when the current owner's heartbeat is older than ttl. A
// live owner refuses with *WriterLeaseConflict (typed Conflict).
func (s *Store) AcquireWriterLease(ctx context.Context, scope, owner string, ttl time.Duration) error {
	if scope == "" || owner == "" {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "writer lease scope and owner are required")
	}
	if ttl <= 0 {
		ttl = DefaultWriterLeaseTTL
	}
	// One statement, DB clock authoritative: the insert wins only when the
	// scope is free OR the heartbeat went silent past the TTL.
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO library_writer_leases (scope, owner, acquired_at, heartbeat_at)
		VALUES ($1, $2, now(), now())
		ON CONFLICT (scope) DO UPDATE SET owner = $2, acquired_at = now(), heartbeat_at = now()
		WHERE library_writer_leases.heartbeat_at < now() - ($3 * interval '1 microsecond')`,
		scope, owner, int64(ttl/time.Microsecond))
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease acquire")
	}
	if ct.RowsAffected() == 0 {
		var cur struct {
			owner string
			age   time.Duration
		}
		err := s.pool.QueryRow(ctx,
			`SELECT owner, now() - heartbeat_at FROM library_writer_leases WHERE scope = $1`, scope).
			Scan(&cur.owner, &cur.age)
		if err != nil {
			return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease read")
		}
		c := &WriterLeaseConflict{Scope: scope, Owner: cur.owner, HeartbeatAge: cur.age}
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassConflict, c, "single-writer guard")
	}
	return nil
}

// RenewWriterLease refreshes the heartbeat (the writer's liveness proof).
// A lost lease (taken over after silence) surfaces as Conflict — the
// writer must stop, never write past a lease it no longer holds.
func (s *Store) RenewWriterLease(ctx context.Context, scope, owner string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE library_writer_leases SET heartbeat_at = now() WHERE scope = $1 AND owner = $2`,
		scope, owner)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease renew")
	}
	if ct.RowsAffected() == 0 {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
			"writer lease "+scope+" no longer held by "+owner+" (taken over after silence) — stop writing")
	}
	return nil
}

// ReleaseWriterLease drops the lease on a graceful stop (the TTL covers
// the ungraceful ones). Releasing a foreign lease is a no-op.
func (s *Store) ReleaseWriterLease(ctx context.Context, scope, owner string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM library_writer_leases WHERE scope = $1 AND owner = $2`, scope, owner)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease release")
	}
	return nil
}

// WriteAuditRow is one provider mutation's audit line.
type WriteAuditRow struct {
	Scope       string
	Operation   string
	Anchor      string
	ProviderRef string
	Outcome     string // created | reused | changed | removed
	Readback    any    // what the readback observed (JSONB)
}

// AppendWriteAudit records one mutation AFTER its readback verified.
// A nil readback detail persists as '{}' (the column is NOT NULL — a
// mutation without readback evidence would violate the audit contract
// anyway, so the empty object is the honest floor).
func (s *Store) AppendWriteAudit(ctx context.Context, r WriteAuditRow) error {
	if r.Readback == nil {
		r.Readback = map[string]any{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO library_write_audit (scope, operation, anchor, provider_ref, outcome, readback, at)
		VALUES ($1,$2,$3,$4,$5,$6,now())`,
		r.Scope, r.Operation, r.Anchor, r.ProviderRef, r.Outcome, r.Readback)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "write audit append")
	}
	return nil
}

// EvictProviderAnchor drops an anchor row whose provider id proved DEAD
// (the item vanished from the provider) so the next ensure re-anchors a
// fresh one. Guarded by provider_id: a row someone else replaced in the
// meantime is not ours to evict.
func (s *Store) EvictProviderAnchor(ctx context.Context, scope, kind, anchor, providerID string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM library_provider_anchors
		WHERE scope = $1 AND kind = $2 AND anchor = $3 AND provider_id = $4`,
		scope, kind, anchor, providerID)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "provider anchor evict")
	}
	return nil
}

// CountWriteAudit counts audit rows for a scope (the 1:1 mutation sonde).
func (s *Store) CountWriteAudit(ctx context.Context, scope string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_write_audit WHERE scope = $1`, scope).Scan(&n); err != nil {
		return 0, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "write audit count")
	}
	return n, nil
}

// LookupProviderAnchor resolves the adapter's idempotency anchor to the
// provider id ("" when absent — pgx.ErrNoRows stays the caller's signal
// for absent, like the other lookups).
func (s *Store) LookupProviderAnchor(ctx context.Context, scope, kind, anchor string) (providerID string, version int64, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT provider_id, provider_version FROM library_provider_anchors
		 WHERE scope = $1 AND kind = $2 AND anchor = $3`, scope, kind, anchor).
		Scan(&providerID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, err
	}
	if err != nil {
		return "", 0, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "provider anchor lookup")
	}
	return providerID, version, nil
}

// PutProviderAnchor records the anchor; a concurrent duplicate returns
// the SURVIVING provider id (the anchor is the dedup winner — the loser
// must return the winner's id so the saga never books two).
func (s *Store) PutProviderAnchor(ctx context.Context, scope, kind, anchor, providerID string, version int64) (string, error) {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO library_provider_anchors (scope, kind, anchor, provider_id, provider_version)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (scope, kind, anchor) DO NOTHING`,
		scope, kind, anchor, providerID, version)
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "provider anchor put")
	}
	surviving, _, err := s.LookupProviderAnchor(ctx, scope, kind, anchor)
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "provider anchor readback")
	}
	return surviving, nil
}
