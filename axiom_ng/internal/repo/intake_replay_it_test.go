// intake_replay_it_test.go — the 23505 re-ask path's witness (review
// R2-3): resolveIntakeKey answers the intake-key question for BOTH the
// fast path and the concurrent-mint race; this pins the shared SQL's
// classification directly (deterministic — no race to orchestrate).
// Gated: AXIOM_TEST_DATABASE_URL.
package repo

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	storemigrations "github.com/Cyb3rDudu/axiom/axiom_ng/internal/store/migrations"
)

func openReplayDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping replay IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %q", dsn)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := storemigrations.Migrate(ctx, d.Pool()); err != nil {
		t.Fatalf("store migrate: %v", err)
	}
	if _, err := d.Pool().Exec(ctx, `TRUNCATE ingest_jobs CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return d
}

// TestResolveIntakeKeyClassifiesReplayAndMismatch — pre-insert a revision
// job under key K, then resolve: identical canonical JSON → the same job;
// diverged JSON → ErrIntakeKeyMismatch; unknown key → pgx.ErrNoRows.
func TestResolveIntakeKeyClassifiesReplayAndMismatch(t *testing.T) {
	d := openReplayDB(t)
	ctx := context.Background()
	hash := revision.HashContent([]byte("replay resolve bytes"))
	rev := revision.SourceRevision{
		SourceID: "11111111-1111-1111-1111-111111111111", RevisionID: "1", RenditionID: "R1",
		ContentHash: hash, MediaType: revision.MediaTypePDF,
		Bibliography:  revision.Bibliography{RecordID: "D1", CitationClass: "citable"},
		ContentTicket: "zat:x:R1",
	}
	revJSON, _ := json.Marshal(rev)
	var jobID string
	if err := d.Pool().QueryRow(ctx, `
		INSERT INTO ingest_jobs (intake_kind, intake_idempotency_key, content_hash, status,
		                         revision_source_id, revision_record_id, revision_rendition_id,
		                         revision_no, revision_json, max_attempts)
		VALUES ('revision','replay-key',$1,'pending',$2,'D1','R1','1',$3::jsonb,3) RETURNING id::text`,
		hash, rev.SourceID, string(revJSON)).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	rep := New(d.Pool())

	req := IntakeRequest{IdempotencyKey: "replay-key", RevisionJSON: revJSON}
	got, err := rep.resolveIntakeKey(ctx, req)
	if err != nil || got == nil || got.ID != jobID {
		t.Fatalf("identical revision must resolve to the same job: %+v %v", got, err)
	}

	diverged := rev
	diverged.RevisionID = "2"
	divJSON, _ := json.Marshal(diverged)
	req.RevisionJSON = divJSON
	if _, err := rep.resolveIntakeKey(ctx, req); err != ErrIntakeKeyMismatch {
		t.Fatalf("diverged revision must be a key mismatch, got %v", err)
	}

	req.IdempotencyKey = "unknown-key"
	req.RevisionJSON = revJSON
	if _, err := rep.resolveIntakeKey(ctx, req); !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("unknown key must surface pgx.ErrNoRows, got %v", err)
	}
}
