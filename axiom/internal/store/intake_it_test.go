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

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/store"
	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
	storemigrations "github.com/Cyb3rDudu/axiom/axiom/internal/store/migrations"
)

func openIntakeDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping intake IT")
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

// TestRevisionReIntakeAfterObsoletion — the B3 witness: an obsoleted
// revision job (terminal, FK-less) must never block a re-intake once its
// transient cause resolved — the identity arbiter is scoped to ACTIVE
// rows, so the second mint creates a FRESH pending job.
func TestRevisionReIntakeAfterObsoletion(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("re-intake bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()

	// Obsolete the only job via the ghost-rendition route: mint against a
	// rendition that does not exist yet, claim → skipped.
	ghost := seedRevision(srcID, hash)
	ghost.RenditionID = "GHOST1"
	first, minted, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "rearm-1", RevisionSourceID: srcID, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "GHOST1", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, ghost),
	})
	if err != nil || !minted {
		t.Fatalf("ghost mint: %v %v", first, err)
	}
	if _, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{WorkerID: "rearm", LeaseDuration: 30 * time.Second, Profile: json.RawMessage(`{"profile":"full-rag-v1"}`)}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// The transient cause resolves: the rendition appears in the mirror.
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
		SELECT $1::uuid, d.id, 'GHOST1', 1, 'DOCIT1', 'imported_file','application/pdf','g.pdf','/tmp/g.pdf',$2,true,false
		FROM zotero_documents d WHERE d.zotero_key='DOCIT1' AND d.source_id::text=$1::text`, srcID, hash); err != nil {
		t.Fatal(err)
	}

	// Re-intake with a NEW key: must mint a FRESH pending job (not join
	// the corpse), claimable immediately.
	second, minted2, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "rearm-2", RevisionSourceID: srcID, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "GHOST1", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, ghost),
	})
	if err != nil || second == nil || !minted2 {
		t.Fatalf("re-intake must mint a fresh job: %+v minted=%v err=%v", second, minted2, err)
	}
	if second.ID == first.ID {
		t.Fatalf("re-intake joined the obsoleted corpse %s", first.ID)
	}
	if second.Status != "pending" {
		t.Fatalf("fresh mint status %q, want pending", second.Status)
	}
	claimed, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{WorkerID: "rearm2", LeaseDuration: 30 * time.Second, Profile: json.RawMessage(`{"profile":"full-rag-v1"}`)})
	if err != nil || claimed == nil || claimed.JobID != second.ID {
		t.Fatalf("the fresh job must be claimable: %v %v", claimed, err)
	}
}

// TestRevisionIntakeCrossSourceNoFalseDedup — the M1 witness: identical
// rendition key + content under TWO sources are distinct identities; B
// mints its own job (the old source-less index answered A's job and
// silently dropped B).
func TestRevisionIntakeCrossSourceNoFalseDedup(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("cross source bytes"))
	srcA, _, _ := seedMirror(t, d, "DOCA", "ATTDUP", hash)
	// Source B with its OWN document + attachment using the SAME keys.
	var srcB, docB, itemB, attB string
	ctx := context.Background()
	if err := d.Pool().QueryRow(ctx, `INSERT INTO zotero_sources (base_url, library_id, server_id) VALUES ('https://b.local','users/0','srv') RETURNING id::text`).Scan(&srcB); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool().QueryRow(ctx, `INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title) VALUES ($1,'DOCA',1,'book','B') RETURNING id::text`, srcB).Scan(&docB); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool().QueryRow(ctx, `INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data) VALUES ($1,'DOCA',1,'book',NULL,'{}','{}') RETURNING id::text`, srcB).Scan(&itemB); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docB, itemB); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool().QueryRow(ctx, `INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted) VALUES ($1,$2,'ATTDUP',1,'DOCA','imported_file','application/pdf','b.pdf','/tmp/b.pdf',$3,true,false) RETURNING id::text`, srcB, docB, hash).Scan(&attB); err != nil {
		t.Fatal(err)
	}

	rep := repo.New(d.Pool())
	revA := seedRevision(srcA, hash)
	revA.RenditionID = "ATTDUP"
	revA.Bibliography.RecordID = "DOCA"
	jobA, mintedA, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "xsrc-a", RevisionSourceID: srcA, RevisionRecordID: "DOCA",
		RevisionRenditionID: "ATTDUP", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, revA),
	})
	if err != nil || !mintedA {
		t.Fatalf("mint A: %v %v", jobA, err)
	}
	revB := seedRevision(srcB, hash)
	revB.RenditionID = "ATTDUP"
	revB.Bibliography.RecordID = "DOCA"
	jobB, mintedB, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "xsrc-b", RevisionSourceID: srcB, RevisionRecordID: "DOCA",
		RevisionRenditionID: "ATTDUP", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, revB),
	})
	if err != nil || jobB == nil || !mintedB {
		t.Fatalf("source B must mint its OWN job: %+v minted=%v err=%v", jobB, mintedB, err)
	}
	if jobB.ID == jobA.ID {
		t.Fatalf("cross-source dedup: B joined A's job %s", jobA.ID)
	}
	// Claim independence: both jobs resolve against their OWN mirror rows
	// (the claim lane is source-scoped) — A claims first, then B.
	claimedA, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{WorkerID: "xsrc-a", LeaseDuration: 30 * time.Second, Profile: json.RawMessage(`{"profile":"full-rag-v1"}`)})
	if err != nil || claimedA == nil || claimedA.JobID != jobA.ID {
		t.Fatalf("A must claim its own job: %v %v", claimedA, err)
	}
	claimedB, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{WorkerID: "xsrc-b", LeaseDuration: 30 * time.Second, Profile: json.RawMessage(`{"profile":"full-rag-v1"}`)})
	if err != nil || claimedB == nil || claimedB.JobID != jobB.ID {
		t.Fatalf("B must claim its own job: %v %v", claimedB, err)
	}
	if claimedA.AttachmentID == claimedB.AttachmentID {
		t.Fatalf("cross-source claims must resolve DIFFERENT attachments: %s", claimedA.AttachmentID)
	}
}

// TestRevisionSameKeyReplayAfterObsoletion — pins the same-key half of
// the re-intake semantics (review R4-4): the intake-key idempotency
// answers the terminal job as a REPLAY (same key, same durable answer —
// the API reports the job's terminal failure honestly), while a NEW key
// re-mints (the cross-way witness above). The migration header's
// "never blocks a re-intake" claim is the new-key contract.
func TestRevisionSameKeyReplayAfterObsoletion(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("same key obsoletion bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()

	rev := seedRevision(srcID, hash)
	rev.RenditionID = "GHOSTK"
	req := repo.IntakeRequest{
		IdempotencyKey: "samekey-1", RevisionSourceID: srcID, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "GHOSTK", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, rev),
	}
	first, minted, err := rep.EnqueueRevisionIntake(ctx, req)
	if err != nil || !minted {
		t.Fatalf("mint: %v %v", first, err)
	}
	if _, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{WorkerID: "sk", LeaseDuration: 30 * time.Second, Profile: json.RawMessage(`{"profile":"full-rag-v1"}`)}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Same key, identical revision: the terminal corpse answers as replay.
	again, minted2, err := rep.EnqueueRevisionIntake(ctx, req)
	if err != nil || minted2 || again == nil || again.ID != first.ID {
		t.Fatalf("same-key replay must return the terminal job: %+v minted=%v err=%v", again, minted2, err)
	}
	if again.Status != "skipped" {
		t.Fatalf("replay must report the terminal status, got %q", again.Status)
	}
	// The DTO carries the Failure (the API-visible truth).
	dto := ingestJobDTO(again, rev)
	if dto.Status != store.IngestTerminalFailed || dto.Failure == nil {
		t.Fatalf("terminal replay must carry Failure: %+v", dto)
	}
}

// TestRevisionIntakeClaimObsoletesOnGhostSourceAndDocument — the DM06
// #315 intake witnesses for the references the dropped cross-component
// FKs used to secure at the database level (the rendition ghost has its
// own witness above): a revision whose SOURCE does not exist, and one
// whose DOCUMENT record does not exist, must both be obsoleted LOUDLY
// with REVISION_REF_UNRESOLVED at claim time — never silently claimed,
// never silently resolved onto wrong rows. Runs post-drop: the contract
// layer is the only guard left, and this proves it holds.
func TestRevisionIntakeClaimObsoletesOnGhostSourceAndDocument(t *testing.T) {
	d := openIntakeDB(t)
	hash := revision.HashContent([]byte("ghost ref bytes"))
	srcID, _, _ := seedMirror(t, d, "DOCIT1", "ATTIT1", hash)
	rep := repo.New(d.Pool())
	ctx := context.Background()

	claim := func(t *testing.T) {
		t.Helper()
		if _, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{
			WorkerID: "intake-it", LeaseDuration: 30 * time.Second,
			Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
		}); err != nil {
			t.Fatalf("claim (must obsolete the ghost, not error): %v", err)
		}
	}
	assertObsoleted := func(t *testing.T, jobID, wantSub string) {
		t.Helper()
		var status, skipped string
		if err := d.Pool().QueryRow(ctx,
			`SELECT status::text, COALESCE(error_message,'') FROM ingest_jobs WHERE id=$1`, jobID).Scan(&status, &skipped); err != nil {
			t.Fatal(err)
		}
		if status != "skipped" || !strings.Contains(skipped, wantSub) {
			t.Fatalf("ghost reference must be obsoleted with %s, got %s / %q", wantSub, status, skipped)
		}
	}

	// Ghost SOURCE: the revision names a source uuid the mirror never saw
	// (a syntactically valid uuid that exists in no zotero_sources row).
	ghostSrc := "00000000-0000-4000-8000-00000000d006"
	rev := seedRevision(ghostSrc, hash)
	job, minted, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "ghost-src-1", RevisionSourceID: ghostSrc, RevisionRecordID: "DOCIT1",
		RevisionRenditionID: "ATTIT1", RevisionNo: "1", ContentHash: hash,
		RevisionJSON: mustCanonical(t, rev),
	})
	if err != nil || !minted {
		t.Fatalf("mint ghost source: %v %v", job, err)
	}
	claim(t)
	assertObsoleted(t, job.ID, "REVISION_REF_UNRESOLVED")

	// Ghost DOCUMENT: the source exists, the record does not — the claim
	// resolves source first and must obsolete at the document branch.
	// Own rendition key AND own content hash: the two ghost rows must stay
	// identical-free under EVERY index shape, including the status-blind
	// legacy identity index the ledger-wipe IT rebuilds (rendition+hash
	// alone) — a shared pair would poison that IT with 23505.
	docHash := revision.HashContent([]byte("ghost doc bytes"))
	rev2 := seedRevision(srcID, docHash)
	rev2.RenditionID = "ATTGHOSTDOC"
	rev2.Bibliography.RecordID = "GONEDOC"
	job2, minted2, err := rep.EnqueueRevisionIntake(ctx, repo.IntakeRequest{
		IdempotencyKey: "ghost-doc-1", RevisionSourceID: srcID, RevisionRecordID: "GONEDOC",
		RevisionRenditionID: "ATTGHOSTDOC", RevisionNo: "1", ContentHash: docHash,
		RevisionJSON: mustCanonical(t, rev2),
	})
	if err != nil || !minted2 {
		t.Fatalf("mint ghost document: %v %v", job2, err)
	}
	claim(t)
	assertObsoleted(t, job2.ID, "REVISION_REF_UNRESOLVED")
}
