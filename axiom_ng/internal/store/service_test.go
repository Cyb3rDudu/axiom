// service_test.go — the Store service contract semantics at the seam:
// validation precedence over idempotency, the mismatch class, the
// suppression echo. DB-backed (AXIOM_TEST_DATABASE_URL, scratch).
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

func TestIngestRevisionValidationPrecedence(t *testing.T) {
	d := openIntakeDB(t)
	svc := New(repo.New(d.Pool()), nil, nil)
	ctx := context.Background()

	// Blank key: InvalidArgument before anything else.
	_, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "", Revision: seedRevision("x", "y")})
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInvalidArgument {
		t.Fatalf("blank key must be InvalidArgument, got %v", err)
	}
	// Invalid revision: InvalidArgument, even on a REUSED key (validation
	// precedes idempotency — the contract's precedence rule). The key is
	// one a VALID intake minted first: a lookup-first implementation
	// staying green here would mean it ignored the stored row's class.
	hash := revision.HashContent([]byte("precedence bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	if _, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "svc-reused", Revision: seedRevision(srcID, hash)}); err != nil {
		t.Fatalf("valid mint: %v", err)
	}
	_, err = svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "svc-reused", Revision: revision.SourceRevision{}})
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInvalidArgument {
		t.Fatalf("invalid revision on a REUSED key must be InvalidArgument, got %v", err)
	}
}

func TestIngestRevisionMismatchIsTypedConflict(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("service mismatch bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	svc := New(repo.New(d.Pool()), nil, nil)
	ctx := context.Background()

	if _, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "svc-key", Revision: seedRevision(srcID, hash)}); err != nil {
		t.Fatalf("first intake: %v", err)
	}
	diverged := seedRevision(srcID, hash)
	diverged.RevisionID = "2"
	_, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "svc-key", Revision: diverged})
	if err == nil {
		t.Fatal("diverged replay must fail")
	}
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassConflict {
		t.Fatalf("mismatch must be the Conflict class, got %v", err)
	}
	var mm *contracterr.IdempotencyMismatch
	if !asMismatch(err, &mm) || mm.Key != "svc-key" {
		t.Fatalf("mismatch must carry the key, got %v", err)
	}
	// Replay returns the SAME job id.
	j1, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "svc-key", Revision: seedRevision(srcID, hash)})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	j2, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "svc-key", Revision: seedRevision(srcID, hash)})
	if err != nil {
		t.Fatalf("replay 2: %v", err)
	}
	if j1.JobID != j2.JobID || j1.RevisionID != "1" || j1.ContentHash != hash || j1.Status != store.IngestReceived {
		t.Fatalf("replay must echo the same job: %+v vs %+v", j1, j2)
	}
	// DTO shape: UpdatedAt RFC3339 µs discipline.
	if j1.UpdatedAt.IsZero() {
		t.Fatal("job UpdatedAt must be set")
	}
	b, _ := json.Marshal(j1)
	t.Logf("job DTO: %s", b)
}

func TestIngestRevisionSuppressedEchoesCommitted(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("suppressed bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()
	// An ACTIVE snapshot for the same content: the #294 suppression fires.
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		SELECT a.id, $1, 'p', 'v', 'ph', a.document_id, '{}', true
		FROM zotero_attachments a WHERE a.source_id::text=$2 AND a.zotero_key='ATTIT1'`,
		hash, srcID); err != nil {
		t.Fatal(err)
	}
	svc := New(rep, nil, nil)
	job, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "supp-key", Revision: seedRevision(srcID, hash)})
	if err != nil {
		t.Fatalf("suppressed intake must not error: %v", err)
	}
	if job.Status != store.IngestCommitted {
		t.Fatalf("suppressed intake must echo committed, got %q", job.Status)
	}
	// And it must not have minted a pending job.
	var pending int
	if err := d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM ingest_jobs WHERE intake_kind='revision' AND status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("suppression must leave no pending job, got %d", pending)
	}
}

func asMismatch(err error, out **contracterr.IdempotencyMismatch) bool {
	for err != nil {
		if mm, ok := err.(*contracterr.IdempotencyMismatch); ok {
			*out = mm
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestIngestRevisionSourceIDTrustBoundary — normalizeSourceID's two
// branches (review R2-2): malformed uuid is InvalidArgument at the door;
// a non-canonical-case uuid normalizes to the SAME durable job (the #294
// suppression and LockKey see one canonical form, never two).
func TestIngestRevisionSourceIDTrustBoundary(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("uuid boundary bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	svc := New(repo.New(d.Pool()), nil, nil)
	ctx := context.Background()

	// Malformed: rejected at the trust boundary, never minted.
	rev := seedRevision("not-a-uuid", hash)
	rev.RenditionID = "ATTIT1"
	if _, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "uuid-bad", Revision: rev}); err == nil {
		t.Fatal("malformed source_id must be rejected")
	} else if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInvalidArgument {
		t.Fatalf("malformed source_id must be InvalidArgument, got %v", err)
	}
	var minted int
	if err := d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM ingest_jobs WHERE intake_kind='revision'`).Scan(&minted); err != nil {
		t.Fatal(err)
	}
	if minted != 0 {
		t.Fatalf("rejected intake must mint nothing, got %d rows", minted)
	}

	// Non-canonical case: upper-casing the uuid must land on the SAME job.
	first, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "uuid-case", Revision: seedRevision(srcID, hash)})
	if err != nil {
		t.Fatalf("canonical intake: %v", err)
	}
	// Same revision, only the SourceID SPELLING upper-cased (the ticket
	// and every other field stay byte-identical — the realistic
	// case-variant replay).
	upper := seedRevision(srcID, hash)
	upper.SourceID = strings.ToUpper(srcID)
	second, err := svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "uuid-case", Revision: upper})
	if err != nil {
		t.Fatalf("case-variant replay must resolve through normalization: %v", err)
	}
	if second.JobID != first.JobID {
		t.Fatalf("case-variant replay must be the SAME job: %s vs %s", second.JobID, first.JobID)
	}
}

// TestIngestRevisionEmptyRenditionIDRejected — the new Validate arm has
// its own probe (review R2-7).
func TestIngestRevisionEmptyRenditionIDRejected(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("rendition empty bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	svc := New(repo.New(d.Pool()), nil, nil)
	rev := seedRevision(srcID, hash)
	rev.RenditionID = ""
	_, err := svc.IngestRevision(context.Background(), store.IngestRevisionRequest{IdempotencyKey: "rend-empty", Revision: rev})
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInvalidArgument {
		t.Fatalf("empty rendition_id must be InvalidArgument, got %v", err)
	}
}

// failSearchBackend records the error Search should classify (satisfies
// SearchBackend with the real search types).
type failSearchBackend struct {
	searchErr  error
	passageErr error
	lastQuery  string
}

func (f *failSearchBackend) Search(ctx context.Context, req search.Request) (*search.Response, error) {
	f.lastQuery = req.Query
	return nil, f.searchErr
}
func (f *failSearchBackend) GetPassage(ctx context.Context, chunkID string) (*search.Passage, error) {
	return nil, f.passageErr
}

// TestSearchClassifiesContractErrors — the M2/M3 witnesses at the seam:
// blank-after-trim queries are InvalidArgument (the wrapped layer's trim
// rule, mapped HERE — not internal); an inactive-snapshot passage is
// NotFound (the frozen route's 404 class), not internal.
func TestSearchClassifiesContractErrors(t *testing.T) {
	svc := New(nil, &failSearchBackend{}, nil)
	ctx := context.Background()

	_, err := svc.Search(ctx, store.SearchRequest{Query: "   "})
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInvalidArgument {
		t.Fatalf("blank-after-trim query must be InvalidArgument, got %v", err)
	}

	fb := &failSearchBackend{passageErr: &search.InactiveSnapshotError{ChunkID: "c1"}}
	svc2 := New(nil, fb, nil)
	_, err = svc2.GetPassage(ctx, store.PassageRef{ChunkID: "c1"})
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassNotFound {
		t.Fatalf("inactive-snapshot passage must be NotFound, got %v", err)
	}
}

// TestIngestJobDTOPresentSlicesAndFailure — the M4/M5 witnesses at the
// DTO layer: neighbors/section serialize PRESENT (never null), and a
// failed job carries its Failure (retryable below budget, terminal at
// budget).
func TestIngestJobDTOPresentSlicesAndFailure(t *testing.T) {
	code, msg := "LEASE_EXHAUSTED", "lease gone"
	j := &repo.Job{ID: "j1", Status: "failed", Attempt: 3, MaxAttempts: 3,
		ErrorCode: &code, ErrorMessage: &msg, RevisionNo: "7",
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)}
	dto := ingestJobDTO(j, revision.SourceRevision{RevisionID: "1"})
	if dto.Status != store.IngestTerminalFailed || dto.Failure == nil || dto.Failure.Code != code || dto.Failure.Message != msg {
		t.Fatalf("terminal failed must carry Failure: %+v", dto)
	}
	if dto.RevisionID != "7" {
		t.Fatalf("revision echo must be the ROW's revision, got %q", dto.RevisionID)
	}
	j.Attempt = 1
	dto = ingestJobDTO(j, revision.SourceRevision{RevisionID: "1"})
	if dto.Status != store.IngestRetryableFailed {
		t.Fatalf("failed below budget must be retryable, got %q", dto.Status)
	}
	b, err := json.Marshal(passageDTO(&search.Passage{ChunkID: "c", Section: nil}))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"neighbors":null`)) || bytes.Contains(b, []byte(`"section":null`)) {
		t.Fatalf("frozen optionalität: slices must be present-empty, got %s", b)
	}
}

// TestIngestReplayIdentityDeepEqual — the M6 witness: two service-level
// replays of the same intake are reflect.DeepEqual (UpdatedAt comes from
// the row, not the clock).
func TestIngestReplayIdentityDeepEqual(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("deepequal bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	svc := New(repo.New(d.Pool()), nil, nil)
	ctx := context.Background()
	req := store.IngestRevisionRequest{IdempotencyKey: "deq", Revision: seedRevision(srcID, hash)}
	first, err := svc.IngestRevision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.IngestRevision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay identity broken:\n%+v\n%+v", first, second)
	}
	if first.UpdatedAt.Nanosecond()%1000 != 0 {
		t.Fatalf("UpdatedAt must be µs-aligned (DM03), got %v", first.UpdatedAt)
	}
}
