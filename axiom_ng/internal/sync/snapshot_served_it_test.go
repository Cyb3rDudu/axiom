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

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
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
	src.items = []zotero.CanonicalItem{
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
