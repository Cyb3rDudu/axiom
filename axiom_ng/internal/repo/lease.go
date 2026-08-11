// Lease protocol repository: atomic claim, fenced post-claim mutations and
// retry scheduling for ingest_jobs (Gate 1 of the dispatcher work package).
// axiom-ng owns all durable state; the processor only computes.
//
// Claim, obsolete-terminalization, lease-exhaustion, cancellation-terminalization
// and frozen-input freeze all happen in one short transaction. Every post-claim
// mutation is fenced by job_id + worker_id + lease_token and returns
// ErrLostLease when the caller no longer owns a valid, unexpired lease.
//
// Time handling: worker-owned fence PREDICATES compare against clock_timestamp()
// so an expiry is honored at statement time even inside a caller-owned transaction
// (MarkCompletedTx). Lease ASSIGNMENT and the short claim/recovery eligibility
// scans intentionally use transaction-stable now(), which is fine for those
// single-statement/short transactions.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxObsoleteSkips bounds how many obsolete/skipped queue-heads one ClaimNextJob
// invocation may drain before yielding, so a burst of stale rows cannot stall
// the poll loop indefinitely. The next poll continues where the bound left off.
const maxObsoleteSkips = 100

// maxRetryDelaySeconds is the documented upper bound for ScheduleRetry backoff;
// larger or negative requests are clamped into [0, maxRetryDelaySeconds].
const maxRetryDelaySeconds = 86400 // 24h

// LeaseRef identifies a claimed job for a fenced mutation. All post-claim
// operations require these three fields.
type LeaseRef struct {
	JobID      string
	WorkerID   string
	LeaseToken string
}

// ErrLostLease is returned when a fenced mutation affects zero rows because the
// caller no longer owns a valid, unexpired lease (reclaimed, expired, terminal,
// or e.g. MarkCompletedTx found the attachment no longer matches the frozen
// input). The caller must stop local state mutation and never persist or
// acknowledge a processor result in response to the original work.
var ErrLostLease = errors.New("lease lost or job not in expected state")

// ClaimOptions supplies the dispatcher-engine parts of a claim: the worker's
// stable id, the lease duration and the processing-profile to freeze. The
// immutable input snapshot is built inside the claim transaction from the locked
// source/document/attachment/canonical state; profile_hash and idempotency_key
// are COMPUTED deterministically in Go, never taken unchecked from the caller.
type ClaimOptions struct {
	WorkerID      string
	LeaseDuration time.Duration
	Profile       json.RawMessage
}

// ClaimedJob is the immutable claim payload returned to a dispatcher so it can
// build a processor request entirely from the frozen snapshot.
type ClaimedJob struct {
	LeaseRef
	Status         string
	Attempt        int
	MaxAttempts    int
	ContentHash    *string
	ForceRebuild   bool
	SourceID       string
	DocumentID     string
	AttachmentID   string
	InputSnapshot  json.RawMessage
	Profile        json.RawMessage
	ProfileHash    *string
	IdempotencyKey *string
}

// execer is the minimal SQL executor shared by pgxpool.Pool and pgx.Tx so the
// fenced-mutation choke point works against both the pool (post-claim autocommit
// transitions) and a caller-owned transaction (MarkCompletedTx).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// DefaultLeaseDuration is used when a call omits ClaimOptions.LeaseDuration;
// conservative and replaced by real config in the dispatcher (Gate 2).
const DefaultLeaseDuration = 120 * time.Second

// ClaimNextJob atomically claims the oldest eligible ingest job for the worker,
// terminalizing stale jobs first, all in one transaction.
//
// The one transaction:
//  1. Terminalizes cancel-requested pending jobs (->cancelled) and expired
//     claimed/processing jobs at attempt exhaustion (->failed/LEASE_EXHAUSTED)
//     and expired cancel-requested jobs (->cancelled).
//  2. Scans for a claimable candidate with FOR UPDATE SKIP LOCKED (FIFO),
//     skipping obsolete candidates (deleted/unpreferred/hash-stale) by marking
//     them skipped in-tx.
//  3. Freezes the immutable input snapshot (built from the locked current
//     source/document/attachment state) plus the processing profile and
//     idempotency key if not already frozen (reclaims keep stored values).
//  4. Increments attempt, assigns a fresh lease_token and owner, and commits.
//
// Returns (nil, nil) when nothing is claimable now. Errors are transactional:
// any failure rolls back the whole pass including all skips/terminalizations.
func (r *Repo) ClaimNextJob(ctx context.Context, opts ClaimOptions) (*ClaimedJob, error) {
	if opts.WorkerID == "" {
		return nil, errors.New("claim: worker id is required")
	}
	leaseDur := opts.LeaseDuration
	if leaseDur <= 0 {
		leaseDur = DefaultLeaseDuration
	}
	leaseSec := int(leaseDur.Seconds())

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if err := r.terminalizeStale(ctx, tx); err != nil {
		return nil, err
	}

	skipped := 0
	var excludedID *string
	for {
		cand, err := claimCandidate(ctx, tx, excludedID)
		if err != nil {
			return nil, err
		}
		if cand == nil {
			break // nothing more claimable this pass
		}

		// Lock and read the job's source/document/attachment/canonical rows in a
		// fixed (deadlock-avoidant) order, then validate obsolescence against the
		// LOCKED state so a snapshot is never built from a row that changed under
		// us or from a mixed transaction snapshot.
		state, reason, err := r.loadAndLockState(ctx, tx, cand)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			if err := r.markObsolete(ctx, tx, cand.id, reason); err != nil {
				return nil, err
			}
			skipped++
			id := cand.id
			excludedID = &id
			if skipped >= maxObsoleteSkips {
				break
			}
			continue
		}

		// Deterministically derive the profile hash and idempotency key in Go,
		// then build the immutable FrozenInput from the locked state.
		profileHash, err := canonicalProfile(opts.Profile)
		if err != nil {
			return nil, err
		}
		idemKey := idempotencyKey(state.attachment.id, state.attachment.contentHash, profileHash, cand.forceRebuild, cand.attempt+1)
		snapshot := buildFrozenInput(cand, state, opts.Profile, profileHash, idemKey)

		var newAttempt int
		var tsToken string
		err = tx.QueryRow(ctx, `
			UPDATE ingest_jobs SET
				status            = 'claimed',
				claimed_by        = $2,
				lease_token       = gen_random_uuid(),
				attempt           = attempt + 1,
				lease_until       = now() + make_interval(secs => $3),
				last_heartbeat_at = now(),
				started_at        = COALESCE(started_at, now()),
				input_snapshot    = COALESCE(input_snapshot, $4::jsonb),
				processing_profile= COALESCE(processing_profile, $5::jsonb),
				profile_hash      = COALESCE(profile_hash, $6),
				idempotency_key   = COALESCE(idempotency_key, $7),
				updated_at        = now()
			WHERE id = $1
			RETURNING attempt, lease_token::text
		`, cand.id, opts.WorkerID, leaseSec, snapshot, opts.Profile, profileHash, idemKey).
			Scan(&newAttempt, &tsToken)
		if err != nil {
			return nil, fmt.Errorf("claim update: %w", err)
		}

		inputSnapshot, profile, storedHash, storedIdem, err := frozenFields(ctx, tx, cand.id)
		if err != nil {
			return nil, err
		}

		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &ClaimedJob{
			LeaseRef:       LeaseRef{JobID: cand.id, WorkerID: opts.WorkerID, LeaseToken: tsToken},
			Status:         "claimed",
			Attempt:        newAttempt,
			MaxAttempts:    cand.maxAttempts,
			ContentHash:    cand.contentHash,
			ForceRebuild:   cand.forceRebuild,
			SourceID:       state.source.id,
			DocumentID:     state.document.id,
			AttachmentID:   state.attachment.id,
			InputSnapshot:  inputSnapshot,
			Profile:        profile,
			ProfileHash:    storedHash,
			IdempotencyKey: storedIdem,
		}, nil
	}

	// Nothing claimable: commit the batch of terminalizations/skips done in this
	// pass so they are durable, then signal the caller to poll again.
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

// terminalizeStale marks jobs that cannot proceed as terminal, exactly once, in
// the caller's transaction:
//   - pending + cancel-requested                              -> cancelled
//   - claimed/processing + cancel-requested + lease expired   -> cancelled
//   - claimed/processing + lease expired + attempt >= max     -> failed LEASE_EXHAUSTED
//   - pending + already at attempt >= max                     -> failed RETRY_EXHAUSTED
//
// These rows are only ever terminal here; a concurrent renewed/reclaimed job
// still has a future lease_until, so the expiry filter keeps it untouched.
func (r *Repo) terminalizeStale(ctx context.Context, tx pgx.Tx) error {
	stmts := []string{
		`UPDATE ingest_jobs SET status='cancelled', claimed_by=NULL, lease_token=NULL, lease_until=NULL,
		          completed_at=now(), updated_at=now()
		 WHERE status='pending' AND cancel_requested_at IS NOT NULL`,
		`UPDATE ingest_jobs SET status='cancelled', claimed_by=NULL, lease_token=NULL, lease_until=NULL,
		          completed_at=COALESCE(completed_at, now()), updated_at=now()
		 WHERE status IN ('claimed','processing') AND cancel_requested_at IS NOT NULL
		   AND lease_until IS NOT NULL AND lease_until <= now()`,
		`UPDATE ingest_jobs SET status='failed', error_code='LEASE_EXHAUSTED',
		          error_message='attempt limit reached',
		          claimed_by=NULL, lease_token=NULL, lease_until=NULL,
		          completed_at=COALESCE(completed_at, now()), updated_at=now()
		 WHERE status IN ('claimed','processing') AND cancel_requested_at IS NULL
		   AND lease_until IS NOT NULL AND lease_until <= now()
		   AND attempt >= max_attempts`,
	}
	// A genuine defect state needs draining: an exhausted pending row (attempt
	// consumed on claim) is unclaimable and would sit at the head forever. No
	// pending row with attempt>=max_attempts is a legitimate retry under the claim
	// predicate, so clear it with or without a next_attempt_at timestamp (old
	// retry/release paths may have left one).
	stmts = append(stmts,
		`UPDATE ingest_jobs SET status='failed', error_code='RETRY_EXHAUSTED',
		          error_message='attempt limit reached',
		          claimed_by=NULL, lease_token=NULL, lease_until=NULL,
		          next_attempt_at=NULL,
		          completed_at=COALESCE(completed_at, now()), updated_at=now()
		 WHERE status='pending'
		   AND attempt >= max_attempts AND cancel_requested_at IS NULL`,
	)
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("terminalize stale: %w", err)
		}
	}
	return nil
}

// claimCandidate locks and returns the oldest still-claimable JOB (FOR UPDATE
// OF j SKIP LOCKED) excluding a just-skipped id. It locks only the ingest_jobs
// row; the dependent source/document/attachment/canonical rows are locked and
// read separately by loadAndLockState in a fixed order. Returns (nil, nil) when
// nothing is claimable.
func claimCandidate(ctx context.Context, tx pgx.Tx, excludedID *string) (*candidate, error) {
	filter := ""
	args := []any{}
	if excludedID != nil {
		filter = ` AND j.id <> $1`
		args = append(args, *excludedID)
	}
	sql := `
		SELECT j.id::text, j.attempt, j.max_attempts, j.content_hash, j.force_rebuild,
		       COALESCE(j.source_id::text, '') AS src_id,
		       COALESCE(j.document_id::text, '') AS doc_id,
		       COALESCE(j.attachment_id::text, '') AS att_id
		FROM ingest_jobs j
		WHERE j.status IN ('pending','claimed','processing')
		  AND j.cancel_requested_at IS NULL
		  AND j.attempt < j.max_attempts
		  AND (
		        (j.status='pending' AND (j.next_attempt_at IS NULL OR j.next_attempt_at<=now()))
		     OR (j.status IN ('claimed','processing') AND j.lease_until IS NOT NULL AND j.lease_until<=now())
		  )` + filter + `
		ORDER BY j.enqueued_at ASC
		FOR UPDATE OF j SKIP LOCKED
		LIMIT 1`

	var c candidate
	err := tx.QueryRow(ctx, sql, args...).Scan(
		&c.id, &c.attempt, &c.maxAttempts, &c.contentHash, &c.forceRebuild,
		&c.sourceID, &c.documentID, &c.attachmentID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim candidate: %w", err)
	}
	return &c, nil
}

type candidate struct {
	id           string
	attempt      int
	maxAttempts  int
	contentHash  *string
	forceRebuild bool
	sourceID     string
	documentID   string
	attachmentID string
}

// frozenState is the locked-and-read source/document/attachment/canonical state
// for a claim. Every field is read WITH the row locked in the claim transaction,
// so the snapshot built from it is a consistent point-in-time view (never a mix
// of rows changed concurrently by a sync).
type frozenState struct {
	source     zoteroSourceRow
	document   zoteroDocRow
	attachment zoteroAttachRow
}

type zoteroSourceRow struct {
	id       string
	serverID *string
}

type zoteroDocRow struct {
	id              string
	zoteroKey       string
	zoteroVersion   int64
	canonicalItemID *string
	itemType        string
	title           *string
	creators        json.RawMessage
	abstract        *string
	publicationYear *int
	publicationDate *string
	publisher       *string
	isbn            *string
	doi             *string
	url             *string
	language        *string
	tags            json.RawMessage
	collections     json.RawMessage
	metadata        map[string]any
	deleted         bool
	relativePath    *string
	parentKey       *string
	linkMode        *string
	attachmentID    *string
	rawData         json.RawMessage
}

type zoteroAttachRow struct {
	id            string
	zoteroKey     string
	zoteroVersion int64
	contentType   *string
	filename      *string
	localPath     *string
	contentHash   *string
	fileSize      *int64
	mtimeMS       *int64
	preferred     bool
	deleted       bool
}

// loadAndLockState locks and reads the job's source, document, attachment and
// the document's canonical item rows in a FIXED order (deadlock-avoidant):
//
//  1. zotero_sources
//  2. zotero_documents
//  3. zotero_attachments
//  4. zotero_items (document's canonical item)
//
// Every worker takes these locks in this same order and only after its own
// ingest_jobs row is already locked (from claimCandidate's FOR UPDATE OF j).
//
// Returns a non-empty obsolete reason when any dependent row is missing,
// deleted/unpreferred or hash-stale, so no FrozenInput is ever built from a
// broken or mid-flight state.
func (r *Repo) loadAndLockState(ctx context.Context, tx pgx.Tx, c *candidate) (*frozenState, string, error) {
	s := &frozenState{}

	// 1. / 2. Source then document.
	if c.sourceID == "" || c.documentID == "" {
		return nil, "PARENT_REMOVED", nil
	}
	err := tx.QueryRow(ctx, `
		SELECT id::text, server_id FROM zotero_sources WHERE id=$1 FOR UPDATE`, c.sourceID).Scan(
		&s.source.id, &s.source.serverID)
	if err == pgx.ErrNoRows {
		return nil, "PARENT_REMOVED", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("lock source: %w", err)
	}

	var docParentKey, docLinkMode *string
	err = tx.QueryRow(ctx, `
		SELECT id::text, zotero_key, zotero_version, canonical_item_id::text,
		       item_type, title, creators, abstract_note, publication_year,
		       publication_date, publisher, isbn, doi, url, language,
		       tags, collections, metadata, deleted
		FROM zotero_documents WHERE id=$1 FOR UPDATE`, c.documentID).Scan(
		&s.document.id, &s.document.zoteroKey, &s.document.zoteroVersion, &s.document.canonicalItemID,
		&s.document.itemType, &s.document.title, &s.document.creators, &s.document.abstract, &s.document.publicationYear,
		&s.document.publicationDate, &s.document.publisher, &s.document.isbn, &s.document.doi, &s.document.url, &s.document.language,
		&s.document.tags, &s.document.collections, &s.document.metadata, &s.document.deleted)
	if err == pgx.ErrNoRows {
		return nil, "PARENT_REMOVED", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("lock document: %w", err)
	}
	if s.document.deleted {
		return nil, "ATTACHMENT_REMOVED", nil
	}

	// 3. Attachment (parent_zotero_key + link_mode come from the attachment row).
	if c.attachmentID == "" {
		return nil, "PARENT_REMOVED", nil
	}
	err = tx.QueryRow(ctx, `
		SELECT id::text, zotero_key, zotero_version, content_type, filename, local_path,
		       content_hash, file_size, mtime_ms, preferred, deleted, parent_zotero_key, link_mode
		FROM zotero_attachments WHERE id=$1 FOR UPDATE`, c.attachmentID).Scan(
		&s.attachment.id, &s.attachment.zoteroKey, &s.attachment.zoteroVersion,
		&s.attachment.contentType, &s.attachment.filename, &s.attachment.localPath,
		&s.attachment.contentHash, &s.attachment.fileSize, &s.attachment.mtimeMS,
		&s.attachment.preferred, &s.attachment.deleted,
		&docParentKey, &docLinkMode)
	if err == pgx.ErrNoRows {
		return nil, "ATTACHMENT_REMOVED", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("lock attachment: %w", err)
	}
	if s.attachment.deleted {
		return nil, "ATTACHMENT_REMOVED", nil
	}
	if !s.attachment.preferred {
		return nil, "ATTACHMENT_NOT_PREFERRED", nil
	}
	// Check job-vs-current hash consistency exactly as the previous obsoleteReason
	// did. For a non-forced rebuild:
	//   - current hash present and differs from the job hash (or job hash NULL) -> CHANGED
	//   - current hash missing but the job expects one                        -> MISSING
	//
	// A forced rebuild deliberately ignores the stale job-level hash.
	if !c.forceRebuild {
		switch {
		case s.attachment.contentHash != nil && (c.contentHash == nil || *s.attachment.contentHash != *c.contentHash):
			return nil, "CONTENT_HASH_CHANGED", nil
		case s.attachment.contentHash == nil && c.contentHash != nil:
			return nil, "CONTENT_HASH_MISSING", nil
		}
	}
	s.document.parentKey, s.document.linkMode = docParentKey, docLinkMode

	// 4. Document's canonical item raw_data (lossless bibliographic source).
	if s.document.canonicalItemID != nil {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT raw_data FROM zotero_items WHERE id=$1 FOR UPDATE`, *s.document.canonicalItemID).Scan(&raw); err == nil {
			s.document.rawData = raw
		}
		// ErrNoRows (canonical item absent) is tolerated: normalized projection
		// columns still populate the snapshot.
	}
	return s, "", nil
}

// buildFrozenInput assembles the durable immutable snapshot for a claim from the
// locked state, the canonicalized profile and the derived hashes/keys.
func buildFrozenInput(c *candidate, s *frozenState, profile json.RawMessage, profileHash, idemKey string) []byte {
	fi := FrozenInput{
		ContractVersion: "1.0",
		JobID:           c.id,
		IdempotencyKey:  idemKey,
		Source: FrozenSource{
			Type:     "zotero",
			SourceID: s.source.id,
			ServerID: s.source.serverID,
		},
		Document: FrozenDocument{
			DocumentID:    s.document.id,
			ZoteroKey:     s.document.zoteroKey,
			ZoteroVersion: s.document.zoteroVersion,
			MetadataSnapshot: metadataSnapshot(s.document.rawData, zoteroDocFacts{
				ItemType:        s.document.itemType,
				Title:           s.document.title,
				Creators:        s.document.creators,
				Abstract:        s.document.abstract,
				PublicationYear: s.document.publicationYear,
				PublicationDate: s.document.publicationDate,
				Publisher:       s.document.publisher,
				ISBN:            s.document.isbn,
				DOI:             s.document.doi,
				URL:             s.document.url,
				Language:        s.document.language,
				Tags:            s.document.tags,
				Collections:     s.document.collections,
				Metadata:        s.document.metadata,
			}),
		},
		Attachment: FrozenAttachment{
			AttachmentID:  s.attachment.id,
			ZoteroKey:     s.attachment.zoteroKey,
			ZoteroVersion: s.attachment.zoteroVersion,
			ParentKey:     derefStr(s.document.parentKey),
			LinkMode:      derefStr(s.document.linkMode),
			ContentType:   s.attachment.contentType,
			Filename:      s.attachment.filename,
			LocalPath:     s.attachment.localPath,
			ContentHash:   s.attachment.contentHash,
			SizeBytes:     s.attachment.fileSize,
			MtimeMS:       s.attachment.mtimeMS,
		},
		Processing: FrozenProcessing{
			Profile:      string(profile),
			ForceRebuild: c.forceRebuild,
			ProfileHash:  profileHash,
		},
	}
	b, _ := json.Marshal(fi)
	return b
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// frozenFields reads back the COALESCE-resolved frozen input after a claim so
// both first-claim and reclaim return the actual stored values (a reclaim
// returns the earlier freeze, never the fresh opts).
func frozenFields(ctx context.Context, tx pgx.Tx, jobID string) (json.RawMessage, json.RawMessage, *string, *string, error) {
	var snap, prof []byte
	var ph, ik *string
	err := tx.QueryRow(ctx, `
		SELECT input_snapshot::text, COALESCE(processing_profile::text,''), profile_hash, idempotency_key
		FROM ingest_jobs WHERE id=$1`, jobID).Scan(&snap, &prof, &ph, &ik)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read frozen fields: %w", err)
	}
	return json.RawMessage(snap), json.RawMessage(prof), ph, ik, nil
}

// markObsolete transitions a job to skipped with a stable reason in the caller's
// transaction and clears any lease it held. It is always committed with the
// enclosing claim pass (never left dangling by an early return).
func (r *Repo) markObsolete(ctx context.Context, tx pgx.Tx, jobID, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE ingest_jobs SET
			status='skipped', error_code='SKIPPED', error_message=$2,
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE id=$1
	`, jobID, reason)
	return err
}

// fencedUpdate runs an UPDATE whose $1,$2,$3 are job_id, claimed_by and
// lease_token (the fencing predicate) and returns whether it affected a row.
// Most post-claim mutations route through this so a single zero-row guard
// catches a lost lease for all callers; ScheduleRetry returns an outcome and so
// handles its zero-row case with a QueryRow RETURNING instead. The
// expected-status filter is written as SQL literals in each statement. `ex` may
// be the pool (autocommit) or a caller-owned transaction (MarkCompletedTx).
func (r *Repo) fencedUpdate(ctx context.Context, ex execer, sqlStmt string, ref LeaseRef, args ...any) (bool, error) {
	tag, err := ex.Exec(ctx, sqlStmt,
		append([]any{ref.JobID, ref.WorkerID, ref.LeaseToken}, args...)...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RenewLease extends the lease_until and heartbeat for a claimed job using
// database time. Returns ErrLostLease if the job is no longer owned OR its
// lease has expired (an expired token is lost even before recovery/reclaim).
func (r *Repo) RenewLease(ctx context.Context, ref LeaseRef, duration time.Duration) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			lease_until = now() + make_interval(secs => $4),
			last_heartbeat_at = now(),
			updated_at = now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
		  AND lease_until IS NOT NULL AND lease_until > clock_timestamp()
	`, ref, int(duration.Seconds()))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("renew lease: %w", ErrLostLease)
	}
	return nil
}

// MarkProcessing advances a claimed job to processing after processor acceptance.
func (r *Repo) MarkProcessing(ctx context.Context, ref LeaseRef) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET status='processing', updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3 AND status='claimed'
		  AND lease_until IS NOT NULL AND lease_until > clock_timestamp()
	`, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark processing: %w", ErrLostLease)
	}
	return nil
}

// RetryOutcome distinguishes a scheduled retry from a final-attempt exhaustion.
type RetryOutcome int

const (
	// RetryScheduled means the job was returned to pending for a later attempt.
	RetryScheduled RetryOutcome = iota
	// RetryExhausted means this was the last attempt and the job terminalized as
	// failed/RETRY_EXHAUSTED.
	RetryExhausted
)

func (o RetryOutcome) String() string {
	if o == RetryExhausted {
		return "retry_exhausted"
	}
	return "retry_scheduled"
}

// ScheduleRetry returns a claimed/processing job to pending for a later retry
// with bounded backoff, clamped into [0, maxRetryDelaySeconds]; the DB stores
// now()+delay so no sleeps happen inside a transaction or while holding a lease.
//
// If the job has reached max_attempts (this is the last attempt), the job
// becomes terminal failed/RETRY_EXHAUSTED instead of pending, with the lease
// cleared, completed_at set and next_attempt_at NULL, so no stranded pending
// row can be produced at the attempt ceiling.
func (r *Repo) ScheduleRetry(ctx context.Context, ref LeaseRef, errorCode, errorMessage string, nextDelaySeconds int) (RetryOutcome, error) {
	if nextDelaySeconds < 0 {
		nextDelaySeconds = 0
	}
	if nextDelaySeconds > maxRetryDelaySeconds {
		nextDelaySeconds = maxRetryDelaySeconds
	}
	var status string
	err := r.pool.QueryRow(ctx, `
		UPDATE ingest_jobs SET
			status = CASE WHEN attempt >= max_attempts THEN 'failed'::ingest_job_status ELSE 'pending'::ingest_job_status END,
			next_attempt_at = CASE WHEN attempt >= max_attempts THEN NULL
			                      ELSE now() + make_interval(secs => $6) END,
			error_code = CASE WHEN attempt >= max_attempts THEN 'RETRY_EXHAUSTED' ELSE $4 END,
			error_message = $5,
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at = CASE WHEN attempt >= max_attempts THEN COALESCE(completed_at, now()) END,
			updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
		  AND lease_until IS NOT NULL AND lease_until > clock_timestamp()
		RETURNING status
	`, ref.JobID, ref.WorkerID, ref.LeaseToken, errorCode, errorMessage, nextDelaySeconds).Scan(&status)
	if err == pgx.ErrNoRows {
		return 0, fmt.Errorf("schedule retry: %w", ErrLostLease)
	}
	if err != nil {
		return 0, fmt.Errorf("schedule retry: %w", err)
	}
	if status == "failed" {
		return RetryExhausted, nil
	}
	return RetryScheduled, nil
}

// MarkFailed sets a terminal failed state, clearing the lease.
func (r *Repo) MarkFailed(ctx context.Context, ref LeaseRef, errorCode, errorMessage string) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			status='failed', error_code=$4, error_message=$5,
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
		  AND lease_until IS NOT NULL AND lease_until > clock_timestamp()
	`, ref, errorCode, errorMessage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark failed: %w", ErrLostLease)
	}
	return nil
}

// MarkCompletedTx finalises a job as completed inside a CALLER-OWNED
// transaction, alongside the durable processing-snapshot persistence (Gate 4
// persists the snapshot and completes the job in one commit). It re-fences with
// owner+lease+unexpired and re-validates the current attachment: still not
// deleted, still preferred, and its CURRENT hash unchanged from the hash frozen
// in input_snapshot at claim time (forced-rebuild jobs included). A job whose
// frozen snapshot has no content_hash cannot be completed. A rollback of the
// caller's transaction rolls the completion back with it.
func (r *Repo) MarkCompletedTx(ctx context.Context, tx pgx.Tx, ref LeaseRef, processorName, processorVersion, snapshotID string) error {
	ok, err := r.fencedUpdate(ctx, tx, `
		UPDATE ingest_jobs j SET
			status='completed',
			processor_name=$4, processor_version=$5,
			result=COALESCE(result,'{}')::jsonb || jsonb_build_object('snapshot_id',$6::text),
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE j.id=$1 AND j.claimed_by=$2 AND j.lease_token=$3 AND j.status='processing'
		  AND j.lease_until IS NOT NULL AND j.lease_until > clock_timestamp()
		  AND EXISTS (
		        SELECT 1 FROM zotero_attachments a
		        WHERE a.id = j.attachment_id
		          AND a.deleted = false AND a.preferred = true
		          AND j.input_snapshot IS NOT NULL
		          AND (j.input_snapshot->'attachment'->>'content_hash') IS NOT NULL
		          AND (j.input_snapshot->'attachment'->>'content_hash') = a.content_hash
		      )
	`, ref, processorName, processorVersion, snapshotID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark completed: %w", ErrLostLease)
	}
	return nil
}

// MarkCancelled sets a terminal cancelled state, clearing the lease.
func (r *Repo) MarkCancelled(ctx context.Context, ref LeaseRef) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			status='cancelled',
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
		  AND lease_until IS NOT NULL AND lease_until > clock_timestamp()
	`, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark cancelled: %w", ErrLostLease)
	}
	return nil
}

// MarkSkipped sets a terminal skipped state with a reason, clearing the lease.
func (r *Repo) MarkSkipped(ctx context.Context, ref LeaseRef, reason string) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			status='skipped', error_code='SKIPPED', error_message=$4,
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
		  AND lease_until IS NOT NULL AND lease_until > clock_timestamp()
	`, ref, reason)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark skipped: %w", ErrLostLease)
	}
	return nil
}

// RequestCancellation converges a non-terminal job to cancelled. It is
// idempotent and does not require ownership (an operator may cancel a job any
// worker holds). A PENDING job is cancelled in the same SQL operation; a
// claimed/processing job has its cancellation request recorded and is converged
// by the dispatcher (which observes cancel_requested_at).
func (r *Repo) RequestCancellation(ctx context.Context, jobID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE ingest_jobs SET
			status = CASE WHEN status = 'pending' THEN 'cancelled'::ingest_job_status ELSE status END,
			cancel_requested_at = COALESCE(cancel_requested_at, now()),
			claimed_by = CASE WHEN status = 'pending' THEN NULL ELSE claimed_by END,
			lease_token = CASE WHEN status = 'pending' THEN NULL ELSE lease_token END,
			completed_at = CASE WHEN status = 'pending' THEN COALESCE(completed_at, now()) END,
			updated_at=now()
		WHERE id=$1 AND status NOT IN ('completed','failed','cancelled','skipped')
	`, jobID)
	return err
}

// ReleaseOrExpireLease clears the lease fields for an UNEXPIRED job owned by ref
// so in-flight work survives a graceful shutdown without being lost. When the job
// has consumed its max attempts it is terminalized to failed/RETRY_EXHAUSTED
// instead of being returned to pending (a pending row at the attempt ceiling
// would be stranded, as it can neither be claimed nor would normal retry push it
// further). Otherwise it returns to pending, due now.
// returns to pending, due now.
func (r *Repo) ReleaseOrExpireLease(ctx context.Context, ref LeaseRef) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			status = CASE WHEN attempt >= max_attempts THEN 'failed'::ingest_job_status ELSE 'pending'::ingest_job_status END,
			next_attempt_at = CASE WHEN attempt >= max_attempts THEN NULL ELSE now() END,
			error_code = CASE WHEN attempt >= max_attempts THEN 'RETRY_EXHAUSTED' ELSE error_code END,
			error_message = CASE WHEN attempt >= max_attempts THEN 'attempt limit reached' ELSE error_message END,
			completed_at = CASE WHEN attempt >= max_attempts THEN COALESCE(completed_at, now()) END,
			updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
		  AND lease_until IS NOT NULL AND lease_until > clock_timestamp()
	`, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("release lease: %w", ErrLostLease)
	}
	return nil
}

// GetJob returns the full current row for a job.
func (r *Repo) GetJob(ctx context.Context, jobID string) (*Job, error) {
	var j Job
	err := r.pool.QueryRow(ctx, `
		SELECT id::text, source_id, document_id, attachment_id,
		       status::text, content_hash, attempt, max_attempts, error_code,
		       error_message, resolved_at::text, enqueued_at::text
		FROM ingest_jobs WHERE id=$1
	`, jobID).Scan(&j.ID, &j.SourceID, &j.DocumentID, &j.AttachmentID,
		&j.Status, &j.ContentHash, &j.Attempt, &j.MaxAttempts,
		&j.ErrorCode, &j.ErrorMessage, &j.ResolvedAt, &j.EnqueuedAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("get job %s: %w", jobID, pgx.ErrNoRows)
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// RecoverExpiredJobs reclaims expired claimed/processing jobs synchronously and
// atomically, distinguishing the three outcomes in ONE transaction with row
// locking so it never clobbers a parallel renew/reclaim/completion:
//
//   - cancel-requested now (lease expired)     -> cancelled
//   - attempt >= max_attempts now               -> failed LEASE_EXHAUSTED
//   - otherwise (reclaimable)                   -> pending, due now
//
// Because FOR UPDATE SKIP LOCKED pins each row, a concurrent claimer that took
// the row first is simply skipped here; a renewed lease no longer matches the
// expiry filter.
func (r *Repo) RecoverExpiredJobs(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id FROM ingest_jobs
		WHERE status IN ('claimed','processing')
		  AND lease_until IS NOT NULL AND lease_until <= now()
		ORDER BY enqueued_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT $1
	`, limit)
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(ids) == 0 {
		// Nothing to recover; commit the (empty) transaction.
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// Per-row classified update. The row is still locked in this transaction, so
	// the three statements below cannot race a concurrent claimer; each one is
	// fully guarded by its own WHERE.
	cancelled := 0
	exhausted := 0
	recovered := 0
	for _, id := range ids {
		tag, err := tx.Exec(ctx, `UPDATE ingest_jobs SET status='cancelled',
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
			WHERE id=$1 AND status IN ('claimed','processing')
			  AND cancel_requested_at IS NOT NULL`, id)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			cancelled++
			continue
		}
		tag, err = tx.Exec(ctx, `UPDATE ingest_jobs SET status='failed',
			error_code='LEASE_EXHAUSTED', error_message='attempt limit reached',
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
			WHERE id=$1 AND status IN ('claimed','processing')
			  AND cancel_requested_at IS NULL AND attempt >= max_attempts`, id)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			exhausted++
			continue
		}
		tag, err = tx.Exec(ctx, `UPDATE ingest_jobs SET status='pending',
			next_attempt_at=now(),
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			updated_at=now()
			WHERE id=$1 AND status IN ('claimed','processing')
			  AND cancel_requested_at IS NULL AND attempt < max_attempts`, id)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			recovered++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return recovered, nil
}
