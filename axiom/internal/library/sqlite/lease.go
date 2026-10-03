// lease.go — the single-writer declaration over SQLite (F07 #301, F12
// #306): same lease row, same heartbeat/TTL takeover semantics, same
// typed conflicts as the PostgreSQL engine. Cross-process serialization
// rides the IMMEDIATE transaction (the whole acquire is one atomic
// read-decide-write on the single writer slot) plus busy_timeout for a
// competing process — SQLite has no advisory locks and needs none here.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library"
)

// AcquireWriterLease takes the provider-scoped writer lease: fresh
// insert, or takeover when the current owner's heartbeat is older than
// ttl. A live owner refuses with *library.WriterLeaseConflict.
func (r *Repo) AcquireWriterLease(ctx context.Context, scope, owner string, ttl time.Duration) error {
	if scope == "" || owner == "" {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "writer lease scope and owner are required")
	}
	if ttl <= 0 {
		ttl = library.DefaultWriterLeaseTTL
	}
	// One IMMEDIATE transaction: read-decide-write is atomic against
	// every competing acquirer (same process and other processes on the
	// host — they park in busy_timeout, never interleave).
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease acquire")
	}
	defer tx.Rollback()
	var curOwner, heartbeat string
	err = tx.QueryRowContext(ctx,
		`SELECT owner, heartbeat_at FROM library_writer_leases WHERE scope = ?`, scope).
		Scan(&curOwner, &heartbeat)
	ts := formatTS(now(time.Now()))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO library_writer_leases (scope, owner, acquired_at, heartbeat_at) VALUES (?,?,?,?)`,
			scope, owner, ts, ts); err != nil {
			return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease acquire")
		}
	case err != nil:
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease read")
	default:
		hb, perr := parseTS(heartbeat)
		if perr != nil {
			return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, perr, "writer lease heartbeat")
		}
		if age := time.Since(hb); age < ttl {
			c := &library.WriterLeaseConflict{Scope: scope, Owner: curOwner, HeartbeatAge: age}
			return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassConflict, c, "single-writer guard")
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE library_writer_leases SET owner = ?, acquired_at = ?, heartbeat_at = ? WHERE scope = ?`,
			owner, ts, ts, scope); err != nil {
			return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease acquire")
		}
	}
	if err := tx.Commit(); err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease acquire")
	}
	return nil
}

// RenewWriterLease refreshes the heartbeat; a lost lease (taken over
// after silence) surfaces as Conflict — the writer must stop.
func (r *Repo) RenewWriterLease(ctx context.Context, scope, owner string) error {
	ct, err := r.write.ExecContext(ctx,
		`UPDATE library_writer_leases SET heartbeat_at = ? WHERE scope = ? AND owner = ?`,
		formatTS(now(time.Now())), scope, owner)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease renew")
	}
	if n, _ := ct.RowsAffected(); n == 0 {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
			"writer lease "+scope+" no longer held by "+owner+" (taken over after silence) — stop writing")
	}
	return nil
}

// ReleaseWriterLease drops the lease on a graceful stop. Releasing a
// foreign lease is a no-op.
func (r *Repo) ReleaseWriterLease(ctx context.Context, scope, owner string) error {
	_, err := r.write.ExecContext(ctx,
		`DELETE FROM library_writer_leases WHERE scope = ? AND owner = ?`, scope, owner)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "writer lease release")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Anchors & write audit

// LookupProviderAnchor resolves the adapter's idempotency anchor to the
// provider id ("" + ErrRowAbsent when absent).
func (r *Repo) LookupProviderAnchor(ctx context.Context, scope, kind, anchor string) (string, int64, error) {
	var providerID string
	var version int64
	err := r.read.QueryRowContext(ctx,
		`SELECT provider_id, provider_version FROM library_provider_anchors
		 WHERE scope = ? AND kind = ? AND anchor = ?`, scope, kind, anchor).
		Scan(&providerID, &version)
	if err != nil {
		return "", 0, absent(err)
	}
	return providerID, version, nil
}

// PutProviderAnchor records the anchor; the SURVIVING provider id is
// returned (the anchor is the dedup winner; the version refreshes).
func (r *Repo) PutProviderAnchor(ctx context.Context, scope, kind, anchor, providerID string, version int64) (string, error) {
	if _, err := r.write.ExecContext(ctx, `
		INSERT INTO library_provider_anchors (scope, kind, anchor, provider_id, provider_version, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT (scope, kind, anchor) DO UPDATE SET provider_version = EXCLUDED.provider_version`,
		scope, kind, anchor, providerID, version, formatTS(now(time.Now()))); err != nil {
		return "", err
	}
	surviving, _, err := r.LookupProviderAnchor(ctx, scope, kind, anchor)
	return surviving, err
}

// PutProviderAnchorWithAudit persists the anchor AND its write-audit row
// in ONE transaction: the mutation↔audit 1:1 contract holds even against
// a crash between the two writes.
func (r *Repo) PutProviderAnchorWithAudit(ctx context.Context, scope, kind, anchor, providerID string, version int64, audit library.WriteAuditRow) (string, error) {
	if audit.Readback == nil {
		audit.Readback = map[string]any{}
	}
	readback, err := json.Marshal(audit.Readback)
	if err != nil {
		return "", err
	}
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO library_provider_anchors (scope, kind, anchor, provider_id, provider_version, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT (scope, kind, anchor) DO UPDATE SET provider_version = EXCLUDED.provider_version`,
		scope, kind, anchor, providerID, version, formatTS(now(time.Now()))); err != nil {
		return "", err
	}
	var surviving string
	if err := tx.QueryRowContext(ctx,
		`SELECT provider_id FROM library_provider_anchors WHERE scope=? AND kind=? AND anchor=?`,
		scope, kind, anchor).Scan(&surviving); err != nil {
		return "", absent(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO library_write_audit (scope, operation, anchor, provider_ref, outcome, readback, at)
		VALUES (?,?,?,?,?,?,?)`,
		audit.Scope, audit.Operation, audit.Anchor, audit.ProviderRef, audit.Outcome,
		string(readback), formatTS(now(time.Now()))); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return surviving, nil
}

// EvictProviderAnchor drops an anchor row whose provider id proved DEAD,
// guarded by provider_id.
func (r *Repo) EvictProviderAnchor(ctx context.Context, scope, kind, anchor, providerID string) error {
	_, err := r.write.ExecContext(ctx, `
		DELETE FROM library_provider_anchors
		WHERE scope = ? AND kind = ? AND anchor = ? AND provider_id = ?`,
		scope, kind, anchor, providerID)
	return err
}

// AppendWriteAudit records one mutation AFTER its readback verified.
func (r *Repo) AppendWriteAudit(ctx context.Context, row library.WriteAuditRow) error {
	if row.Readback == nil {
		row.Readback = map[string]any{}
	}
	readback, err := json.Marshal(row.Readback)
	if err != nil {
		return err
	}
	_, err = r.write.ExecContext(ctx, `
		INSERT INTO library_write_audit (scope, operation, anchor, provider_ref, outcome, readback, at)
		VALUES (?,?,?,?,?,?,?)`,
		row.Scope, row.Operation, row.Anchor, row.ProviderRef, row.Outcome,
		string(readback), formatTS(now(time.Now())))
	return err
}

// CountWriteAudit counts audit rows for a scope (the 1:1 sonde).
func (r *Repo) CountWriteAudit(ctx context.Context, scope string) (int, error) {
	var n int
	if err := r.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM library_write_audit WHERE scope = ?`, scope).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Zotero-mirror reads — honest absence

// LastSyncAt: the SQLite profile has no Zotero mirror (the mirror lives
// on the legacy shared database; no ATTACH, no cross-component
// queries) — absence, the same answer a standalone PostgreSQL library
// database gives.
func (r *Repo) LastSyncAt(ctx context.Context, sourceID string) (*time.Time, error) {
	return nil, nil
}

// MirrorRenditionPath: no mirror — absent.
func (r *Repo) MirrorRenditionPath(ctx context.Context, sourceID, attachmentKey string) (string, error) {
	return "", library.ErrRowAbsent
}

// MirrorCitation: no mirror — absent.
func (r *Repo) MirrorCitation(ctx context.Context, sourceID, recordID string) (*library.MirrorCitation, bool, error) {
	return nil, false, nil
}
