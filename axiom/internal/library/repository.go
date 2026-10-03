// repository.go — the Library component's engine-neutral persistence
// contract (F12, #306). Everything the Library application (service,
// provider adapters) needs from durable storage sits behind this
// interface; SQL dialects and engine-specific locking (PostgreSQL
// advisory locks, SQLite IMMEDIATE transactions) live ONLY in the
// implementations:
//
//   - internal/library/pglib   — PostgreSQL (pgx), the 0.1.x-borne engine
//   - internal/library/sqlite  — SQLite (library.sqlite, single file)
//
// The SAME repository contract suite (internal/library/reposuite) runs
// against every engine — parity is a test result, not a claim (CI engine
// matrix). Absent rows surface as ErrRowAbsent from EVERY engine; the
// application maps that sentinel, never a driver error.
//
// The Zotero-mirror read methods are the documented strangler seam
// (F06/F09): the zotero_* mirror tables are Library-owned but live on the
// legacy shared database today. Engines without a mirror (SQLite, a
// library-only database) report absence — the service degrades to the
// honest NotFound, exactly like a fresh PostgreSQL library DB without the
// core schema already does.
package library

import (
	"context"
	"errors"
	"fmt"
	"time"

	library "github.com/Cyb3rDudu/axiom/axiom/internal/contracts/library"
)

// ErrDuplicateKey is the engine-neutral unique-violation sentinel: a
// concurrent identical insert won the race — the caller resolves by
// re-reading (the #293 lesson). Engines translate their driver's
// duplicate-key signal into this (possibly wrapped).
var ErrDuplicateKey = errors.New("library: duplicate key")

// ErrRowAbsent is the engine-neutral absent-row sentinel (the neutral
// twin of a driver's ErrNoRows). Lookups return it (possibly wrapped);
// UpdateImportStatus returns it when the guarded CAS matched nothing
// (the conflict signal — the row moved between read and write).
var ErrRowAbsent = errors.New("library: row absent")

// ---------------------------------------------------------------------------
// Rows (neutral DTOs — shared by every engine implementation)

// ImportRow is the persisted import (the library_imports row).
type ImportRow struct {
	ImportID             string
	IdempotencyKey       string
	PayloadHash          string
	RecordType           string
	RequestJSON          []byte
	Status               library.ImportStatus
	StagingSHA256        string
	StagingSize          int64
	MediaType            string
	FailureCode          string
	FailureMessage       string
	RecordProviderID     string
	RenditionProviderID  string
	CollectionProviderID string
	RecordID             string
	RenditionID          string
	RevisionID           int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// EventRow is one append-only import event.
type EventRow struct {
	Seq    int64
	Kind   string
	Detail []byte // canonical JSON, as persisted
	At     time.Time
}

// StepRow is one saga step's bookkeeping.
type StepRow struct {
	Step        string
	State       string // in_progress | done
	ProviderRef string
	Detail      []byte
	Attempts    int
	UpdatedAt   time.Time
}

// ProvenanceRow is one ladder provenance entry (applied or attempted).
type ProvenanceRow struct {
	Field           string
	Source          string
	ResolverVersion string
	Confidence      float64
	Applied         bool
	Value           string
	At              time.Time
}

// WriteAuditRow is one provider mutation's audit line.
type WriteAuditRow struct {
	Scope       string
	Operation   string
	Anchor      string
	ProviderRef string
	Outcome     string // created | reused | changed | adopted | removed
	Readback    any    // what the readback observed (JSON)
}

// WriterLeaseConflict reports a refused lease acquisition: another live
// writer holds the scope. The diagnosis names owner and heartbeat age.
// Engine-neutral — every engine's AcquireWriterLease refuses with it.
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

// MirrorCitation is the zotero_documents mirror projection the citation
// read assembles its bibliography from (raw mirror truth; the mapping to
// revision.Bibliography is application logic, engine-neutral).
type MirrorCitation struct {
	Title         string
	Publisher     string
	Language      string
	Creators      []byte // the mirror's creator JSON
	Year          *int
	CitationClass string
}

// RevisionPublisher is the Mits-Schrieb surface the existing state-change
// points call (sync completion, heal/custody). nil-safe by convention:
// callers skip a nil publisher. Implemented by the PostgreSQL engine only
// — the legacy Zotero-sync lane reads the mirror on the shared database;
// the SQLite profile does not wire this lane (capability-honest).
type RevisionPublisher interface {
	// RecordSyncRevisions re-publishes revisions for every ACTIVE
	// attachment of the source from the Zotero mirror (sync completion).
	RecordSyncRevisions(ctx context.Context, sourceID string) (int, error)
	// RecordAttachmentRevision publishes one rendition's revision (heal /
	// custody apply): the NEW attachment with its content hash.
	RecordAttachmentRevision(ctx context.Context, sourceID, documentKey, attachmentKey, contentHash, mediaType string) error
}

// Repository is the Library persistence contract. Engines implement ALL
// of it — a capability the engine lacks is reported honestly by the
// method's typed behavior (mirror reads: absence; never emulation), not
// by a narrower interface.
type Repository interface {
	// --- Imports / saga ---------------------------------------------------

	// CreateImport inserts the import row; a concurrent identical insert
	// surfaces as a unique violation the caller resolves by re-reading.
	CreateImport(ctx context.Context, r ImportRow) error
	// GetByIdempotencyKey loads an import row; ErrRowAbsent when absent.
	GetByIdempotencyKey(ctx context.Context, key string) (ImportRow, error)
	// GetImport loads an import row by id; ErrRowAbsent when absent.
	GetImport(ctx context.Context, importID string) (ImportRow, error)
	// UpdateImportStatus advances the state machine and stamps updated_at.
	// A non-empty expect makes the update apply ONLY while the row is
	// still in that status — zero rows affected surfaces as ErrRowAbsent,
	// which the guarded callers map to Conflict. Engine-specific
	// compare-and-swap mechanics live behind this method.
	UpdateImportStatus(ctx context.Context, importID string, status library.ImportStatus,
		expect library.ImportStatus,
		failure *library.ImportFailure, rec, rend, coll, recID, rendID string, revID int64) error
	// ResumeInflightIDs lists imports still in an inflight status (the
	// boot-resume scan: everything not terminal, not awaiting decision).
	ResumeInflightIDs(ctx context.Context) ([]string, error)

	// --- Events (append-only log) ----------------------------------------

	// AppendEvent appends one event with the next per-import seq. The seq
	// allocation is engine-serialized (advisory lock / IMMEDIATE tx) — a
	// concurrent double-drive can never collide on (import, seq).
	AppendEvent(ctx context.Context, importID, kind string, detail any) error
	// ListEvents returns the event log in seq order.
	ListEvents(ctx context.Context, importID string) ([]EventRow, error)

	// --- Steps ------------------------------------------------------------

	// UpsertStep books a step: first write in_progress (attempts=1),
	// re-entry increments attempts, done freezes the state.
	UpsertStep(ctx context.Context, importID, step, state, providerRef string, detail any) error
	// GetStep loads one step row; ErrRowAbsent when absent.
	GetStep(ctx context.Context, importID, step string) (StepRow, error)

	// --- External identifiers ----------------------------------------------

	// ClaimIdentifiers records the normalized DOI/ISBN of a record. A
	// conflict (another record owns the identifier) is a typed Conflict —
	// data state, never auto-merged.
	ClaimIdentifiers(ctx context.Context, recordID, doi, isbn string) error

	// --- Provenance --------------------------------------------------------

	// AppendProvenance writes one provenance row, idempotent per
	// (import, field, source, resolver_version, applied, value).
	AppendProvenance(ctx context.Context, importID string, p ProvenanceRow) error
	// ListProvenance returns the provenance rows (applied and attempted).
	ListProvenance(ctx context.Context, importID string) ([]ProvenanceRow, error)

	// --- Source revisions ---------------------------------------------------

	// PublishRevision allocates the revision id and persists the revision
	// atomically, returning the SURVIVING revision id plus whether THIS
	// call minted it (idempotent re-publish returns the existing row's id,
	// minted=false). Concurrent publishers of one record serialize —
	// engine lock mechanics live here, not in the application.
	PublishRevision(ctx context.Context, r SourceRevisionDomain) (survivingID int64, minted bool, err error)
	// LatestRevision loads the newest revision of a rendition.
	LatestRevision(ctx context.Context, sourceID, recordID, renditionID string) (SourceRevisionDomain, error)
	// LatestRevisionByTicket resolves a revision row by its content ticket.
	LatestRevisionByTicket(ctx context.Context, ticket string) (SourceRevisionDomain, error)
	// LatestRevisionByRecord resolves the newest revision of any rendition
	// of a record.
	LatestRevisionByRecord(ctx context.Context, sourceID, recordID string) (SourceRevisionDomain, error)

	// --- Provider anchors & write audit (F07) -------------------------------

	// LookupProviderAnchor resolves the idempotency anchor to the provider
	// id ("" + ErrRowAbsent when absent).
	LookupProviderAnchor(ctx context.Context, scope, kind, anchor string) (providerID string, version int64, err error)
	// PutProviderAnchor records the anchor; the SURVIVING provider id is
	// returned (the anchor is the dedup winner; the version refreshes).
	PutProviderAnchor(ctx context.Context, scope, kind, anchor, providerID string, version int64) (string, error)
	// PutProviderAnchorWithAudit persists the anchor AND its write-audit
	// row in ONE transaction (the mutation↔audit 1:1 contract holds even
	// against a crash between the two writes).
	PutProviderAnchorWithAudit(ctx context.Context, scope, kind, anchor, providerID string, version int64, audit WriteAuditRow) (string, error)
	// EvictProviderAnchor drops an anchor row whose provider id proved
	// DEAD so the next ensure re-anchors fresh (guarded by provider_id).
	EvictProviderAnchor(ctx context.Context, scope, kind, anchor, providerID string) error
	// AppendWriteAudit records one mutation AFTER its readback verified.
	AppendWriteAudit(ctx context.Context, r WriteAuditRow) error
	// CountWriteAudit counts audit rows for a scope (the 1:1 sonde).
	CountWriteAudit(ctx context.Context, scope string) (int, error)

	// --- Writer lease (single-writer guard, F07) ------------------------------

	// AcquireWriterLease takes the provider-scoped writer lease: fresh
	// insert, or takeover when the current owner's heartbeat is older than
	// ttl. A live owner refuses with *WriterLeaseConflict (typed Conflict).
	// Engines implement this on their own cross-process semantics (the
	// lease row is shared state — PostgreSQL row, SQLite file lock domain).
	AcquireWriterLease(ctx context.Context, scope, owner string, ttl time.Duration) error
	// RenewWriterLease refreshes the heartbeat; a lost lease surfaces as
	// Conflict — the writer must stop.
	RenewWriterLease(ctx context.Context, scope, owner string) error
	// ReleaseWriterLease drops the lease on a graceful stop.
	ReleaseWriterLease(ctx context.Context, scope, owner string) error

	// --- Zotero-mirror reads (strangler seam; absence is honest) --------------

	// LastSyncAt resolves the mirror's last sync time for the source
	// (nil, nil when the source is unknown or no mirror exists).
	LastSyncAt(ctx context.Context, sourceID string) (*time.Time, error)
	// MirrorRenditionPath resolves the local staging path of a mirror
	// rendition ("" + ErrRowAbsent when unknown / no mirror).
	MirrorRenditionPath(ctx context.Context, sourceID, attachmentKey string) (string, error)
	// MirrorCitation loads the mirror's citation projection of a record
	// (nil, false, nil when unknown / no mirror).
	MirrorCitation(ctx context.Context, sourceID, recordID string) (*MirrorCitation, bool, error)

	// --- Staging retention ------------------------------------------------------

	// ReferencedStagingHashes lists the content hashes imports still
	// reference (the retention set; the sweep itself is engine-neutral).
	ReferencedStagingHashes(ctx context.Context) (map[string]bool, error)
}
