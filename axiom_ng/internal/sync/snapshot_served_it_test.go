// #294 sync-side defense in depth: a document whose ENTIRE job history is
// gone (the retention bug's production footprint) must NOT be re-enqueued
// as "never processed" while its attachment holds an ACTIVE snapshot with
// the SAME content hash — the snapshot is the proof of processing. A
// changed hash matches no active snapshot and enqueues normally.
//
// Red probe (guard removed from writeJobsTx): run 2 enqueues 1 job for the
// snapshot-served document — exactly the 91-job production requeue.
package sync

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

func TestSnapshotServedNoRequeueIT(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pdfPath := t.TempDir() + "/s.pdf"
	os.WriteFile(pdfPath, []byte("snapshot-served"), 0o600)

	src := &canonicalFake{serverID: "snapserved", baseURL: newScriptedBase(), version: 2}
	src.items = []zoteroprovider.CanonicalItem{
		mkItemJSON("SB1", "book", "", "Snapshot Served Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
		}),
		mkItemJSON("SA1", "attachment", "SB1", "s.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "s.pdf",
		}),
	}
	env, _ := json.Marshal(map[string]any{
		"key": "SA1", "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "SA1", "version": 1, "itemType": "attachment", "parentItem": "SB1", "contentType": "application/pdf", "filename": "s.pdf"},
	})
	src.items[1].Envelope = env

	repoObj := repo.New(d.Pool())
	svc := New(src, repoObj, src.baseURL, "users/0", log.Default())

	// Run 1: projects the document, enqueues exactly one job.
	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res.Enqueued != 1 {
		t.Fatalf("run 1 enqueued = %d, want 1", res.Enqueued)
	}
	var attID, contentHash string
	if err := d.Pool().QueryRow(ctx, `
		SELECT a.id::text, a.content_hash FROM zotero_attachments a
		WHERE a.source_id=$1 AND a.zotero_key='SA1'`, res.SourceID).Scan(&attID, &contentHash); err != nil {
		t.Fatal(err)
	}
	if contentHash == "" {
		t.Fatal("fixture attachment must carry a content hash")
	}
	_ = contentHash // attID drives the fixture below; the hash rides the attachment row

	// The completed processing footprint: an ACTIVE snapshot for this
	// attachment + hash, and (the production damage) NO job rows at all.
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name, processor_version,
			profile_hash, document_id, profile, active)
		SELECT a.id, a.content_hash, 'p', 'v1', 'ph', a.document_id, '{}', true
		FROM zotero_attachments a WHERE a.id=$1::uuid`, attID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `
		DELETE FROM ingest_jobs j USING zotero_attachments a
		WHERE a.id=j.attachment_id AND a.source_id=$1`, res.SourceID); err != nil {
		t.Fatal(err)
	}

	// Run 2 (#294 pin): unchanged file, active snapshot, zero job rows →
	// NOT enqueued. The snapshot is the proof of processing.
	res2, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res2.Enqueued != 0 {
		t.Fatalf("run 2 enqueued = %d, want 0 — snapshot-served document must not requeue (the 91-job production storm)", res2.Enqueued)
	}
	var jobs int
	if err := d.Pool().QueryRow(ctx, `
		SELECT count(*) FROM ingest_jobs j
		JOIN zotero_attachments a ON a.id=j.attachment_id
		WHERE a.source_id=$1`, res.SourceID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("job rows = %d, want 0", jobs)
	}

	// Run 3 (control): the file CHANGES — no active snapshot matches the
	// new hash → the job enqueues normally (new content must process).
	os.WriteFile(pdfPath, []byte("snapshot-served-v2"), 0o600)
	src.version = 3
	src.items[1].Envelope, _ = json.Marshal(map[string]any{
		"key": "SA1", "version": 2,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "SA1", "version": 2, "itemType": "attachment", "parentItem": "SB1", "contentType": "application/pdf", "filename": "s.pdf"},
	})
	res3, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if res3.Enqueued != 1 {
		t.Fatalf("run 3 enqueued = %d, want 1 — changed content must enqueue despite the active old-hash snapshot", res3.Enqueued)
	}
}

// TestSuppressedEnqueueDoesNotResolveFailuresIT (#294 review MAJOR 2): a
// SUPPRESSED pending insert (snapshot served) must not fire the
// failed-row resolution — and the resolution must never bump updated_at.
// Pre-fix hole (reviewer-reproduced on HEAD): the unconditional UPDATE
// set resolved_at=now(), updated_at=now() on the stale failed row even
// when nothing was enqueued, re-ranking it above the completed outcome
// anchor on the (updated_at, id) key; retention then pruned the anchor
// while the sync defense kept the document served at outcome=failed
// forever (the post-apply invariant stayed silent — a row survived).
func TestSuppressedEnqueueDoesNotResolveFailuresIT(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pdfPath := t.TempDir() + "/r.pdf"
	os.WriteFile(pdfPath, []byte("anchor-book"), 0o600)

	src := &canonicalFake{serverID: "snapanchor", baseURL: newScriptedBase(), version: 2}
	src.items = []zoteroprovider.CanonicalItem{
		mkItemJSON("RB1", "book", "", "Anchor Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
		}),
		mkItemJSON("RA1", "attachment", "RB1", "r.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "r.pdf",
		}),
	}
	env, _ := json.Marshal(map[string]any{
		"key": "RA1", "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "RA1", "version": 1, "itemType": "attachment", "parentItem": "RB1", "contentType": "application/pdf", "filename": "r.pdf"},
	})
	src.items[1].Envelope = env

	repoObj := repo.New(d.Pool())
	svc := New(src, repoObj, src.baseURL, "users/0", log.Default())

	res, err := svc.Run(ctx, nil)
	if err != nil || res.Enqueued != 1 {
		t.Fatalf("run 1: err=%v enqueued=%d, want 1", err, res.Enqueued)
	}
	var attID, docID string
	if err := d.Pool().QueryRow(ctx, `
		SELECT a.id::text, a.document_id::text FROM zotero_attachments a
		WHERE a.source_id=$1 AND a.zotero_key='RA1'`, res.SourceID).Scan(&attID, &docID); err != nil {
		t.Fatal(err)
	}
	// the processed footprint: ACTIVE snapshot for the current hash; the
	// run-1 job row stands in for the completed anchor, aged past the
	// retention window on BOTH axes — the age filter reads enqueued_at,
	// the outcome key reads updated_at (aging only updated_at left the
	// anchor age-ineligible and the retention tail below vacuous).
	var anchor string
	if err := d.Pool().QueryRow(ctx, `
		UPDATE ingest_jobs SET status='completed', enqueued_at = now() - interval '21 days', updated_at = now() - interval '20 days'
		WHERE attachment_id=$1::uuid RETURNING id::text`, attID).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name, processor_version,
			profile_hash, document_id, profile, active)
		SELECT a.id, a.content_hash, 'p', 'v1', 'ph', a.document_id, '{}', true
		FROM zotero_attachments a WHERE a.id=$1::uuid`, attID); err != nil {
		t.Fatal(err)
	}
	// the stale failed sibling: unresolved, NULL hash, older than the anchor
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO ingest_jobs (status, attachment_id, document_id, content_hash, enqueued_at, updated_at, error_code, error_message)
		VALUES ('failed', $1::uuid, $2::uuid, NULL, now() - interval '21 days', now() - interval '21 days', 'X', 'stale failure')`,
		attID, docID); err != nil {
		t.Fatal(err)
	}

	// Run 2: unchanged file + active snapshot → suppressed insert; the
	// resolution must NOT fire (nothing was enqueued) and must NOT bump
	// updated_at (bookkeeping never re-ranks the outcome key).
	var updBefore string
	if err := d.Pool().QueryRow(ctx, `
		SELECT updated_at::text FROM ingest_jobs
		WHERE attachment_id=$1::uuid AND status='failed'`, attID).Scan(&updBefore); err != nil {
		t.Fatal(err)
	}
	res2, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res2.Enqueued != 0 {
		t.Fatalf("run 2 enqueued = %d, want 0", res2.Enqueued)
	}
	var resolved *string
	var updAfter string
	if err := d.Pool().QueryRow(ctx, `
		SELECT resolved_at::text, updated_at::text FROM ingest_jobs
		WHERE attachment_id=$1::uuid AND status='failed'`, attID).Scan(&resolved, &updAfter); err != nil {
		t.Fatal(err)
	}
	if resolved != nil {
		t.Fatal("suppressed enqueue resolved the stale failed row — resolution must only fire on an actual enqueue")
	}
	if updAfter != updBefore {
		t.Fatalf("suppressed enqueue bumped updated_at on the failed row (%s -> %s) — bookkeeping must not re-rank the outcome key", updBefore, updAfter)
	}

	// The anchor survives retention: the failed row never outranked it.
	// Ordering rationale: this tail must run while NO newer job exists —
	// a fresh pending job legitimately outranks the aged anchor on
	// (updated_at, id) and would make it prunable; and the apply itself
	// legitimately prunes the aged, outranked failed row. The
	// actual-enqueue resolution asserts therefore live in the sibling IT
	// below, which needs that failed row alive.
	rem, _, err := repoObj.ApplyRetention(ctx, 14*24*time.Hour, 0, nil)
	if err != nil {
		t.Fatalf("apply retention: %v", err)
	}
	var anchorLeft int
	if err := d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM ingest_jobs WHERE id=$1::uuid`, anchor).Scan(&anchorLeft); err != nil {
		t.Fatal(err)
	}
	if anchorLeft != 1 {
		t.Fatalf("completed outcome anchor was pruned (removals=%d) — the doc would read outcome=failed while served", rem.Jobs)
	}
	// The apply must have ENGAGED — else "anchor survives" above would
	// pass vacuously on a no-op apply: the aged, outranked failed row is
	// exactly the prunable attempt retention exists to remove.
	var failedLeft int
	if err := d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM ingest_jobs WHERE attachment_id=$1::uuid AND status='failed'`, attID).Scan(&failedLeft); err != nil {
		t.Fatal(err)
	}
	if failedLeft != 0 {
		t.Fatalf("stale failed row survived retention (removals=%d) — the aged, outranked attempt must be pruned", rem.Jobs)
	}
}

// TestActualEnqueueResolvesFailuresWithoutBumpIT (#294 review MAJOR 2,
// executing path): an ACTUAL enqueue — changed content, no active
// snapshot match — DOES fire the failed-row resolution, and the
// resolution must still not bump updated_at. This pins the half the
// suppressed-path IT above cannot reach: on a suppressed insert the
// resolution never runs, so restoring only the updated_at bump (gate
// kept) would keep that test green. Separate IT with no retention apply:
// the sibling's apply legitimately prunes the failed row this test
// needs alive.
func TestActualEnqueueResolvesFailuresWithoutBumpIT(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pdfPath := t.TempDir() + "/r.pdf"
	os.WriteFile(pdfPath, []byte("anchor-book"), 0o600)

	src := &canonicalFake{serverID: "snapresolve", baseURL: newScriptedBase(), version: 2}
	src.items = []zoteroprovider.CanonicalItem{
		mkItemJSON("RB1", "book", "", "Anchor Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
		}),
		mkItemJSON("RA1", "attachment", "RB1", "r.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "r.pdf",
		}),
	}
	env, _ := json.Marshal(map[string]any{
		"key": "RA1", "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "RA1", "version": 1, "itemType": "attachment", "parentItem": "RB1", "contentType": "application/pdf", "filename": "r.pdf"},
	})
	src.items[1].Envelope = env

	repoObj := repo.New(d.Pool())
	svc := New(src, repoObj, src.baseURL, "users/0", log.Default())

	res, err := svc.Run(ctx, nil)
	if err != nil || res.Enqueued != 1 {
		t.Fatalf("run 1: err=%v enqueued=%d, want 1", err, res.Enqueued)
	}
	var attID, docID string
	if err := d.Pool().QueryRow(ctx, `
		SELECT a.id::text, a.document_id::text FROM zotero_attachments a
		WHERE a.source_id=$1 AND a.zotero_key='RA1'`, res.SourceID).Scan(&attID, &docID); err != nil {
		t.Fatal(err)
	}
	// the completed anchor (aged on both axes) + the active snapshot of
	// its processing + the stale failed sibling, as in the IT above
	if _, err := d.Pool().Exec(ctx, `
		UPDATE ingest_jobs SET status='completed', enqueued_at = now() - interval '21 days', updated_at = now() - interval '20 days'
		WHERE attachment_id=$1::uuid`, attID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name, processor_version,
			profile_hash, document_id, profile, active)
		SELECT a.id, a.content_hash, 'p', 'v1', 'ph', a.document_id, '{}', true
		FROM zotero_attachments a WHERE a.id=$1::uuid`, attID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO ingest_jobs (status, attachment_id, document_id, content_hash, enqueued_at, updated_at, error_code, error_message)
		VALUES ('failed', $1::uuid, $2::uuid, NULL, now() - interval '21 days', now() - interval '21 days', 'X', 'stale failure')`,
		attID, docID); err != nil {
		t.Fatal(err)
	}

	var updBefore string
	if err := d.Pool().QueryRow(ctx, `
		SELECT updated_at::text FROM ingest_jobs
		WHERE attachment_id=$1::uuid AND status='failed'`, attID).Scan(&updBefore); err != nil {
		t.Fatal(err)
	}

	// The file CHANGES: no active snapshot matches the new hash, so the
	// pending insert is an ACTUAL enqueue — the resolution MUST fire now.
	os.WriteFile(pdfPath, []byte("anchor-book-v2"), 0o600)
	src.version = 3
	src.items[1].Envelope, _ = json.Marshal(map[string]any{
		"key": "RA1", "version": 2,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "RA1", "version": 2, "itemType": "attachment", "parentItem": "RB1", "contentType": "application/pdf", "filename": "r.pdf"},
	})
	res2, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res2.Enqueued != 1 {
		t.Fatalf("run 2 enqueued = %d, want 1 — changed content must enqueue despite the active old-hash snapshot", res2.Enqueued)
	}
	var resolved *string
	var updAfter string
	if err := d.Pool().QueryRow(ctx, `
		SELECT resolved_at::text, updated_at::text FROM ingest_jobs
		WHERE attachment_id=$1::uuid AND status='failed'`, attID).Scan(&resolved, &updAfter); err != nil {
		t.Fatal(err)
	}
	if resolved == nil {
		t.Fatal("actual enqueue left the stale failed row unresolved — a later real failure would be masked by the stale one")
	}
	if updAfter != updBefore {
		t.Fatalf("resolution bumped updated_at on the failed row (%s -> %s) — bookkeeping must not re-rank the outcome key", updBefore, updAfter)
	}
}
