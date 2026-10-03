// repo.go — the library.Repository method set over SQLite (F12 #306):
// the exact neutral contract, SQLite dialect. Every mutation rides the
// write handle (BEGIN IMMEDIATE), every read the WAL reader; the
// per-import advisory locks of the PostgreSQL engine map onto the
// IMMEDIATE transaction (the single writer slot serializes seq
// allocation and revision minting exactly like the lock did).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/contracterr"
	contracts "github.com/Cyb3rDudu/axiom/axiom/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library"
)

// ---------------------------------------------------------------------------
// Imports / saga

// CreateImport inserts the import row (app-minted canonical uuid); a
// concurrent identical insert surfaces as library.ErrDuplicateKey.
func (r *Repo) CreateImport(ctx context.Context, row library.ImportRow) error {
	// One clock reading for both timestamps (PG's single $9,$9 twin): a
	// µs drift between created_at and updated_at on INSERT is a parity wart.
	ts := formatTS(now(time.Now()))
	_, err := r.write.ExecContext(ctx, `
		INSERT INTO library_imports (import_id, idempotency_key, payload_hash, record_type, request_json,
			status, staging_sha256, staging_size, media_type, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		mustNewUUID(), row.IdempotencyKey, row.PayloadHash, row.RecordType, string(row.RequestJSON),
		string(row.Status), row.StagingSHA256, row.StagingSize, row.MediaType, ts, ts)
	return duplicate(err)
}

// now returns the DM03-compatible time form (UTC, microsecond-aligned).
func now(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// mustNewUUID mints the import id (crypto/rand failure at THIS point has
// no degradation path — the row cannot exist without an id).
func mustNewUUID() string {
	id, err := newUUID()
	if err != nil {
		panic(err)
	}
	return id
}

const importSelect = `SELECT import_id, idempotency_key, payload_hash, record_type, request_json, status,
		staging_sha256, staging_size, media_type,
		COALESCE(failure_code,''), COALESCE(failure_message,''),
		COALESCE(record_provider_id,''), COALESCE(rendition_provider_id,''), COALESCE(collection_provider_id,''),
		COALESCE(record_id,''), COALESCE(rendition_id,''), COALESCE(revision_id,0), created_at, updated_at
		FROM library_imports`

func scanImport(row *sql.Row) (library.ImportRow, error) {
	var r library.ImportRow
	var status, req, created, updated string
	err := row.Scan(&r.ImportID, &r.IdempotencyKey, &r.PayloadHash, &r.RecordType, &req, &status,
		&r.StagingSHA256, &r.StagingSize, &r.MediaType, &r.FailureCode, &r.FailureMessage,
		&r.RecordProviderID, &r.RenditionProviderID, &r.CollectionProviderID,
		&r.RecordID, &r.RenditionID, &r.RevisionID, &created, &updated)
	if err != nil {
		return library.ImportRow{}, absent(err)
	}
	r.Status = contracts.ImportStatus(status)
	r.RequestJSON = []byte(req)
	if r.CreatedAt, err = parseTS(created); err != nil {
		return library.ImportRow{}, err
	}
	if r.UpdatedAt, err = parseTS(updated); err != nil {
		return library.ImportRow{}, err
	}
	return r, nil
}

// GetByIdempotencyKey loads an import row; ErrRowAbsent when absent.
func (r *Repo) GetByIdempotencyKey(ctx context.Context, key string) (library.ImportRow, error) {
	return scanImport(r.read.QueryRowContext(ctx, importSelect+` WHERE idempotency_key = ?`, key))
}

// GetImport loads an import row by id; ErrRowAbsent when absent.
func (r *Repo) GetImport(ctx context.Context, importID string) (library.ImportRow, error) {
	return scanImport(r.read.QueryRowContext(ctx, importSelect+` WHERE import_id = ?`, importID))
}

// UpdateImportStatus advances the state machine (guarded CAS semantics
// identical to the PostgreSQL engine: zero rows affected is
// ErrRowAbsent, the guarded callers' Conflict signal).
func (r *Repo) UpdateImportStatus(ctx context.Context, importID string, status contracts.ImportStatus,
	expect contracts.ImportStatus,
	failure *contracts.ImportFailure, rec, rend, coll, recID, rendID string, revID int64) error {
	var fcode, fmsg any
	if failure != nil {
		fcode, fmsg = failure.Code, failure.Message
	}
	var rev any
	if revID > 0 {
		rev = revID
	}
	ct, err := r.write.ExecContext(ctx, `
		UPDATE library_imports SET
			status = ?,
			failure_code = CASE WHEN ? IN ('retryable_failed','terminal_failed')
				THEN COALESCE(?, failure_code) END,
			failure_message = CASE WHEN ? IN ('retryable_failed','terminal_failed')
				THEN COALESCE(?, failure_message) END,
			record_provider_id = COALESCE(NULLIF(?,''), record_provider_id),
			rendition_provider_id = COALESCE(NULLIF(?,''), rendition_provider_id),
			collection_provider_id = COALESCE(NULLIF(?,''), collection_provider_id),
			record_id = COALESCE(NULLIF(?,''), record_id),
			rendition_id = COALESCE(NULLIF(?,''), rendition_id),
			revision_id = COALESCE(?, revision_id),
			updated_at = ?
		WHERE import_id = ? AND (? = '' OR status = ?)`,
		string(status), string(status), fcode, string(status), fmsg, rec, rend, coll, recID, rendID,
		rev, formatTS(now(time.Now())), importID, string(expect), string(expect))
	if err != nil {
		return err
	}
	if n, _ := ct.RowsAffected(); n == 0 {
		return library.ErrRowAbsent
	}
	return nil
}

// ResumeInflightIDs lists imports still in an inflight status.
func (r *Repo) ResumeInflightIDs(ctx context.Context) ([]string, error) {
	rows, err := r.read.QueryContext(ctx, `
		SELECT import_id FROM library_imports
		WHERE status NOT IN ('committed','retryable_failed','terminal_failed','awaiting_confirmation')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---------------------------------------------------------------------------
// Events

// AppendEvent appends one event with the next seq — the allocation runs
// inside the IMMEDIATE transaction (the single writer slot replaces the
// PostgreSQL advisory lock; a concurrent double-drive serializes here).
func (r *Repo) AppendEvent(ctx context.Context, importID, kind string, detail any) error {
	d, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO library_import_events (import_id, seq, kind, detail, at)
		VALUES (?, (SELECT COALESCE(MAX(seq),0)+1 FROM library_import_events WHERE import_id = ?), ?, ?, ?)`,
		importID, importID, kind, string(d), formatTS(now(time.Now()))); err != nil {
		return err
	}
	return tx.Commit()
}

// ListEvents returns the event log in seq order.
func (r *Repo) ListEvents(ctx context.Context, importID string) ([]library.EventRow, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT seq, kind, detail, at FROM library_import_events WHERE import_id = ? ORDER BY seq`, importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []library.EventRow
	for rows.Next() {
		var e library.EventRow
		var at string
		if err := rows.Scan(&e.Seq, &e.Kind, &e.Detail, &at); err != nil {
			return nil, err
		}
		if e.At, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Steps

// UpsertStep books a step: first in_progress (attempts=1), re-entry
// increments attempts, done freezes the state.
func (r *Repo) UpsertStep(ctx context.Context, importID, step, state, providerRef string, detail any) error {
	d, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = r.write.ExecContext(ctx, `
		INSERT INTO library_import_steps (import_id, step, state, provider_ref, detail, attempts, updated_at)
		VALUES (?,?,?,?,?,1,?)
		ON CONFLICT (import_id, step) DO UPDATE SET
			state = EXCLUDED.state,
			provider_ref = COALESCE(NULLIF(EXCLUDED.provider_ref,''), library_import_steps.provider_ref),
			detail = EXCLUDED.detail,
			attempts = library_import_steps.attempts + CASE WHEN EXCLUDED.state = 'in_progress' THEN 1 ELSE 0 END,
			updated_at = EXCLUDED.updated_at`,
		importID, step, state, providerRef, string(d), formatTS(now(time.Now())))
	return err
}

// GetStep loads one step row; ErrRowAbsent when absent.
func (r *Repo) GetStep(ctx context.Context, importID, step string) (library.StepRow, error) {
	var sr library.StepRow
	var updated string
	err := r.read.QueryRowContext(ctx,
		`SELECT step, state, COALESCE(provider_ref,''), detail, attempts, updated_at
		 FROM library_import_steps WHERE import_id = ? AND step = ?`, importID, step).
		Scan(&sr.Step, &sr.State, &sr.ProviderRef, &sr.Detail, &sr.Attempts, &updated)
	if err != nil {
		return library.StepRow{}, absent(err)
	}
	if sr.UpdatedAt, err = parseTS(updated); err != nil {
		return library.StepRow{}, err
	}
	return sr, nil
}

// ---------------------------------------------------------------------------
// Identifiers

// ClaimIdentifiers records the normalized DOI/ISBN of a record; another
// record owning the identifier is a typed Conflict (identical wording
// and class to the PostgreSQL engine).
func (r *Repo) ClaimIdentifiers(ctx context.Context, recordID, doi, isbn string) error {
	for _, id := range []struct{ kind, val string }{
		{"doi", library.NormalizeDOI(doi)},
		{"isbn", library.NormalizeISBN(isbn)},
	} {
		if id.val == "" {
			continue
		}
		if _, err := r.write.ExecContext(ctx, `
			INSERT INTO library_external_identifiers (kind, normalized_value, record_id, created_at)
			VALUES (?,?,?,?)
			ON CONFLICT (kind, normalized_value) DO UPDATE SET record_id = EXCLUDED.record_id
			WHERE library_external_identifiers.record_id = EXCLUDED.record_id`,
			id.kind, id.val, recordID, formatTS(now(time.Now()))); err != nil {
			return err
		}
		var owner string
		if err := r.read.QueryRowContext(ctx,
			`SELECT record_id FROM library_external_identifiers WHERE kind = ? AND normalized_value = ?`,
			id.kind, id.val).Scan(&owner); err != nil {
			return absent(err)
		}
		if owner != recordID {
			return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
				fmt.Sprintf("%s %s already belongs to record %s", id.kind, id.val, owner))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Provenance

// AppendProvenance writes one provenance row, idempotent per the
// natural key (a resumed resolve step re-runs the ladder; the audit
// trail must not inflate).
func (r *Repo) AppendProvenance(ctx context.Context, importID string, p library.ProvenanceRow) error {
	if p.At.IsZero() {
		p.At = time.Now()
	}
	var exists bool
	if err := r.read.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM library_metadata_provenance
			WHERE import_id = ? AND field = ? AND source = ?
			  AND resolver_version = ? AND applied = ? AND value = ?)`,
		importID, p.Field, p.Source, p.ResolverVersion, p.Applied, p.Value).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := r.write.ExecContext(ctx, `
		INSERT INTO library_metadata_provenance (import_id, field, source, resolver_version, confidence, applied, value, at)
		VALUES (?,?,?,?,?,?,?,?)`,
		importID, p.Field, p.Source, p.ResolverVersion, p.Confidence, p.Applied, p.Value, formatTS(now(p.At)))
	return err
}

// ListProvenance returns the provenance rows (applied and attempted).
func (r *Repo) ListProvenance(ctx context.Context, importID string) ([]library.ProvenanceRow, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT field, source, resolver_version, confidence, applied, value, at
		 FROM library_metadata_provenance WHERE import_id = ? ORDER BY id`, importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []library.ProvenanceRow
	for rows.Next() {
		var p library.ProvenanceRow
		var applied int
		var at string
		if err := rows.Scan(&p.Field, &p.Source, &p.ResolverVersion, &p.Confidence, &applied, &p.Value, &at); err != nil {
			return nil, err
		}
		p.Applied = applied != 0
		if p.At, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Source revisions

// PublishRevision allocates and persists atomically under the IMMEDIATE
// transaction (concurrent publishers serialize on the single writer
// slot — the advisory-lock twin).
func (r *Repo) PublishRevision(ctx context.Context, dom library.SourceRevisionDomain) (survivingID int64, minted bool, err error) {
	bib, err := json.Marshal(dom.Bibliography)
	if err != nil {
		return 0, false, err
	}
	loc, err := json.Marshal(dom.LocatorCapabilities)
	if err != nil {
		return 0, false, err
	}
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `
		SELECT revision_id FROM library_source_revisions
		WHERE source_id = ? AND record_id = ? AND rendition_id = ?
		  AND content_hash = ? AND origin = ?`,
		dom.SourceID, dom.RecordID, dom.RenditionID, dom.ContentHash, dom.Origin).Scan(&survivingID)
	if err == nil {
		return survivingID, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	var max int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision_id),0) FROM library_source_revisions WHERE source_id = ? AND record_id = ?`,
		dom.SourceID, dom.RecordID).Scan(&max); err != nil {
		return 0, false, err
	}
	revID := max + 1
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO library_source_revisions (source_id, record_id, rendition_id, revision_id,
			content_hash, media_type, bibliography, locator_capabilities, content_ticket, origin, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		dom.SourceID, dom.RecordID, dom.RenditionID, revID, dom.ContentHash, dom.MediaType,
		string(bib), string(loc), dom.ContentTicket, dom.Origin, formatTS(now(dom.CreatedAt))); err != nil {
		return 0, false, err
	}
	return revID, true, tx.Commit()
}

const revisionSelect = `SELECT source_id, record_id, rendition_id, revision_id, content_hash, media_type,
		bibliography, locator_capabilities, content_ticket, origin, created_at
		FROM library_source_revisions`

func (r *Repo) scanRevision(row *sql.Row) (library.SourceRevisionDomain, error) {
	var d library.SourceRevisionDomain
	var bib, loc string
	var created string
	err := row.Scan(&d.SourceID, &d.RecordID, &d.RenditionID, &d.RevisionID, &d.ContentHash,
		&d.MediaType, &bib, &loc, &d.ContentTicket, &d.Origin, &created)
	if err != nil {
		return library.SourceRevisionDomain{}, absent(err)
	}
	if err := json.Unmarshal([]byte(bib), &d.Bibliography); err != nil {
		return library.SourceRevisionDomain{}, fmt.Errorf("revision bibliography: %w", err)
	}
	if err := json.Unmarshal([]byte(loc), &d.LocatorCapabilities); err != nil {
		return library.SourceRevisionDomain{}, fmt.Errorf("revision locator capabilities: %w", err)
	}
	if d.CreatedAt, err = parseTS(created); err != nil {
		return library.SourceRevisionDomain{}, err
	}
	return d, nil
}

// LatestRevision loads the newest revision of a rendition.
func (r *Repo) LatestRevision(ctx context.Context, sourceID, recordID, renditionID string) (library.SourceRevisionDomain, error) {
	return r.scanRevision(r.read.QueryRowContext(ctx, revisionSelect+`
		WHERE source_id = ? AND record_id = ? AND rendition_id = ?
		ORDER BY revision_id DESC LIMIT 1`, sourceID, recordID, renditionID))
}

// LatestRevisionByTicket resolves a revision row by its content ticket.
func (r *Repo) LatestRevisionByTicket(ctx context.Context, ticket string) (library.SourceRevisionDomain, error) {
	return r.scanRevision(r.read.QueryRowContext(ctx, revisionSelect+`
		WHERE content_ticket = ?
		ORDER BY revision_id DESC LIMIT 1`, ticket))
}

// LatestRevisionByRecord resolves the newest revision of any rendition
// of a record.
func (r *Repo) LatestRevisionByRecord(ctx context.Context, sourceID, recordID string) (library.SourceRevisionDomain, error) {
	return r.scanRevision(r.read.QueryRowContext(ctx, revisionSelect+`
		WHERE source_id = ? AND record_id = ?
		ORDER BY revision_id DESC LIMIT 1`, sourceID, recordID))
}

// ReferencedStagingHashes lists the content hashes imports still
// reference (the retention set).
func (r *Repo) ReferencedStagingHashes(ctx context.Context) (map[string]bool, error) {
	rows, err := r.read.QueryContext(ctx, `SELECT DISTINCT staging_sha256 FROM library_imports`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	referenced := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return referenced, err
		}
		referenced[sha] = true
	}
	return referenced, rows.Err()
}
