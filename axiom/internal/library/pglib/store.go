// store.go — the Library component's PostgreSQL repository (F06 #300,
// F12 #306). One of the two engine implementations of library.Repository
// (the SQLite twin lives in internal/library/sqlite); the SQL dialect
// and PostgreSQL-specific locking (advisory locks) stay in here. The
// saga's durable state: imports, append-only events, step bookkeeping,
// provenance, external identifiers, source revisions.
package pglib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/contracterr"
	contracts "github.com/Cyb3rDudu/axiom/axiom/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Library PostgreSQL repository (a library.Repository).
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a Store over a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// compile-time interface conformance — the PostgreSQL engine implements
// the WHOLE neutral contract.
var _ library.Repository = (*Store)(nil)

// absent translates the driver's no-rows signal into the engine-neutral
// library.ErrRowAbsent — NO-ROWS ONLY: a missing library_* relation is a
// schema fault and stays a raw error (Internal), never masked as data
// absence (F12 review: the widening folded 42P01 here too and turned a
// schema fault into a 404). Mirror-read folding (no-rows ∪ 42P01) is
// mirrorAbsent, used only by the strangler reads.
func absent(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return library.ErrRowAbsent
	}
	return err
}

// isMissingRelation reports a 42P01 (relation absent): the Library
// schema runs standalone (fake providers, no Zotero mirror) — the mirror
// fallbacks treat that as plain absence, never as an internal error.
func isMissingRelation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "42P01"
}

// isBadUUID reports a 22P02 against a uuid parameter: an id that cannot
// even parse is an unknown row, not an internal error.
func isBadUUID(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "22P02"
}

// now returns the DM03-compatible time form (UTC, microsecond-aligned) —
// every produced timestamp goes through here.
func now(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// isUniqueViolation reports a pg unique-violation (23505), optionally
// narrowed to the named constraint.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
		return constraint == "" || strings.Contains(err.Error(), constraint)
	}
	return false
}

// CreateImport inserts the import row; a concurrent identical insert
// surfaces as a unique violation the caller resolves by re-reading.
func (s *Store) CreateImport(ctx context.Context, r library.ImportRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO library_imports (idempotency_key, payload_hash, record_type, request_json,
			status, staging_sha256, staging_size, media_type, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`,
		r.IdempotencyKey, r.PayloadHash, r.RecordType, r.RequestJSON, string(r.Status),
		r.StagingSHA256, r.StagingSize, r.MediaType, now(time.Now()))
	if err != nil && isUniqueViolation(err, "library_imports_idempotency_key") {
		return fmt.Errorf("%w: %v", library.ErrDuplicateKey, err)
	}
	return err
}

// GetByIdempotencyKey loads an import row; ErrRowAbsent when absent.
func (s *Store) GetByIdempotencyKey(ctx context.Context, key string) (library.ImportRow, error) {
	return s.scanImport(s.pool.QueryRow(ctx, s.importSelect()+` WHERE idempotency_key = $1`, key))
}

// GetImport loads an import row by id; absent rows (including a
// syntactically invalid uuid — an unknown import, not an internal error)
// surface as library.ErrRowAbsent.
func (s *Store) GetImport(ctx context.Context, importID string) (library.ImportRow, error) {
	row, err := s.scanImport(s.pool.QueryRow(ctx, s.importSelect()+` WHERE import_id = $1`, importID))
	if isBadUUID(err) {
		return library.ImportRow{}, library.ErrRowAbsent
	}
	return row, absent(err)
}

// importSelect is the NULL-safe import projection (nullable columns
// coalesce to their zero forms).
func (s *Store) importSelect() string {
	return `SELECT import_id, idempotency_key, payload_hash, record_type, request_json, status,
			staging_sha256, staging_size, media_type,
			COALESCE(failure_code,''), COALESCE(failure_message,''),
			COALESCE(record_provider_id,''), COALESCE(rendition_provider_id,''), COALESCE(collection_provider_id,''),
			COALESCE(record_id,''), COALESCE(rendition_id,''), COALESCE(revision_id,0), created_at, updated_at
			FROM library_imports`
}

func (s *Store) scanImport(row pgx.Row) (library.ImportRow, error) {
	var r library.ImportRow
	var status, req string
	err := row.Scan(&r.ImportID, &r.IdempotencyKey, &r.PayloadHash, &r.RecordType, &req, &status,
		&r.StagingSHA256, &r.StagingSize, &r.MediaType, &r.FailureCode, &r.FailureMessage,
		&r.RecordProviderID, &r.RenditionProviderID, &r.CollectionProviderID,
		&r.RecordID, &r.RenditionID, &r.RevisionID, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return library.ImportRow{}, absent(err)
	}
	r.Status = contracts.ImportStatus(status)
	r.RequestJSON = []byte(req)
	return r, nil
}

// UpdateImportStatus advances the state machine and stamps updated_at.
// The optional fields update only their non-zero values (NULL-aware COALESCE).
// expect guards against concurrent drivers ("" = unguarded, used by the
// saga's own single driver): a non-empty expect makes the update apply
// ONLY while the row is still in that status — zero rows affected
// surfaces as library.ErrRowAbsent, which the guarded callers map to Conflict.
// Failure columns are cleared on every transition INTO a non-failed
// status (a retried-then-committed import must not keep its stale
// failure_code in the row).
func (s *Store) UpdateImportStatus(ctx context.Context, importID string, status contracts.ImportStatus,
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
	ct, err := s.pool.Exec(ctx, `
		UPDATE library_imports SET
			status = $2,
			failure_code = CASE WHEN $2 IN ('retryable_failed','terminal_failed')
				THEN COALESCE($3, failure_code) END,
			failure_message = CASE WHEN $2 IN ('retryable_failed','terminal_failed')
				THEN COALESCE($4, failure_message) END,
			record_provider_id = COALESCE(NULLIF($5,''), record_provider_id),
			rendition_provider_id = COALESCE(NULLIF($6,''), rendition_provider_id),
			collection_provider_id = COALESCE(NULLIF($7,''), collection_provider_id),
			record_id = COALESCE(NULLIF($8,''), record_id),
			rendition_id = COALESCE(NULLIF($9,''), rendition_id),
			revision_id = COALESCE($10, revision_id),
			updated_at = $11
		WHERE import_id = $1 AND ($12 = '' OR status = $12)`,
		importID, string(status), fcode, fmsg, rec, rend, coll, recID, rendID, rev, now(time.Now()), string(expect))
	if err != nil {
		if isBadUUID(err) {
			return library.ErrRowAbsent
		}
		return err
	}
	if ct.RowsAffected() == 0 {
		return library.ErrRowAbsent
	}
	return nil
}

// AppendEvent appends one event with the next seq. The seq allocation
// runs under a per-import advisory lock inside one transaction — a
// concurrent double-drive (double-clicked confirm/retry, boot-resume vs
// user retry) can no longer collide on the (import_id, seq) PK and
// surface a 23505 as a spurious Internal.
func (s *Store) AppendEvent(ctx context.Context, importID, kind string, detail any) error {
	d, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('libevt:' || $1))`, importID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO library_import_events (import_id, seq, kind, detail, at)
		VALUES ($1::uuid, (SELECT COALESCE(MAX(seq),0)+1 FROM library_import_events WHERE import_id = $1::uuid), $2, $3, $4)`,
		importID, kind, d, now(time.Now())); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListEvents returns the event log in seq order.
func (s *Store) ListEvents(ctx context.Context, importID string) ([]library.EventRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, kind, detail, at FROM library_import_events WHERE import_id = $1 ORDER BY seq`, importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []library.EventRow
	for rows.Next() {
		var e library.EventRow
		if err := rows.Scan(&e.Seq, &e.Kind, &e.Detail, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpsertStep books a step: first write in_progress (attempts=1),
// re-entry increments attempts (crash/resume visibility), done freezes it.
func (s *Store) UpsertStep(ctx context.Context, importID, step, state, providerRef string, detail any) error {
	d, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO library_import_steps (import_id, step, state, provider_ref, detail, attempts, updated_at)
		VALUES ($1::uuid,$2,$3,$4,$5,1,$6)
		ON CONFLICT (import_id, step) DO UPDATE SET
			state = EXCLUDED.state,
			provider_ref = COALESCE(NULLIF(EXCLUDED.provider_ref,''), library_import_steps.provider_ref),
			detail = EXCLUDED.detail,
			attempts = library_import_steps.attempts + CASE WHEN EXCLUDED.state = 'in_progress' THEN 1 ELSE 0 END,
			updated_at = EXCLUDED.updated_at`,
		importID, step, state, providerRef, d, now(time.Now()))
	return err
}

// GetStep loads one step row ("" step when absent).
func (s *Store) GetStep(ctx context.Context, importID, step string) (library.StepRow, error) {
	var r library.StepRow
	err := s.pool.QueryRow(ctx,
		`SELECT step, state, COALESCE(provider_ref,''), detail, attempts, updated_at
		 FROM library_import_steps WHERE import_id = $1 AND step = $2`, importID, step).
		Scan(&r.Step, &r.State, &r.ProviderRef, &r.Detail, &r.Attempts, &r.UpdatedAt)
	if err != nil {
		return library.StepRow{}, absent(err)
	}
	return r, nil
}

// AppendProvenance writes one provenance row. Idempotent per
// (import, field, source, resolver_version, applied): a resumed resolve
// step re-runs the ladder, and the audit trail must not inflate.
func (s *Store) AppendProvenance(ctx context.Context, importID string, p library.ProvenanceRow) error {
	if p.At.IsZero() {
		p.At = time.Now() // rows and wire timestamps must be real, never 0001-01-01
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM library_metadata_provenance
			WHERE import_id = $1 AND field = $2 AND source = $3
			  AND resolver_version = $4 AND applied = $5 AND value = $6)`,
		importID, p.Field, p.Source, p.ResolverVersion, p.Applied, p.Value).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO library_metadata_provenance (import_id, field, source, resolver_version, confidence, applied, value, at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		importID, p.Field, p.Source, p.ResolverVersion, p.Confidence, p.Applied, p.Value, now(p.At))
	return err
}

// ListProvenance returns the provenance rows (both applied and attempted).
func (s *Store) ListProvenance(ctx context.Context, importID string) ([]library.ProvenanceRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT field, source, resolver_version, confidence, applied, value, at
		 FROM library_metadata_provenance WHERE import_id = $1 ORDER BY id`, importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []library.ProvenanceRow
	for rows.Next() {
		var p library.ProvenanceRow
		if err := rows.Scan(&p.Field, &p.Source, &p.ResolverVersion, &p.Confidence, &p.Applied, &p.Value, &p.At); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ClaimIdentifiers records the normalized DOI/ISBN of a record. A
// conflict (another record owns the identifier) is a loud contract
// Conflict — the caller decides (this is data state, never auto-merged).
func (s *Store) ClaimIdentifiers(ctx context.Context, recordID, doi, isbn string) error {
	for _, id := range []struct{ kind, val string }{
		{"doi", library.NormalizeDOI(doi)},
		{"isbn", library.NormalizeISBN(isbn)},
	} {
		if id.val == "" {
			continue
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO library_external_identifiers (kind, normalized_value, record_id)
			VALUES ($1,$2,$3)
			ON CONFLICT (kind, normalized_value) DO UPDATE SET record_id = EXCLUDED.record_id
			WHERE library_external_identifiers.record_id = EXCLUDED.record_id`,
			id.kind, id.val, recordID)
		if err != nil {
			return err
		}
		var owner string
		if err := s.pool.QueryRow(ctx,
			`SELECT record_id FROM library_external_identifiers WHERE kind = $1 AND normalized_value = $2`,
			id.kind, id.val).Scan(&owner); err != nil {
			return err
		}
		if owner != recordID {
			return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
				fmt.Sprintf("%s %s already belongs to record %s", id.kind, id.val, owner))
		}
	}
	return nil
}

// PublishRevision allocates the revision id and persists the revision in
// ONE transaction under the advisory lock, returning the SURVIVING
// revision id plus whether THIS call minted it:
//   - same (source, record, rendition, content, origin) → the idempotent
//     Mits-Schrieb short-circuit: the existing row's revision_id is
//     returned, minted=false (no history inflation on every sync);
//   - changed content → the next monotonic id, minted=true.
//
// Because allocation and insert commit atomically, the returned id
// ALWAYS references an existing row (a crash-resume re-run returns the
// surviving id instead of stamping a dangling one), and concurrent
// publishers of the same record serialize on the lock instead of racing
// an INSERT … DO NOTHING that could silently drop the loser.
func (s *Store) PublishRevision(ctx context.Context, r library.SourceRevisionDomain) (survivingID int64, minted bool, err error) {
	bib, err := json.Marshal(r.Bibliography)
	if err != nil {
		return 0, false, err
	}
	loc, err := json.Marshal(r.LocatorCapabilities)
	if err != nil {
		return 0, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`, r.SourceID, r.RecordID); err != nil {
		return 0, false, err
	}
	// Idempotence: same (source, record, rendition, content, origin) →
	// keep the existing revision row, report its id. created_at stays
	// the original mint's — an idempotent re-publish must not rewrite
	// history timestamps.
	err = tx.QueryRow(ctx, `
		SELECT revision_id FROM library_source_revisions
		WHERE source_id = $1 AND record_id = $2 AND rendition_id = $3
		  AND content_hash = $4 AND origin = $5`,
		r.SourceID, r.RecordID, r.RenditionID, r.ContentHash, r.Origin).Scan(&survivingID)
	if err == nil {
		return survivingID, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	var max int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(revision_id),0) FROM library_source_revisions WHERE source_id = $1 AND record_id = $2`,
		r.SourceID, r.RecordID).Scan(&max); err != nil {
		return 0, false, err
	}
	revID := max + 1
	// Under the advisory xact lock no concurrent insert for (source,
	// record) can interleave — a plain INSERT (conflicts here would mean
	// a bug, not a race to swallow).
	if _, err := tx.Exec(ctx, `
		INSERT INTO library_source_revisions (source_id, record_id, rendition_id, revision_id,
			content_hash, media_type, bibliography, locator_capabilities, content_ticket, origin, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		r.SourceID, r.RecordID, r.RenditionID, revID, r.ContentHash, r.MediaType,
		bib, loc, r.ContentTicket, r.Origin, now(r.CreatedAt)); err != nil {
		return 0, false, err
	}
	return revID, true, tx.Commit(ctx)
}

// LatestRevision loads the newest revision of a rendition (for ticket
// redemption and citation projection). Empty RecordID when absent.
func (s *Store) LatestRevision(ctx context.Context, sourceID, recordID, renditionID string) (library.SourceRevisionDomain, error) {
	r, err := s.scanRevision(s.pool.QueryRow(ctx, `
		SELECT source_id, record_id, rendition_id, revision_id, content_hash, media_type,
			bibliography, locator_capabilities, content_ticket, origin, created_at
		FROM library_source_revisions
		WHERE source_id = $1 AND record_id = $2 AND rendition_id = $3
		ORDER BY revision_id DESC LIMIT 1`, sourceID, recordID, renditionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return library.SourceRevisionDomain{}, library.ErrRowAbsent
	}
	return r, err
}

// LatestRevisionByTicket resolves a revision row by its content ticket.
func (s *Store) LatestRevisionByTicket(ctx context.Context, ticket string) (library.SourceRevisionDomain, error) {
	r, err := s.scanRevision(s.pool.QueryRow(ctx, `
		SELECT source_id, record_id, rendition_id, revision_id, content_hash, media_type,
			bibliography, locator_capabilities, content_ticket, origin, created_at
		FROM library_source_revisions
		WHERE content_ticket = $1
		ORDER BY revision_id DESC LIMIT 1`, ticket))
	if errors.Is(err, pgx.ErrNoRows) {
		return library.SourceRevisionDomain{}, library.ErrRowAbsent
	}
	return r, err
}

// LatestRevisionByRecord resolves the newest revision of any rendition of
// a record (citation projection entry).
func (s *Store) LatestRevisionByRecord(ctx context.Context, sourceID, recordID string) (library.SourceRevisionDomain, error) {
	r, err := s.scanRevision(s.pool.QueryRow(ctx, `
		SELECT source_id, record_id, rendition_id, revision_id, content_hash, media_type,
			bibliography, locator_capabilities, content_ticket, origin, created_at
		FROM library_source_revisions
		WHERE source_id = $1 AND record_id = $2
		ORDER BY revision_id DESC LIMIT 1`, sourceID, recordID))
	if errors.Is(err, pgx.ErrNoRows) {
		return library.SourceRevisionDomain{}, library.ErrRowAbsent
	}
	return r, err
}

func (s *Store) scanRevision(row pgx.Row) (library.SourceRevisionDomain, error) {
	var r library.SourceRevisionDomain
	var bib, loc []byte
	err := row.Scan(&r.SourceID, &r.RecordID, &r.RenditionID, &r.RevisionID, &r.ContentHash,
		&r.MediaType, &bib, &loc, &r.ContentTicket, &r.Origin, &r.CreatedAt)
	if err != nil {
		return library.SourceRevisionDomain{}, err
	}
	if err := json.Unmarshal(bib, &r.Bibliography); err != nil {
		return library.SourceRevisionDomain{}, fmt.Errorf("revision bibliography: %w", err)
	}
	if err := json.Unmarshal(loc, &r.LocatorCapabilities); err != nil {
		return library.SourceRevisionDomain{}, fmt.Errorf("revision locator capabilities: %w", err)
	}
	return r, nil
}

// ---------------------------------------------------------------------------
