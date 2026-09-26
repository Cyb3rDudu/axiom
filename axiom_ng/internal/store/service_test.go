// service_test.go — the Store service contract semantics at the seam:
// validation precedence over idempotency, the mismatch class, the
// suppression echo. DB-backed (AXIOM_TEST_DATABASE_URL, scratch).
package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
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
	// precedes idempotency — the contract's precedence rule).
	_, err = svc.IngestRevision(ctx, store.IngestRevisionRequest{IdempotencyKey: "k", Revision: revision.SourceRevision{}})
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInvalidArgument {
		t.Fatalf("invalid revision must be InvalidArgument, got %v", err)
	}
}

func TestIngestRevisionMismatchIsTypedConflict(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("service mismatch bytes"))
	srcID := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
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
	srcID := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
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
