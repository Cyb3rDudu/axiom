// intake_it_test.go — the F09 #303 revision-intake lane, proven at the
// durable layer: mint (idempotency / mismatch / dedup / #294 suppression)
// → claim (mirror FK resolution, revision-typed freeze, lease fence) →
// persist (snapshot + chunks + OS outbox in ONE transaction — the #264
// atomicity carried over unchanged). Gated: AXIOM_TEST_DATABASE_URL.
package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	storemigrations "github.com/Cyb3rDudu/axiom/axiom_ng/internal/store/migrations"
)

func openIntakeDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping intake IT")
	}
	if !strings.HasSuffix(strings.Split(dsn, "/")[len(strings.Split(dsn, "/"))-1], "_test") {
		t.Fatalf("refusing to run against non-_test database")
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
	if _, err := d.Pool().Exec(ctx, `TRUNCATE ingest_jobs, zotero_attachments, zotero_documents,
		zotero_items, zotero_sources, processing_snapshots CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return d
}

// seedMirror seeds the mirror rows a ledger revision resolves to.
func seedMirror(t *testing.T, d *db.DB, docKey, attKey, contentHash string) (srcID, docID, attID string) {
	t.Helper()
	ctx := context.Background()
	var itemID string
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_sources (base_url, library_id, server_id) VALUES ('https://intake-it.local','users/0','srv-1') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatalf("source: %v", err)
	}
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title, creators)
		 VALUES ($1,$2,1,'book','Intake IT Book', '[{"firstName":"Ada","lastName":"Example","creatorType":"author"}]'::jsonb) RETURNING id::text`,
		srcID, docKey).Scan(&docID); err != nil {
		t.Fatalf("document: %v", err)
	}
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		 VALUES ($1,$2,1,'book',NULL,$3,$4) RETURNING id::text`,
		srcID, docKey, `{"key":"`+docKey+`"}`, `{"key":"`+docKey+`","version":1,"itemType":"book","title":"Intake IT Book"}`).Scan(&itemID); err != nil {
		t.Fatalf("item: %v", err)
	}
	if _, err := d.Pool().Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docID, itemID); err != nil {
		t.Fatalf("link item: %v", err)
	}
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
		 VALUES ($1,$2,$3,1,$4,'imported_file','application/pdf','it.pdf','/tmp/it.pdf',$5,true,false) RETURNING id::text`,
		srcID, docID, attKey, docKey, contentHash).Scan(&attID); err != nil {
		t.Fatalf("attachment: %v", err)
	}
	return srcID, docID, attID
}

func seedRevision(srcID, hash string) revision.SourceRevision {
	year := 2026
	return revision.SourceRevision{
		SourceID:    srcID,
		RevisionID:  "1",
		RenditionID: "ATTIT1",
		ContentHash: hash,
		MediaType:   revision.MediaTypePDF,
		Bibliography: revision.Bibliography{
			RecordID: "DOCIT1", Title: "Intake IT Book", Authors: []string{"Ada Example"},
			Year: &year, CitationClass: "citable",
		},
		ContentTicket: "zat:" + srcID + ":ATTIT1",
	}
}

func mustCanonical(t *testing.T, r revision.SourceRevision) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRevisionIntakeMintIdempotency(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("intake it bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()

	req := repo.IntakeRequest{
		IdempotencyKey:      "intake-key-1",
		RevisionSourceID:    srcID,
		RevisionRecordID:    "DOCIT1",
		RevisionRenditionID: "ATTIT1",
		RevisionNo:          "1",
		ContentHash:         hash,
		RevisionJSON:        mustCanonical(t, seedRevision(srcID, hash)),
	}
	job, minted, err := rep.EnqueueRevisionIntake(ctx, req)
	if err != nil || job == nil || !minted {
		t.Fatalf("first mint: job=%v minted=%v err=%v", job, minted, err)
	}
	if job.Status != "pending" {
		t.Fatalf("minted status %q, want pending", job.Status)
	}

	// Replay: same key + identical revision → SAME job.
	again, minted2, err := rep.EnqueueRevisionIntake(ctx, req)
	if err != nil || minted2 || again == nil || again.ID != job.ID {
		t.Fatalf("replay must return the same job: %+v minted=%v err=%v", again, minted2, err)
	}

	// Mismatch: same key + different revision.
	diverged := req
	diverged.RevisionJSON = mustCanonical(t, func() revision.SourceRevision {
		r := seedRevision(srcID, hash)
		r.RevisionID = "2"
		return r
	}())
	_, _, err = rep.EnqueueRevisionIntake(ctx, diverged)
	if err != repo.ErrIntakeKeyMismatch {
		t.Fatalf("diverged replay must be a key mismatch, got %v", err)
	}

	// Identity dedup: different key + identical revision → same durable job.
	dedup := req
	dedup.IdempotencyKey = "intake-key-2"
	same, minted3, err := rep.EnqueueRevisionIntake(ctx, dedup)
	if err != nil || minted3 || same == nil || same.ID != job.ID {
		t.Fatalf("identity dedup must join the existing job: %+v minted=%v err=%v", same, minted3, err)
	}
}

func TestRevisionIntakeClaimFreezesAndResolvesFKs(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("intake claim bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()

	job, minted, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey:      "claim-key-1",
		RevisionSourceID:    srcID,
		RevisionRecordID:    "DOCIT1",
		RevisionRenditionID: "ATTIT1",
		RevisionNo:          "1",
		ContentHash:         hash,
		RevisionJSON:        mustCanonical(t, seedRevision(srcID, hash)),
	})
	if err != nil || !minted {
		t.Fatalf("mint: %v %v", job, err)
	}

	claimed, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{
		WorkerID: "intake-it", LeaseDuration: 30 * time.Second,
		Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if claimed.JobID != job.ID {
		t.Fatalf("claimed job %s, want the minted %s", claimed.JobID, job.ID)
	}
	if claimed.AttachmentID == "" || claimed.DocumentID == "" {
		t.Fatalf("claim must resolve the mirror FKs, got doc=%q att=%q", claimed.DocumentID, claimed.AttachmentID)
	}
	var frozen repo.FrozenInput
	if err := json.Unmarshal(claimed.InputSnapshot, &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.Intake != "revision" || frozen.Revision == nil {
		t.Fatalf("frozen input must carry the revision lane marker + block, got %+v", frozen.Intake)
	}
	if frozen.Revision.RenditionID != "ATTIT1" || frozen.Revision.Bibliography.RecordID != "DOCIT1" {
		t.Fatalf("frozen revision identity: %+v", frozen.Revision)
	}
	if frozen.Attachment.AttachmentID != claimed.AttachmentID || derefP(frozen.Attachment.ContentHash) != hash {
		t.Fatalf("frozen attachment must be the resolved mirror row: %+v", frozen.Attachment)
	}
	var bib struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(frozen.Document.MetadataSnapshot, &bib); err != nil || bib.Title != "Intake IT Book" {
		t.Fatalf("metadata snapshot must be the revision bibliography, got %s (%v)", bib.Title, err)
	}
}

func TestRevisionIntakeClaimObsoletesOnUnresolvableRef(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("unresolvable bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	// A revision whose rendition does NOT exist in the mirror.
	rev := seedRevision(srcID, hash)
	rev.RenditionID = "GONE1"
	rep := repo.New(d.Pool())
	ctx := context.Background()
	job, minted, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "ghost-key-1", RevisionSourceID: srcID, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "GONE1", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, rev),
	})
	if err != nil || !minted {
		t.Fatalf("mint: %v %v", job, err)
	}
	if _, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{
		WorkerID: "intake-it", LeaseDuration: 30 * time.Second,
		Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
	}); err != nil {
		t.Fatalf("claim (must obsolete the ghost, not error): %v", err)
	}
	var status, skipped string
	if err := d.Pool().QueryRow(ctx,
		`SELECT status::text, COALESCE(error_message,'') FROM ingest_jobs WHERE id=$1`, job.ID).Scan(&status, &skipped); err != nil {
		t.Fatal(err)
	}
	if status != "skipped" || !strings.Contains(skipped, "REVISION_REF_UNRESOLVED") {
		t.Fatalf("ghost revision must be obsoleted with REVISION_REF_UNRESOLVED, got %s / %q", status, skipped)
	}
}

func derefP(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// TestRevisionIntakeClaimObsoletesOnStaleHash — the staleness invariant:
// the mirror moved on (a newer revision exists) → the job must be
// obsoleted, never claimed with stale content (review F2.3: the
// load-bearing CONTENT_HASH_CHANGED branch had no witness).
func TestRevisionIntakeClaimObsoletesOnStaleHash(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("original bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()
	job, minted, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "stale-key-1", RevisionSourceID: srcID, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "ATTIT1", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, seedRevision(srcID, hash)),
	})
	if err != nil || !minted {
		t.Fatalf("mint: %v %v", job, err)
	}
	// The mirror advances under the revision (a newer content hash).
	if _, err := d.Pool().Exec(ctx,
		`UPDATE zotero_attachments SET content_hash='newer-hash' WHERE zotero_key='ATTIT1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{
		WorkerID: "stale-it", LeaseDuration: 30 * time.Second,
		Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
	}); err != nil {
		t.Fatalf("claim (must obsolete, not error): %v", err)
	}
	var status, msg string
	if err := d.Pool().QueryRow(ctx,
		`SELECT status::text, COALESCE(error_message,'') FROM ingest_jobs WHERE id=$1`, job.ID).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "skipped" || !strings.Contains(msg, "CONTENT_HASH_CHANGED") {
		t.Fatalf("stale revision must be obsoleted with CONTENT_HASH_CHANGED, got %s / %q", status, msg)
	}
}

// TestRevisionIntakeClaimObsoletesWhenSyncLaneHoldsThePair — the
// transition-collision guard (review F1.1): a legacy job already holds the
// resolved (attachment, content_hash); the revision claim must NOT enter
// the legacy idempotency partial index (unique violation would poison the
// FIFO head forever) — it obsoletes with a readable reason instead.
func TestRevisionIntakeClaimObsoletesWhenSyncLaneHoldsThePair(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("collision bytes"))
	srcID, docID, attID := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()
	// The legacy lane's job (completed — idempotency-index rows keep the
	// (attachment, hash) pair regardless of status).
	if _, err := d.Pool().Exec(ctx,
		`INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		 VALUES ($1,$2,$3,$4,'completed',3)`, srcID, docID, attID, hash); err != nil {
		t.Fatal(err)
	}
	job, minted, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "coll-key-1", RevisionSourceID: srcID, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "ATTIT1", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, seedRevision(srcID, hash)),
	})
	if err != nil || !minted {
		t.Fatalf("mint: %v %v", job, err)
	}
	if _, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{
		WorkerID: "coll-it", LeaseDuration: 30 * time.Second,
		Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
	}); err != nil {
		t.Fatalf("claim must not hit the legacy unique index: %v", err)
	}
	var status, msg string
	if err := d.Pool().QueryRow(ctx,
		`SELECT status::text, COALESCE(error_message,'') FROM ingest_jobs WHERE id=$1`, job.ID).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "skipped" || !strings.Contains(msg, "REVISION_SUPERSEDED_BY_SYNC_LANE") {
		t.Fatalf("collision must be obsoleted with REVISION_SUPERSEDED_BY_SYNC_LANE, got %s / %q", status, msg)
	}
}

// TestRevisionIntakeClaimObsoletesNonPreferred — preferred parity with the
// legacy lane (review F1.3): a live but non-preferred rendition must skip
// at claim (completion would refuse it anyway), not burn processing runs.
func TestRevisionIntakeClaimObsoletesNonPreferred(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("non preferred bytes"))
	srcID, docID, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()
	// Demote the seeded attachment; add a preferred sibling.
	if _, err := d.Pool().Exec(ctx,
		`UPDATE zotero_attachments SET preferred=false WHERE zotero_key='ATTIT1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
		VALUES ($1,$2,'ATTPREF',1,'DOCIT1','imported_file','application/pdf','p.pdf','/tmp/p.pdf','pref-hash',true,false)`,
		srcID, docID); err != nil {
		t.Fatal(err)
	}
	job, minted, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "pref-key-1", RevisionSourceID: srcID, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "ATTIT1", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, seedRevision(srcID, hash)),
	})
	if err != nil || !minted {
		t.Fatalf("mint: %v %v", job, err)
	}
	if _, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{
		WorkerID: "pref-it", LeaseDuration: 30 * time.Second,
		Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	var status, msg string
	if err := d.Pool().QueryRow(ctx,
		`SELECT status::text, COALESCE(error_message,'') FROM ingest_jobs WHERE id=$1`, job.ID).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "skipped" || !strings.Contains(msg, "ATTACHMENT_NOT_PREFERRED") {
		t.Fatalf("non-preferred rendition must skip with ATTACHMENT_NOT_PREFERRED, got %s / %q", status, msg)
	}
}

// TestRevisionIntakeReplayAcrossKeyOrdering — pins the JSONB semantic
// comparison (review F2.5): a replay whose JSON text differs only in key
// order still resolves to the SAME job (a byte/text comparison regression
// goes red here).
func TestRevisionIntakeReplayAcrossKeyOrdering(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("key order bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()
	req := repo.IntakeRequest{
		IdempotencyKey: "order-key-1", RevisionSourceID: srcID, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "ATTIT1", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, seedRevision(srcID, hash)),
	}
	first, minted, err := rep.EnqueueRevisionIntake(ctx, req)
	if err != nil || !minted {
		t.Fatalf("mint: %v %v", first, err)
	}
	// Same revision, semantically identical, textually reordered: the
	// canonical JSON round-tripped through a generic map (Go marshals map
	// keys sorted — a DIFFERENT textual order, the SAME JSONB value).
	var generic any
	if err := json.Unmarshal(mustCanonical(t, seedRevision(srcID, hash)), &generic); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	if string(reordered) == string(mustCanonical(t, seedRevision(srcID, hash))) {
		t.Fatal("test bug: round-trip must actually reorder the text")
	}
	req.RevisionJSON = reordered
	again, minted2, err := rep.EnqueueRevisionIntake(ctx, req)
	if err != nil || minted2 || again == nil || again.ID != first.ID {
		t.Fatalf("reordered replay must be the SAME job: %+v minted=%v err=%v", again, minted2, err)
	}
}
