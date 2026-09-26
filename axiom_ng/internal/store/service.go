// service.go — the Store component behind the F03 contract (F09 #303).
// IngestRevision is the single intake: validate → canonical idempotency →
// durable mint; the dispatcher's revision claim lane does the rest. Search
// and GetPassage wrap the existing retrieval stack and map its types onto
// the contract DTOs (field-name-frozen mirrors of the same public shapes).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
	"github.com/jackc/pgx/v5/pgtype"
)

// SearchBackend is the retrieval surface the Store wraps (implemented by
// *search.Service).
type SearchBackend interface {
	Search(ctx context.Context, req search.Request) (*search.Response, error)
	GetPassage(ctx context.Context, chunkID string) (*search.Passage, error)
}

// Service is the store.Store implementation over the durable repo and the
// retrieval stack.
type Service struct {
	rep    *repo.Repo
	search SearchBackend
	log    *log.Logger
}

// New builds the Store service.
func New(rep *repo.Repo, sb SearchBackend, lg *log.Logger) *Service {
	if lg == nil {
		lg = log.Default()
	}
	return &Service{rep: rep, search: sb, log: lg}
}

var _ store.Store = (*Service)(nil)

// IngestRevision starts durable intake of one source revision. Precedence
// per the contract: revision validation, then idempotency, then the mint
// (content verification happens in the claim/compute lane — the snapshot
// hash chain is the proof).
func (s *Service) IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error) {
	if req.IdempotencyKey == "" {
		return store.IngestJob{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "intake idempotency key is empty")
	}
	if err := req.Revision.Validate(); err != nil {
		return store.IngestJob{}, err // already a typed contract error (ClassInvalidArgument)
	}
	// SourceID is opaque to the CONTRACT but the durable lane resolves it
	// as a mirror uuid: validate the shape at the trust boundary and
	// normalize to the canonical lowercase form — a malformed id would
	// otherwise poison the claim queue (uuid cast error at claim), and a
	// non-canonical case would silently miss the #294 suppression and take
	// a different advisory-lock key than the sync's canonical one.
	if err := normalizeSourceID(&req.Revision); err != nil {
		return store.IngestJob{}, err
	}
	canonical, err := canonicalRevisionJSON(req.Revision)
	if err != nil {
		return store.IngestJob{}, contracterr.Wrap(contracterr.ComponentStore, contracterr.ClassInternal, err, "canonicalize revision")
	}
	job, minted, err := s.rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey:      req.IdempotencyKey,
		RevisionSourceID:    req.Revision.SourceID,
		RevisionRecordID:    req.Revision.Bibliography.RecordID,
		RevisionRenditionID: req.Revision.RenditionID,
		RevisionNo:          req.Revision.RevisionID,
		ContentHash:         req.Revision.ContentHash,
		RevisionJSON:        canonical,
	})
	switch {
	case err == nil:
		if job == nil {
			return store.IngestJob{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInternal, "intake mint returned no job")
		}
		if minted {
			s.log.Printf("store: revision intake minted job %s (rendition %s revision %s)", job.ID, req.Revision.RenditionID, req.Revision.RevisionID)
		}
		return ingestJobDTO(job, req.Revision), nil
	case errors.Is(err, repo.ErrIntakeKeyMismatch):
		return store.IngestJob{}, &contracterr.IdempotencyMismatch{Key: req.IdempotencyKey}
	case errors.Is(err, repo.ErrIntakeSuppressed):
		// #294: the content is already processed and served. The honest
		// contract answer is a committed-looking job echo — the corpus
		// state IS the terminal observable; v1 has no job row to point
		// at (the suppression deleted nothing, it minted nothing).
		return store.IngestJob{
			Status:      store.IngestCommitted,
			RevisionID:  req.Revision.RevisionID,
			ContentHash: req.Revision.ContentHash,
			Attempt:     1, MaxAttempts: 1,
			UpdatedAt: time.Now().UTC(),
		}, nil
	default:
		return store.IngestJob{}, contracterr.Wrap(contracterr.ComponentStore, contracterr.ClassInternal, err, "intake mint")
	}
}

// normalizeSourceID validates the revision's SourceID as a UUID and
// rewrites it in canonical lowercase form (the mirror's uuid::text shape;
// pgtype renders exactly that). No new dependency: pgx ships the parser.
func normalizeSourceID(r *revision.SourceRevision) error {
	var u pgtype.UUID
	if err := u.Scan(r.SourceID); err != nil {
		return contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument,
			"revision source_id is not a valid uuid: "+r.SourceID)
	}
	r.SourceID = u.String()
	return nil
}

// canonicalRevisionJSON renders the DTO deterministically (Go struct field
// order is fixed; the marshal IS the canonical form both idempotency sides
// compare).
func canonicalRevisionJSON(r revision.SourceRevision) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ingestJobDTO maps a durable job row onto the contract DTO. Legacy SQL
// states map onto the intake machine; revision identity echoes the
// ingested revision.
func ingestJobDTO(j *repo.Job, rev revision.SourceRevision) store.IngestJob {
	dto := store.IngestJob{
		JobID:       j.ID,
		Status:      mapJobStatus(j.Status),
		RevisionID:  rev.RevisionID,
		ContentHash: derefStr(j.ContentHash),
		Attempt:     j.Attempt,
		MaxAttempts: j.MaxAttempts,
		UpdatedAt:   time.Now().UTC(),
	}
	if j.ErrorCode != nil {
		dto.Failure = &store.IngestFailure{Code: *j.ErrorCode}
		if j.ErrorMessage != nil {
			dto.Failure.Message = *j.ErrorMessage
		}
	}
	return dto
}

func mapJobStatus(sql string) store.IngestStatus {
	switch sql {
	case "pending":
		return store.IngestReceived
	case "claimed", "processing":
		return store.IngestProcessing
	case "completed":
		return store.IngestCommitted
	case "skipped", "obsolete", "cancelled":
		return store.IngestTerminalFailed
	default: // failed
		return store.IngestRetryableFailed
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Search runs hybrid retrieval through the wrapped stack. The contract's
// argument rules are enforced HERE (the frozen v0.1.18 behavior), internal
// errors are wrapped as contract errors.
func (s *Service) Search(ctx context.Context, req store.SearchRequest) (store.SearchResult, error) {
	if req.Query == "" {
		return store.SearchResult{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "search: query is blank")
	}
	if req.TopN > store.MaxTopN {
		return store.SearchResult{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, fmt.Sprintf("search: top_n %d above the overfetch cap %d", req.TopN, store.MaxTopN))
	}
	sreq := search.Request{Query: req.Query, TopN: req.TopN}
	if req.Filters != nil {
		sreq.Filters = &search.Filters{DocumentIDs: req.Filters.DocumentIDs}
	}
	res, err := s.search.Search(ctx, sreq)
	if err != nil {
		return store.SearchResult{}, contracterr.Wrap(contracterr.ComponentStore, contracterr.ClassInternal, err, "search")
	}
	return searchResponseDTO(res), nil
}

// GetPassage resolves one chunk with neighbors; unknown ids are the
// contract's NotFound.
func (s *Service) GetPassage(ctx context.Context, ref store.PassageRef) (store.Passage, error) {
	if ref.ChunkID == "" {
		return store.Passage{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "passage: chunk id is blank")
	}
	p, err := s.search.GetPassage(ctx, ref.ChunkID)
	if err != nil {
		if errors.Is(err, search.ErrPassageNotFound) {
			return store.Passage{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassNotFound, "passage: "+ref.ChunkID)
		}
		return store.Passage{}, contracterr.Wrap(contracterr.ComponentStore, contracterr.ClassInternal, err, "passage")
	}
	return passageDTO(p), nil
}
