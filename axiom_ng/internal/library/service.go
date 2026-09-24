// service.go — LibraryService: the F03 Library implementation (F06, #300)
// and its durable import saga. Every step persists BEFORE and AFTER its
// provider effect; provider ids are the dedup anchors. A process death
// between any two points resumes into EXACTLY ONE committed artifact set
// (kill/resume table tests prove it) — no automatic DELETE compensation;
// Teilzustände bleiben mit erzeugten Provider-IDs als reparierbar stehen.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/jackc/pgx/v5"
)

// HardImportByteCap is the hard ceiling for import content — config may
// lower it, never raise it (the issue's "size limit configurable,
// hard-capped").
const HardImportByteCap = int64(2 << 30) // 2 GiB

// DefaultImportByteLimit is the configured default (512 MiB).
const DefaultImportByteLimit = int64(512 << 20)

// Config carries the constructed service configuration (env-free).
type Config struct {
	SourceID       string
	Provider       string
	LibraryID      string
	MaxImportBytes int64 // <=0 → DefaultImportByteLimit; clamped to HardImportByteCap
}

// Service is the Library application service.
type Service struct {
	cfg     Config
	store   *Store
	staging *Staging
	ports   Ports
	haltMu  sync.Mutex
	halt    haltBook // test-only crash simulation
}

// haltBook simulates process death at saga seams (kill/resume DoD).
type haltBook struct {
	afterState         map[string]int // state name → pending trips
	afterProviderWrite map[string]int // step name → pending trips
}

// ErrHaltSimulated is the crash sentinel: the saga stopped exactly where
// the halt book tripped; NOTHING after that point was written.
var ErrHaltSimulated = errors.New("halt simulated (process death)")

func (s *Service) tripAfterState(state string) bool {
	s.haltMu.Lock()
	defer s.haltMu.Unlock()
	if s.halt.afterState[state] > 0 {
		s.halt.afterState[state]--
		return true
	}
	return false
}

func (s *Service) tripAfterProviderWrite(step string) bool {
	s.haltMu.Lock()
	defer s.haltMu.Unlock()
	if s.halt.afterProviderWrite[step] > 0 {
		s.halt.afterProviderWrite[step]--
		return true
	}
	return false
}

// NewService builds the service. Ports may be partially nil — the
// corresponding operations report Unavailable (capability-honest).
func NewService(cfg Config, store *Store, staging *Staging, ports Ports) *Service {
	if cfg.MaxImportBytes <= 0 {
		cfg.MaxImportBytes = DefaultImportByteLimit
	}
	if cfg.MaxImportBytes > HardImportByteCap {
		cfg.MaxImportBytes = HardImportByteCap
	}
	if cfg.Provider == "" {
		cfg.Provider = "unconfigured"
	}
	return &Service{cfg: cfg, store: store, staging: staging, ports: ports}
}

// ---------------------------------------------------------------------------
// F03 Library interface

// GetSource resolves the configured source. SyncedAt delegates thinly to
// the zotero mirror when the source id matches a synced zotero source.
func (s *Service) GetSource(ctx context.Context, ref library.SourceRef) (library.Source, error) {
	if ref.SourceID == "" {
		return library.Source{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source ref is blank")
	}
	if ref.SourceID != s.cfg.SourceID {
		return library.Source{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "source "+ref.SourceID)
	}
	src := library.Source{SourceID: s.cfg.SourceID, Provider: s.cfg.Provider, LibraryID: s.cfg.LibraryID}
	// Thin read delegation onto the existing Zotero mirror (strangler:
	// same physical DB, no behavior change).
	var synced *time.Time
	if t, err := s.zoteroSyncedAt(ctx, ref.SourceID); err == nil && t != nil {
		synced = t
	}
	src.SyncedAt = synced
	return src, nil
}

func (s *Service) zoteroSyncedAt(ctx context.Context, sourceID string) (*time.Time, error) {
	var t *time.Time
	err := s.store.pool.QueryRow(ctx,
		`SELECT last_sync_at FROM zotero_sources WHERE id::text = $1`, sourceID).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) || isMissingRelation(err) {
		return nil, nil
	}
	return t, err
}

// StartImport begins a document intake. Validation (including magic-byte
// derivation) precedes idempotency — a reused key with invalid input is
// InvalidArgument, never a mismatch. The saga runs synchronously to its
// first stopping point (terminal or awaiting_confirmation).
func (s *Service) StartImport(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, error) {
	op, _, err := s.StartImportDetailed(ctx, req, content)
	return op, err
}

// MaxImportBytes reports the configured content limit (the HTTP layer
// bounds the multipart body with it).
func (s *Service) MaxImportBytes() int64 { return s.cfg.MaxImportBytes }

// StartImportDetailed is StartImport with replay visibility for the HTTP
// binding (202 for a fresh intake, 200 for an idempotent replay).
func (s *Service) StartImportDetailed(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, bool, error) {
	// 1. Request validation.
	if req.IdempotencyKey == "" {
		return library.ImportOperation{}, false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "idempotency_key is required")
	}
	if req.RecordType == "" {
		return library.ImportOperation{}, false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "record_type is required")
	}
	if req.Target.CollectionID != "" && len(req.Target.CollectionPath) > 0 {
		return library.ImportOperation{}, false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "collection_id and collection_path are mutually exclusive")
	}
	if req.RecordType == "webpage" && (req.Source == nil || req.Source.OriginalURL == "") {
		return library.ImportOperation{}, false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "webpage imports require source.original_url")
	}

	// 2. Content validation — magic bytes ONLY, before any state exists.
	b, err := io.ReadAll(io.LimitReader(content, s.cfg.MaxImportBytes+1))
	if err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, err, "reading import content")
	}
	if int64(len(b)) > s.cfg.MaxImportBytes {
		return library.ImportOperation{}, false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument,
			fmt.Sprintf("import content exceeds the configured limit (%d bytes)", s.cfg.MaxImportBytes))
	}
	media, err := mediaTypeFromMagic(b)
	if err != nil {
		return library.ImportOperation{}, false, err
	}

	// 3. Payload identity: canonical request JSON + content bytes.
	meta, err := json.Marshal(req)
	if err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "canonicalizing import request")
	}
	payload := revision.HashContent(append(append([]byte{}, meta...), b...))

	// 4. Idempotency (AFTER validation — the contract precedence).
	if prior, err := s.store.GetByIdempotencyKey(ctx, req.IdempotencyKey); err == nil && prior.ImportID != "" {
		if prior.PayloadHash != payload {
			return library.ImportOperation{}, false, &contracterr.IdempotencyMismatch{Component: contracterr.ComponentLibrary, Key: req.IdempotencyKey}
		}
		op, oerr := s.operation(ctx, prior)
		return op, true, oerr
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "idempotency lookup")
	}

	// 5. Staging (hashed file; descriptor rides the import row).
	sha, err := s.staging.StoreImport(b)
	if err != nil {
		return library.ImportOperation{}, false, err
	}

	// 6. Durable row (received) — crash-safe from here on.
	if err := s.store.CreateImport(ctx, ImportRow{
		IdempotencyKey: req.IdempotencyKey,
		PayloadHash:    payload,
		RecordType:     req.RecordType,
		RequestJSON:    meta,
		Status:         library.ImportReceived,
		StagingSHA256:  sha,
		StagingSize:    int64(len(b)),
		MediaType:      media,
	}); err != nil {
		if isUniqueViolation(err, "library_imports_idempotency_key") {
			// Concurrent identical intake won the race — replay it.
			prior, rerr := s.store.GetByIdempotencyKey(ctx, req.IdempotencyKey)
			if rerr == nil && prior.ImportID != "" {
				if prior.PayloadHash != payload {
					return library.ImportOperation{}, false, &contracterr.IdempotencyMismatch{Component: contracterr.ComponentLibrary, Key: req.IdempotencyKey}
				}
				op, oerr := s.operation(ctx, prior)
				return op, true, oerr
			}
		}
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "creating import row")
	}
	row, err := s.store.GetByIdempotencyKey(ctx, req.IdempotencyKey)
	if err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "loading created import")
	}
	if err := s.store.AppendEvent(ctx, row.ImportID, "state_entered", map[string]any{"status": string(library.ImportReceived)}); err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "event append")
	}
	if s.tripAfterState(string(library.ImportReceived)) {
		// Simulated crash right after the durable row exists (the
		// kill/resume "received" seam) — the row IS the state; boot
		// recovery resumes it.
		return library.ImportOperation{}, false, ErrHaltSimulated
	}

	// 7. Run the saga to its first stopping point. A failed saga is a
	// POLLED outcome (the operation carries the failure), not a
	// StartImport error — errors are for request-level refusals. Only
	// the crash sentinel propagates (tests assert it).
	if err := s.advance(ctx, row.ImportID); err != nil {
		if errors.Is(err, ErrHaltSimulated) {
			return library.ImportOperation{}, false, err
		}
		// Real saga failure stays a polled outcome.
	}
	final, err := s.store.GetImport(ctx, row.ImportID)
	if err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "loading final import")
	}
	op, oerr := s.operation(ctx, final)
	return op, false, oerr
}

// GetImport reports the current operation state.
func (s *Service) GetImport(ctx context.Context, ref library.ImportRef) (library.ImportOperation, error) {
	if ref.ImportID == "" {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "import ref is blank")
	}
	row, err := s.store.GetImport(ctx, ref.ImportID)
	if errors.Is(err, pgx.ErrNoRows) || isBadUUID(err) {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "import "+ref.ImportID)
	}
	if err != nil {
		return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "loading import")
	}
	return s.operation(ctx, row)
}

// OpenRendition redeems a content ticket. Import revisions redeem from
// hashed staging ("lst:<sha>" tickets); Zotero-mirror Mitschrieb
// revisions redeem from the mirrored local path ("zat:<source>:<key>").
func (s *Service) OpenRendition(ctx context.Context, ticket library.ContentTicket) (io.ReadCloser, error) {
	t := string(ticket)
	if t == "" {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "content ticket is blank")
	}
	switch {
	case strings.HasPrefix(t, "lst:"):
		return s.staging.Open(strings.TrimPrefix(t, "lst:"))
	case strings.HasPrefix(t, "zat:"):
		parts := strings.SplitN(strings.TrimPrefix(t, "zat:"), ":", 2)
		if len(parts) != 2 {
			return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "malformed zotero ticket")
		}
		var local string
		err := s.store.pool.QueryRow(ctx,
			`SELECT local_path FROM zotero_attachments WHERE source_id::text = $1 AND zotero_key = $2 AND deleted = false`,
			parts[0], parts[1]).Scan(&local)
		if errors.Is(err, pgx.ErrNoRows) || isMissingRelation(err) || (err == nil && (local == "" || !fileExists(local))) {
			return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "rendition unknown or expired")
		}
		if err != nil {
			return nil, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "zotero rendition lookup")
		}
		return os.Open(local)
	}
	// Ticket-shaped like a published revision ticket? Resolve by table.
	rev, err := s.store.LatestRevisionByTicket(ctx, t)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "content ticket unknown or expired")
	}
	if err != nil {
		return nil, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "ticket lookup")
	}
	if strings.HasPrefix(rev.ContentTicket, "lst:") {
		return s.staging.Open(strings.TrimPrefix(rev.ContentTicket, "lst:"))
	}
	return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "content ticket unknown or expired")
}

// ProjectCitation composes the citation projection (initial thin form —
// the full projection is F07/R17). The record resolves via its published
// revision, falling back to the Zotero mirror read model.
func (s *Service) ProjectCitation(ctx context.Context, req library.CitationRequest) (library.CitationProjection, error) {
	if req.RecordID == "" {
		return library.CitationProjection{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "record id is blank")
	}
	switch req.Locator.Kind {
	case "page", "epub_cfi":
	default:
		return library.CitationProjection{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "locator kind "+req.Locator.Kind+" is not citable")
	}
	bib, err := s.bibliographyFor(ctx, req.RecordID)
	if err != nil {
		return library.CitationProjection{}, err
	}
	year := "n.d."
	if bib.Year != nil {
		year = fmt.Sprintf("%d", *bib.Year)
	}
	author := "Unknown"
	if len(bib.Authors) > 0 {
		a := bib.Authors[0]
		if sp := strings.LastIndex(a, " "); sp > 0 && sp < len(a)-1 {
			a = a[sp+1:]
		}
		author = a
	}
	locus := ""
	if req.Locator.Kind == "page" && req.Locator.PageStart != nil {
		locus = fmt.Sprintf(", S. %d", *req.Locator.PageStart)
	}
	style := req.Style
	if style == "" {
		style = "apa-7"
	}
	return library.CitationProjection{
		RecordID:  req.RecordID,
		Citation:  fmt.Sprintf("(%s, %s%s)", author, year, locus),
		Reference: fmt.Sprintf("%s (%s). %s. %s.", author, year, bib.Title, bib.Publisher),
		Style:     style,
		Locator:   req.Locator,
	}, nil
}

// bibliographyFor: published revision first, then the Zotero mirror read
// model (thin delegation, strangler).
func (s *Service) bibliographyFor(ctx context.Context, recordID string) (revision.Bibliography, error) {
	rev, err := s.store.LatestRevisionByRecord(ctx, s.cfg.SourceID, recordID)
	if err == nil {
		return rev.Bibliography, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return revision.Bibliography{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "revision lookup")
	}
	var bib revision.Bibliography
	var creators []byte
	var year *int
	var title, publisher, language, class string
	err = s.store.pool.QueryRow(ctx, `
		SELECT d.title, COALESCE(d.publisher,''), COALESCE(d.language,''), d.creators, d.publication_year,
			COALESCE(d.citation_class, 'citable')
		FROM zotero_documents d
		WHERE d.zotero_key = $1 AND d.source_id::text = $2 AND d.deleted = false`,
		recordID, s.cfg.SourceID).Scan(&title, &publisher, &language, &creators, &year, &class)
	if errors.Is(err, pgx.ErrNoRows) {
		return revision.Bibliography{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "record "+recordID)
	}
	if isMissingRelation(err) {
		return revision.Bibliography{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "record "+recordID)
	}
	if err != nil {
		return revision.Bibliography{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "zotero record lookup")
	}
	bib = revision.Bibliography{
		RecordID: recordID, Title: title, Publisher: publisher, Language: language,
		Year: year, CitationClass: class,
	}
	if len(creators) > 0 {
		var cs []Creator
		if json.Unmarshal(creators, &cs) == nil {
			for _, c := range cs {
				bib.Authors = append(bib.Authors, strings.TrimSpace(strings.TrimSpace(c.FirstName)+" "+c.LastName))
			}
		}
	}
	return bib, nil
}

// ---------------------------------------------------------------------------
// Confirm / Retry (the F06 decision surface; HTTP-exposed)

// ConfirmImport resolves an awaiting_confirmation decision with the
// chosen candidate and continues the saga.
func (s *Service) ConfirmImport(ctx context.Context, importID, decisionID, candidateID string) (library.ImportOperation, error) {
	if importID == "" {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "import id is blank")
	}
	row, err := s.store.GetImport(ctx, importID)
	if errors.Is(err, pgx.ErrNoRows) {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "import "+importID)
	}
	if err != nil {
		return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "loading import")
	}
	if row.Status != library.ImportAwaitingConfirm {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
			"import is "+string(row.Status)+", not awaiting_confirmation")
	}
	dec, err := s.openDecision(ctx, row)
	if err != nil {
		return library.ImportOperation{}, err
	}
	if dec == nil {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInternal, "awaiting_confirmation without an open decision")
	}
	if decisionID == "" || dec.DecisionID != decisionID {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument,
			"unknown decision "+decisionID+" (open: "+dec.DecisionID+")")
	}

	// Fold the choice into the persisted resolve state — the persisted
	// step detail carries the candidates WITH fields (the event log is
	// the audit view).
	det, err := s.loadResolveDetail(ctx, row.ImportID)
	if err != nil {
		return library.ImportOperation{}, err
	}
	if det.Pending == nil || det.Pending.DecisionID != decisionID {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInternal,
			"awaiting_confirmation without the persisted decision detail")
	}
	var chosen *DecisionCandidate
	for i := range det.Pending.Candidates {
		if det.Pending.Candidates[i].CandidateID == candidateID {
			chosen = &det.Pending.Candidates[i]
			break
		}
	}
	if chosen == nil {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument,
			"unknown candidate "+candidateID+" for decision "+decisionID)
	}
	switch decisionID {
	case "dec-bibliography":
		// The user-confirmed candidate's carried fields win (deliberate
		// choice — the automatic-rung lock does not apply). The dedup
		// re-check runs BEFORE the choice is finalized: an ambiguous
		// outcome must surface as the NEXT decision (dec-duplicate),
		// never strand the import in awaiting_confirmation forever.
		det.Merged = chosen.Fields
		det.RecordType = firstNonEmpty(chosen.Fields.RecordType, det.RecordType)
		if s.ports.Catalog == nil {
			return library.ImportOperation{}, s.ports.unavailable("CatalogReader")
		}
		plan, dupDec, err := DedupScan(ctx, s.ports.Catalog, catalogRecordFrom(det), row.StagingSHA256)
		if err != nil {
			return library.ImportOperation{}, err
		}
		if dupDec != nil {
			// The confirmed fields match MULTIPLE existing records: the
			// folded choice stays persisted (Pending = the duplicate
			// question) and the caller resolves dec-duplicate next.
			det.Plan = plan
			det.Pending = dupDec
			if err := s.persistResolveDetail(ctx, row, det, "in_progress"); err != nil {
				return library.ImportOperation{}, err
			}
			if err := s.store.AppendEvent(ctx, row.ImportID, "decision_resolved", map[string]any{
				"decision_id": decisionID, "candidate_id": candidateID,
			}); err != nil {
				return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "event append")
			}
			if err := s.offerDecision(ctx, row.ImportID, dupDec); err != nil {
				return library.ImportOperation{}, err
			}
			return s.GetImport(ctx, library.ImportRef{ImportID: importID})
		}
		det.Plan = plan
		// The chosen candidate's fields become applied provenance — only
		// now that the outcome is known (the scan above could have
		// rerouted to a duplicate decision). Source "user" is the
		// contract vocabulary for a deliberate choice; the rung the
		// candidate was offered from rides ResolverVersion.
		for _, f := range ladderFields {
			if v, ok := fieldOf(chosen.Fields, f); ok {
				if err := s.store.AppendProvenance(ctx, row.ImportID, ProvenanceRow{
					Field: f, Source: "user", ResolverVersion: chosen.Origin, Confidence: 1.0, Applied: true, Value: v,
				}); err != nil {
					return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "provenance append")
				}
			}
		}
	default: // dec-duplicate: the chosen record is the link target
		det.Plan.LinkProviderRecordID = chosen.CandidateID
		det.Plan.AddRendition = true
		if existing, cerr := s.catalogRecord(ctx, chosen.CandidateID); cerr == nil && existing != nil {
			if id := findRendition(*existing, row.StagingSHA256); id != "" {
				det.Plan.AddRendition = false
				det.Plan.ExistingAttachmentID = id
			}
		}
	}
	det.Pending = nil
	if err := s.persistResolveDetail(ctx, row, det, "done"); err != nil {
		return library.ImportOperation{}, err
	}
	if err := s.store.AppendEvent(ctx, row.ImportID, "decision_resolved", map[string]any{
		"decision_id": decisionID, "candidate_id": candidateID,
	}); err != nil {
		return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "event append")
	}
	if err := s.enterFrom(ctx, row.ImportID, library.ImportAwaitingConfirm, library.ImportEnsuringCollections); err != nil {
		return library.ImportOperation{}, err
	}
	if err := s.advance(ctx, row.ImportID); err != nil && !errors.Is(err, ErrHaltSimulated) {
		return library.ImportOperation{}, err
	}
	return s.GetImport(ctx, library.ImportRef{ImportID: importID})
}

// RetryImport continues a retryable_failed import from its failed state.
func (s *Service) RetryImport(ctx context.Context, importID string) (library.ImportOperation, error) {
	if importID == "" {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "import id is blank")
	}
	row, err := s.store.GetImport(ctx, importID)
	if errors.Is(err, pgx.ErrNoRows) {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "import "+importID)
	}
	if err != nil {
		return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "loading import")
	}
	if row.Status != library.ImportRetryableFailed {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
			"import is "+string(row.Status)+" — only retryable_failed steps continue")
	}
	from, err := s.failedFromState(ctx, row)
	if err != nil {
		return library.ImportOperation{}, err
	}
	if err := s.store.AppendEvent(ctx, importID, "retry", map[string]any{"from": from}); err != nil {
		return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "event append")
	}
	if err := s.enterFrom(ctx, importID, library.ImportRetryableFailed, library.ImportStatus(from)); err != nil {
		return library.ImportOperation{}, err
	}
	if err := s.advance(ctx, importID); err != nil && !errors.Is(err, ErrHaltSimulated) {
		return library.ImportOperation{}, err
	}
	return s.GetImport(ctx, library.ImportRef{ImportID: importID})
}

// ResumeInflight is the boot-time crash recovery: imports left in a
// RUNNING state (a process death between steps) continue from their
// persisted progress. awaiting_confirmation stays untouched — that stop
// is a caller decision, not a crash. Returns the resumed import ids.
func (s *Service) ResumeInflight(ctx context.Context) ([]string, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT import_id FROM library_imports
		WHERE status NOT IN ('committed','retryable_failed','terminal_failed','awaiting_confirmation')`)
	if err != nil {
		return nil, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "inflight scan")
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ids, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "inflight scan")
		}
		ids = append(ids, id)
	}
	var errs []error
	for _, id := range ids {
		if err := s.advance(ctx, id); err != nil && !errors.Is(err, ErrHaltSimulated) {
			// One broken import must not starve the others of their
			// resume — collect and keep walking (the caller logs the join).
			errs = append(errs, fmt.Errorf("resume %s: %w", id, err))
		}
	}
	return ids, errors.Join(errs...)
}

// failedFromState reads the terminal event's from_state (where the saga
// died — retry resumes THERE, not from scratch).
func (s *Service) failedFromState(ctx context.Context, row ImportRow) (string, error) {
	events, err := s.store.ListEvents(ctx, row.ImportID)
	if err != nil {
		return "", contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "event read")
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "terminal" {
			var d struct {
				FromState string `json:"from_state"`
			}
			if json.Unmarshal(events[i].Detail, &d) == nil && d.FromState != "" {
				return d.FromState, nil
			}
		}
	}
	return string(library.ImportReceived), nil
}

// ---------------------------------------------------------------------------
// The durable saga

// step names (library_import_steps.step).
const (
	stepInspect     = "inspecting"
	stepResolve     = "resolving_metadata"
	stepCollections = "ensuring_collections"
	stepCreateRec   = "creating_record"
	stepUpload      = "uploading_rendition"
	stepVerify      = "verifying"
)

// resolveDetail is the persisted ladder + plan state of the resolve step.
type resolveDetail struct {
	DocFields  ResolvedFields `json:"doc_fields"`
	Merged     ResolvedFields `json:"merged"`
	RecordType string         `json:"record_type"`
	HintDOI    string         `json:"hint_doi,omitempty"`
	HintISBN   string         `json:"hint_isbn,omitempty"`
	Plan       PlacementPlan  `json:"plan"`
	Pending    *Decision      `json:"pending,omitempty"`
}

func (s *Service) loadResolveDetail(ctx context.Context, importID string) (resolveDetail, error) {
	var det resolveDetail
	step, err := s.store.GetStep(ctx, importID, stepResolve)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(step.Detail) == 0) {
		return det, nil
	}
	if err != nil {
		return det, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "resolve step read")
	}
	if err := json.Unmarshal(step.Detail, &det); err != nil {
		return det, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "resolve step decode")
	}
	return det, nil
}

func (s *Service) persistResolveDetail(ctx context.Context, row ImportRow, det resolveDetail, state string) error {
	step, err := s.store.GetStep(ctx, row.ImportID, stepResolve)
	if errors.Is(err, pgx.ErrNoRows) {
		step = StepRow{}
		err = nil
	}
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "resolve step read")
	}
	return s.store.UpsertStep(ctx, row.ImportID, stepResolve, state, step.ProviderRef, det)
}

// enter transitions the state machine and logs the event (unguarded —
// the saga's own single driver uses this).
func (s *Service) enter(ctx context.Context, importID string, status library.ImportStatus) error {
	return s.enterFrom(ctx, importID, "", status)
}

// enterFrom is enter with a concurrency guard: the transition applies
// only while the row is still in `from` ("" = unguarded). The second of
// two racing confirms/retries loses with Conflict instead of
// double-driving the saga.
func (s *Service) enterFrom(ctx context.Context, importID string, from, status library.ImportStatus) error {
	if err := s.store.UpdateImportStatus(ctx, importID, status, from, nil, "", "", "", "", "", 0); err != nil {
		if from != "" && errors.Is(err, pgx.ErrNoRows) {
			return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
				"import state changed concurrently (expected "+string(from)+")")
		}
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "state transition")
	}
	if err := s.store.AppendEvent(ctx, importID, "state_entered", map[string]any{"status": string(status)}); err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "event append")
	}
	if s.tripAfterState(string(status)) {
		return ErrHaltSimulated
	}
	return nil
}

// fail marks the saga failed with the contract class deciding retryable.
func (s *Service) fail(ctx context.Context, importID string, from library.ImportStatus, err error) error {
	class, ok := contracterr.ClassOf(err)
	if !ok {
		class = contracterr.ClassInternal
	}
	status := library.ImportTerminalFailed
	if class == contracterr.ClassUnavailable || class == contracterr.ClassDeadline || class == contracterr.ClassInternal {
		status = library.ImportRetryableFailed
	}
	f := &library.ImportFailure{Code: strings.ToUpper(string(class)), Message: err.Error()}
	if cerr := s.store.UpdateImportStatus(ctx, importID, status, "", f, "", "", "", "", "", 0); cerr != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, cerr, "fail transition")
	}
	_ = s.store.AppendEvent(ctx, importID, "terminal", map[string]any{
		"from_state": string(from), "code": f.Code, "message": f.Message,
	})
	return err
}

// advance drives the saga to its next stopping point. Idempotent: every
// step re-checks its persisted progress first.
func (s *Service) advance(ctx context.Context, importID string) error {
	for {
		row, err := s.store.GetImport(ctx, importID)
		if err != nil {
			return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "saga load")
		}
		switch row.Status {
		case library.ImportReceived:
			if err := s.enter(ctx, importID, library.ImportInspecting); err != nil {
				return err
			}

		case library.ImportInspecting:
			done, err := s.runInspect(ctx, row)
			if errors.Is(err, ErrHaltSimulated) {
				return err // simulated crash: NOTHING may be persisted
			}
			if err != nil {
				return s.fail(ctx, importID, row.Status, err)
			}
			if done {
				if err := s.enter(ctx, importID, library.ImportResolvingMetadata); err != nil {
					return err
				}
			} else {
				return nil // halted
			}

		case library.ImportResolvingMetadata:
			done, err := s.runResolve(ctx, row)
			if errors.Is(err, ErrHaltSimulated) {
				return err // simulated crash: NOTHING may be persisted
			}
			if err != nil {
				return s.fail(ctx, importID, row.Status, err)
			}
			if done {
				if err := s.enter(ctx, importID, library.ImportEnsuringCollections); err != nil {
					return err
				}
			} else {
				return nil // awaiting_confirmation (or halted)
			}

		case library.ImportEnsuringCollections:
			done, err := s.runCollections(ctx, row)
			if errors.Is(err, ErrHaltSimulated) {
				return err // simulated crash: NOTHING may be persisted
			}
			if err != nil {
				return s.fail(ctx, importID, row.Status, err)
			}
			if done {
				if err := s.enter(ctx, importID, library.ImportCreatingRecord); err != nil {
					return err
				}
			} else {
				return nil
			}

		case library.ImportCreatingRecord:
			done, err := s.runCreateRecord(ctx, row)
			if errors.Is(err, ErrHaltSimulated) {
				return err // simulated crash: NOTHING may be persisted
			}
			if err != nil {
				return s.fail(ctx, importID, row.Status, err)
			}
			if done {
				if err := s.enter(ctx, importID, library.ImportUploadingRendition); err != nil {
					return err
				}
			} else {
				return nil
			}

		case library.ImportUploadingRendition:
			done, err := s.runUpload(ctx, row)
			if errors.Is(err, ErrHaltSimulated) {
				return err // simulated crash: NOTHING may be persisted
			}
			if err != nil {
				return s.fail(ctx, importID, row.Status, err)
			}
			if done {
				if err := s.enter(ctx, importID, library.ImportVerifying); err != nil {
					return err
				}
			} else {
				return nil
			}

		case library.ImportVerifying:
			done, err := s.runVerify(ctx, row)
			if errors.Is(err, ErrHaltSimulated) {
				return err // simulated crash: NOTHING may be persisted
			}
			if err != nil {
				return s.fail(ctx, importID, row.Status, err)
			}
			if done {
				if err := s.enter(ctx, importID, library.ImportCommitted); err != nil {
					return err
				}
			} else {
				return nil
			}

		default:
			return nil // committed / failed / awaiting: stopping points
		}
	}
}

// runInspect — ladder rung 1: document content.
func (s *Service) runInspect(ctx context.Context, row ImportRow) (bool, error) {
	step, err := s.store.GetStep(ctx, row.ImportID, stepInspect)
	if err == nil && step.State == "done" {
		return true, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if s.ports.Documents == nil {
		return false, s.ports.unavailable("DocumentInspector")
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepInspect, "in_progress", "", nil); err != nil {
		return false, err
	}
	doc, err := s.ports.Documents.Inspect(ctx, row.MediaType, s.staging.Path(row.StagingSHA256))
	if err != nil {
		return false, err
	}
	if s.tripAfterProviderWrite(stepInspect) {
		return false, ErrHaltSimulated
	}
	return true, s.store.UpsertStep(ctx, row.ImportID, stepInspect, "done", "", doc)
}

// runResolve — ladder rungs 2–4 + duplicate scan; pauses on decisions.
func (s *Service) runResolve(ctx context.Context, row ImportRow) (bool, error) {
	step, err := s.store.GetStep(ctx, row.ImportID, stepResolve)
	if err == nil && step.State == "done" {
		return true, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var req library.ImportRequest
	if err := json.Unmarshal(row.RequestJSON, &req); err != nil {
		return false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "request decode")
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepResolve, "in_progress", "", nil); err != nil {
		return false, err
	}

	doc, err := s.loadDocFields(ctx, row.ImportID)
	if err != nil {
		return false, err
	}
	enrich := struct{ crossref, openLibrary bool }{true, true}
	if req.Enrichment.Crossref != nil {
		enrich.crossref = *req.Enrichment.Crossref
	}
	if req.Enrichment.OpenLibrary != nil {
		enrich.openLibrary = *req.Enrichment.OpenLibrary
	}
	hints := struct{ DOI, ISBN, Title string }{req.MetadataHints.DOI, req.MetadataHints.ISBN, req.MetadataHints.Title}

	out, err := runLadder(ctx, s.ports, row.RecordType, doc, hints, enrich)
	if err != nil {
		return false, err
	}
	if out.Ambiguous != nil {
		det := resolveDetail{DocFields: doc, Merged: out.Merged, RecordType: row.RecordType, HintDOI: hints.DOI, HintISBN: hints.ISBN, Pending: out.Ambiguous}
		if err := s.persistProvenance(ctx, row.ImportID, out.Provenance); err != nil {
			return false, err
		}
		if err := s.persistResolveDetail(ctx, row, det, "in_progress"); err != nil {
			return false, err
		}
		if err := s.offerDecision(ctx, row.ImportID, out.Ambiguous); err != nil {
			return false, err
		}
		return false, nil
	}

	// Duplicate detection before every write — full catalog.
	if s.ports.Catalog == nil {
		return false, s.ports.unavailable("CatalogReader")
	}
	plan, dup, err := DedupScan(ctx, s.ports.Catalog,
		catalogIncoming(row.RecordType, out.Merged, hints.DOI, hints.ISBN), row.StagingSHA256)
	if err != nil {
		return false, err
	}
	if dup != nil {
		det := resolveDetail{DocFields: doc, Merged: out.Merged, RecordType: row.RecordType, HintDOI: hints.DOI, HintISBN: hints.ISBN, Plan: plan, Pending: dup}
		if err := s.persistProvenance(ctx, row.ImportID, out.Provenance); err != nil {
			return false, err
		}
		if err := s.persistResolveDetail(ctx, row, det, "in_progress"); err != nil {
			return false, err
		}
		if err := s.offerDecision(ctx, row.ImportID, dup); err != nil {
			return false, err
		}
		return false, nil
	}

	det := resolveDetail{DocFields: doc, Merged: out.Merged, RecordType: row.RecordType, HintDOI: hints.DOI, HintISBN: hints.ISBN, Plan: plan}
	if err := s.persistProvenance(ctx, row.ImportID, out.Provenance); err != nil {
		return false, err
	}
	if s.tripAfterProviderWrite(stepResolve) {
		_ = s.persistResolveDetail(ctx, row, det, "in_progress")
		return false, ErrHaltSimulated
	}
	return true, s.persistResolveDetail(ctx, row, det, "done")
}

// runCollections — placement resolution (parent-first, idempotent).
func (s *Service) runCollections(ctx context.Context, row ImportRow) (bool, error) {
	step, err := s.store.GetStep(ctx, row.ImportID, stepCollections)
	if err == nil && step.State == "done" {
		return true, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var req library.ImportRequest
	if err := json.Unmarshal(row.RequestJSON, &req); err != nil {
		return false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "request decode")
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepCollections, "in_progress", "", nil); err != nil {
		return false, err
	}
	collID := ""
	switch {
	case req.Target.CollectionID != "":
		collID = req.Target.CollectionID // addressed directly
	case len(req.Target.CollectionPath) > 0:
		if s.ports.Collections == nil {
			return false, s.ports.unavailable("CollectionWriter")
		}
		id, err := s.ports.Collections.ResolvePath(ctx, req.Target.CollectionPath, req.Target.CreateMissing)
		if err != nil {
			if errors.Is(err, ErrProviderConflict) {
				return false, siblingConflictError(strings.Join(req.Target.CollectionPath, "/"))
			}
			return false, err
		}
		collID = id
	}
	if s.tripAfterProviderWrite(stepCollections) {
		return false, ErrHaltSimulated
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepCollections, "done", collID, nil); err != nil {
		return false, err
	}
	if collID != "" {
		if err := s.store.UpdateImportStatus(ctx, row.ImportID, row.Status, "", nil, "", "", collID, "", "", 0); err != nil {
			return false, err
		}
	}
	return true, nil
}

// runCreateRecord — ensure the record (or link the dedup target).
func (s *Service) runCreateRecord(ctx context.Context, row ImportRow) (bool, error) {
	step, err := s.store.GetStep(ctx, row.ImportID, stepCreateRec)
	if err == nil && step.State == "done" && step.ProviderRef != "" {
		return true, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	det, err := s.loadResolveDetail(ctx, row.ImportID)
	if err != nil {
		return false, err
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepCreateRec, "in_progress", "", nil); err != nil {
		return false, err
	}
	var providerID string
	if det.Plan.LinkProviderRecordID != "" {
		providerID = det.Plan.LinkProviderRecordID // dedup/confirmed link
	} else {
		if s.ports.Records == nil {
			return false, s.ports.unavailable("RecordWriter")
		}
		id, err := s.ports.Records.EnsureRecord(ctx, RecordDraft{
			// Import-stable external key: same key+payload can never
			// create a second record even across a lost-ACK crash.
			ExternalKey: "imp-" + row.IdempotencyKey + "-" + row.PayloadHash[:12],
			RecordType:  det.RecordType,
			Title:       det.Merged.Title,
			Authors:     authorsFromMerged(det.Merged),
			Year:        det.Merged.Year,
			Publisher:   det.Merged.Publisher,
			Language:    det.Merged.Language,
			DOI:         det.Merged.DOI,
			ISBN:        det.Merged.ISBN,
		})
		if err != nil {
			return false, err
		}
		providerID = id
	}
	// Register the normalized identifiers against their record — the
	// unique rule makes a second record claiming the same DOI/ISBN loud
	// (terminal; the provider Teilzustand stays repairable, no delete
	// compensation).
	if err := s.store.ClaimIdentifiers(ctx, providerID, det.Merged.DOI, det.Merged.ISBN); err != nil {
		return false, err
	}
	if s.tripAfterProviderWrite(stepCreateRec) {
		return false, ErrHaltSimulated
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepCreateRec, "done", providerID, nil); err != nil {
		return false, err
	}
	if err := s.store.AppendEvent(ctx, row.ImportID, "provider_write", map[string]any{
		"step": stepCreateRec, "provider_ref": providerID,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// runUpload — ensure the rendition + membership.
func (s *Service) runUpload(ctx context.Context, row ImportRow) (bool, error) {
	step, err := s.store.GetStep(ctx, row.ImportID, stepUpload)
	if err == nil && step.State == "done" {
		return true, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	recStep, err := s.store.GetStep(ctx, row.ImportID, stepCreateRec)
	if err != nil || recStep.ProviderRef == "" {
		return false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInternal, "creating_record step not done before upload")
	}
	det, err := s.loadResolveDetail(ctx, row.ImportID)
	if err != nil {
		return false, err
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepUpload, "in_progress", "", nil); err != nil {
		return false, err
	}

	attID := det.Plan.ExistingAttachmentID
	if det.Plan.AddRendition {
		if s.ports.Renditions == nil {
			return false, s.ports.unavailable("RenditionWriter")
		}
		// Schema filename from VERIFIED metadata (naming.go), honoring
		// the grown pattern of an existing record's attachments.
		var existing []string
		if det.Plan.LinkProviderRecordID != "" {
			if rec, cerr := s.catalogRecord(ctx, det.Plan.LinkProviderRecordID); cerr == nil && rec != nil {
				for _, a := range rec.Renditions {
					existing = append(existing, a.Filename)
				}
			}
		}
		filename := SchemaFilenameForFormat(authorsFromMerged(det.Merged), yearValue(det.Merged.Year), det.Merged.Title, row.MediaType, existing)
		id, err := s.ports.Renditions.EnsureRendition(ctx, RenditionDraft{
			ParentProviderID: recStep.ProviderRef,
			ContentHash:      row.StagingSHA256,
			MediaType:        row.MediaType,
			Filename:         filename,
			StagingPath:      s.staging.Path(row.StagingSHA256),
		})
		if err != nil {
			return false, err
		}
		attID = id
		if s.tripAfterProviderWrite(stepUpload) {
			return false, ErrHaltSimulated
		}
	}

	if row.CollectionProviderID != "" {
		if s.ports.Renditions == nil {
			return false, s.ports.unavailable("RenditionWriter")
		}
		if err := s.ports.Renditions.EnsureMembership(ctx, recStep.ProviderRef, row.CollectionProviderID); err != nil {
			return false, err
		}
		if s.tripAfterProviderWrite(stepUpload) {
			return false, ErrHaltSimulated
		}
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepUpload, "done", attID, nil); err != nil {
		return false, err
	}
	if err := s.store.AppendEvent(ctx, row.ImportID, "provider_write", map[string]any{
		"step": stepUpload, "provider_ref": attID, "rendition_added": det.Plan.AddRendition,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// runVerify — verify the provider state (full catalog) and publish the
// source revision; then commit.
func (s *Service) runVerify(ctx context.Context, row ImportRow) (bool, error) {
	step, err := s.store.GetStep(ctx, row.ImportID, stepVerify)
	if err == nil && step.State == "done" {
		return true, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	recStep, rerr := s.store.GetStep(ctx, row.ImportID, stepCreateRec)
	if rerr != nil || recStep.ProviderRef == "" {
		return false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInternal, "creating_record step not done before verify")
	}
	upStep, uerr := s.store.GetStep(ctx, row.ImportID, stepUpload)
	if uerr != nil || upStep.ProviderRef == "" {
		return false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInternal, "uploading_rendition step not done before verify")
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepVerify, "in_progress", "", nil); err != nil {
		return false, err
	}

	// Verify against the catalog (the truth): record carries the
	// rendition; membership present when targeted.
	rec, err := s.catalogRecord(ctx, recStep.ProviderRef)
	if err != nil {
		return false, err
	}
	if rec == nil || findRendition(*rec, row.StagingSHA256) == "" {
		return false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"verify failed: rendition not observable in the provider catalog after upload")
	}
	det, err := s.loadResolveDetail(ctx, row.ImportID)
	if err != nil {
		return false, err
	}
	if row.CollectionProviderID != "" && !membershipObserved(rec, row.CollectionProviderID) {
		return false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"verify failed: collection membership not observable in the provider catalog")
	}

	// Publish the source revision (the Library→Store bridge artifact).
	// PublishRevision allocates AND persists atomically — the returned id
	// always resolves to a row (the import stamp can never dangle).
	bib := revision.Bibliography{
		RecordID:      recStep.ProviderRef,
		Title:         det.Merged.Title,
		Authors:       authorStrings(det.Merged),
		Year:          det.Merged.Year,
		Publisher:     det.Merged.Publisher,
		Language:      det.Merged.Language,
		CitationClass: revision.CitationClassCitable,
	}
	revID, _, err := s.store.PublishRevision(ctx, SourceRevisionDomain{
		SourceID:     s.cfg.SourceID,
		RecordID:     recStep.ProviderRef,
		RenditionID:  upStep.ProviderRef,
		ContentHash:  row.StagingSHA256,
		MediaType:    row.MediaType,
		Bibliography: bib,
		LocatorCapabilities: revision.LocatorCapabilities{
			Page: &revision.PageCapability{Trust: revision.TrustPhysicalOnly},
		},
		ContentTicket: "lst:" + row.StagingSHA256,
		Origin:        "import",
		CreatedAt:     time.Now(),
	})
	if err != nil {
		return false, err
	}
	if s.tripAfterProviderWrite(stepVerify) {
		return false, ErrHaltSimulated
	}
	if err := s.store.UpsertStep(ctx, row.ImportID, stepVerify, "done", fmt.Sprintf("%d", revID), nil); err != nil {
		return false, err
	}
	if err := s.store.UpdateImportStatus(ctx, row.ImportID, row.Status, "", nil,
		recStep.ProviderRef, upStep.ProviderRef, row.CollectionProviderID,
		recStep.ProviderRef, upStep.ProviderRef, revID); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// helpers

func (s *Service) loadDocFields(ctx context.Context, importID string) (ResolvedFields, error) {
	step, err := s.store.GetStep(ctx, importID, stepInspect)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(step.Detail) == 0) {
		return ResolvedFields{}, nil
	}
	if err != nil {
		return ResolvedFields{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "inspect step read")
	}
	var f ResolvedFields
	if err := json.Unmarshal(step.Detail, &f); err != nil {
		return ResolvedFields{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "inspect step decode")
	}
	return f, nil
}

func (s *Service) persistProvenance(ctx context.Context, importID string, rows []ProvenanceRow) error {
	for _, p := range rows {
		if err := s.store.AppendProvenance(ctx, importID, p); err != nil {
			return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "provenance append")
		}
	}
	return nil
}

func (s *Service) offerDecision(ctx context.Context, importID string, d *Decision) error {
	cands := make([]map[string]any, len(d.Candidates))
	for i, c := range d.Candidates {
		cands[i] = map[string]any{"candidate_id": c.CandidateID, "origin": c.Origin, "summary": c.Summary}
	}
	if err := s.store.AppendEvent(ctx, importID, "decision_offered", map[string]any{
		"decision_id": d.DecisionID, "subject": d.Subject, "candidates": cands,
	}); err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "decision event")
	}
	if err := s.store.UpdateImportStatus(ctx, importID, library.ImportAwaitingConfirm, "", nil, "", "", "", "", "", 0); err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "state transition")
	}
	if s.tripAfterState(string(library.ImportAwaitingConfirm)) {
		return ErrHaltSimulated
	}
	return nil
}

// openDecision reconstructs the open decision from the event log.
func (s *Service) openDecision(ctx context.Context, row ImportRow) (*Decision, error) {
	events, err := s.store.ListEvents(ctx, row.ImportID)
	if err != nil {
		return nil, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "event read")
	}
	var open *Decision
	for _, e := range events {
		switch e.Kind {
		case "decision_offered":
			var d struct {
				DecisionID string `json:"decision_id"`
				Subject    string `json:"subject"`
				Candidates []struct {
					CandidateID string `json:"candidate_id"`
					Origin      string `json:"origin"`
					Summary     string `json:"summary"`
				} `json:"candidates"`
			}
			if json.Unmarshal(e.Detail, &d) != nil {
				continue
			}
			dec := &Decision{DecisionID: d.DecisionID, Subject: d.Subject}
			for _, c := range d.Candidates {
				dec.Candidates = append(dec.Candidates, DecisionCandidate{CandidateID: c.CandidateID, Origin: c.Origin, Summary: c.Summary})
			}
			open = dec
		case "decision_resolved":
			open = nil
		}
	}
	return open, nil
}

// operation assembles the F03 ImportOperation DTO.
func (s *Service) operation(ctx context.Context, row ImportRow) (library.ImportOperation, error) {
	op := library.ImportOperation{
		ImportID:  row.ImportID,
		Status:    row.Status,
		UpdatedAt: now(row.UpdatedAt),
	}
	if row.Status == library.ImportCommitted {
		rev, err := s.store.LatestRevision(ctx, s.cfg.SourceID, row.RecordID, row.RenditionID)
		if err != nil {
			return op, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "revision load")
		}
		op.Result = &library.ImportResult{
			RecordID:    row.RecordID,
			RenditionID: row.RenditionID,
			Revision:    RevisionFromDomain(rev),
		}
	}
	if row.Status.Terminal() && row.Status != library.ImportCommitted {
		op.Failure = &library.ImportFailure{Code: row.FailureCode, Message: row.FailureMessage}
	}
	if row.Status == library.ImportAwaitingConfirm {
		dec, err := s.openDecision(ctx, row)
		if err != nil {
			return op, err
		}
		if dec != nil {
			dto := library.ImportDecision{DecisionID: dec.DecisionID, Subject: dec.Subject}
			for _, c := range dec.Candidates {
				dto.Candidates = append(dto.Candidates, library.ImportCandidate{
					CandidateID: c.CandidateID, Origin: c.Origin, Summary: c.Summary,
				})
			}
			op.Decisions = append(op.Decisions, dto)
		}
	}
	// Field provenance (GET status carries it per field).
	prov, err := s.store.ListProvenance(ctx, row.ImportID)
	if err != nil {
		return op, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "provenance load")
	}
	for _, p := range prov {
		op.Fields = append(op.Fields, library.FieldProvenance{
			Field: p.Field, Source: p.Source, ResolverVersion: p.ResolverVersion,
			Confidence: p.Confidence, Applied: p.Applied,
		})
	}
	return op, nil
}

// catalogRecord fetches one catalog record by provider id (full
// pagination until found — the catalog is the truth).
func (s *Service) catalogRecord(ctx context.Context, providerID string) (*CatalogRecord, error) {
	if s.ports.Catalog == nil {
		return nil, nil
	}
	token := ""
	for {
		page, err := s.ports.Catalog.ListRecords(ctx, token)
		if err != nil {
			return nil, err
		}
		for i := range page.Records {
			if page.Records[i].ProviderRecordID == providerID {
				cp := page.Records[i]
				return &cp, nil
			}
		}
		if page.NextPageToken == "" {
			return nil, nil
		}
		token = page.NextPageToken
	}
}

func membershipObserved(rec *CatalogRecord, collectionProviderID string) bool {
	if rec == nil {
		return false
	}
	for _, c := range rec.Collections {
		if c == collectionProviderID {
			return true
		}
	}
	return false
}

// catalogIncoming builds the dedup-matrix view of the incoming record.
// Hint identifiers steer matching too (caller-known identifiers are
// legitimate match criteria — they still never enter the field set).
func catalogIncoming(recordType string, f ResolvedFields, hintDOI, hintISBN string) CatalogRecord {
	return CatalogRecord{
		RecordType: recordType,
		Title:      f.Title,
		Authors:    authorsFromMerged(f),
		Year:       f.Year,
		DOI:        NormalizeDOI(firstNonEmpty(f.DOI, hintDOI)),
		ISBN:       NormalizeISBN(firstNonEmpty(f.ISBN, hintISBN)),
	}
}

func catalogRecordFrom(det resolveDetail) CatalogRecord {
	return catalogIncoming(det.RecordType, det.Merged, det.HintDOI, det.HintISBN)
}

func authorsFromMerged(f ResolvedFields) []Creator {
	var out []Creator
	for _, a := range f.Authors {
		// Resolvers deliver display names ("Ada Example"); the schema
		// convention wants the lastName component (the #287 cascade).
		last := a
		if sp := strings.LastIndex(a, " "); sp >= 0 && sp < len(a)-1 {
			last = a[sp+1:]
		}
		out = append(out, Creator{LastName: last, CreatorType: "author"})
	}
	return out
}

func authorStrings(f ResolvedFields) []string { return f.Authors }

func yearValue(y *int) int {
	if y == nil {
		return 0
	}
	return *y
}

// isBadUUID reports a 22P02 against the uuid import_id: an id that
// cannot even parse is an unknown import, not an internal error.
func isBadUUID(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "22P02"
}

// isMissingRelation reports a 42P01 (relation absent): the Library
// schema runs standalone (fake providers, no Zotero mirror) — the mirror
// fallbacks treat that as plain absence, never as an internal error.
func isMissingRelation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "42P01"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
