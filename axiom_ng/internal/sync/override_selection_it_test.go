// A2 #166: wiring proof for the selection flow through Service.Run — the
// request override reaches the job gate, is NOT persisted (once-ness), and a
// persisted selection gates later runs without any override. Every gating
// step is non-vacuous: the job row is deleted before it, so "no job" can only
// come from the gate.
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

func TestOverrideOnceAndPersistedSelectionIT(t *testing.T) {
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

	pdfPath := t.TempDir() + "/o.pdf"
	os.WriteFile(pdfPath, []byte("override-once"), 0o600)

	src := &canonicalFake{serverID: "selonce", baseURL: newScriptedBase(), version: 3}
	src.items = []zotero.CanonicalItem{
		mkItemJSON("OB1", "book", "", "Override Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
		}),
		mkItemJSON("OA1", "attachment", "OB1", "o.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "o.pdf",
		}),
	}
	env, _ := json.Marshal(map[string]any{
		"key": "OA1", "version": 1,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "OA1", "version": 1, "itemType": "attachment", "parentItem": "OB1", "contentType": "application/pdf", "filename": "o.pdf"},
	})
	src.items[1].Envelope = env

	repoObj := repo.New(d.Pool())
	svc := New(src, repoObj, src.baseURL, "users/0", log.Default())

	jobCount := func(sourceID string) int {
		var n int
		if err := d.Pool().QueryRow(ctx, `
			SELECT count(*) FROM ingest_jobs j
			JOIN zotero_attachments a ON a.id=j.attachment_id
			WHERE a.source_id=$1`, sourceID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	delJobs := func(sourceID string) {
		if _, err := d.Pool().Exec(ctx, `
			DELETE FROM ingest_jobs j USING zotero_attachments a
			WHERE a.id=j.attachment_id AND a.source_id=$1`, sourceID); err != nil {
			t.Fatal(err)
		}
	}

	// Run 1: plain sync projects the document and enqueues the job.
	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res.Enqueued != 1 || jobCount(res.SourceID) != 1 {
		t.Fatalf("run 1 must enqueue the one job, got %d/%d", res.Enqueued, jobCount(res.SourceID))
	}
	var docID string
	if err := d.Pool().QueryRow(ctx,
		`SELECT id::text FROM zotero_documents WHERE zotero_key='OB1' AND source_id=$1`, res.SourceID).Scan(&docID); err != nil {
		t.Fatal(err)
	}

	// Run 2: override excluding the document -> the gate suppresses the job
	// (row deleted first, so zero can only mean the override reached the gate).
	delJobs(res.SourceID)
	res, err = svc.Run(ctx, &SyncOverride{Exclude: []string{docID}})
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res.Enqueued != 0 || jobCount(res.SourceID) != 0 {
		t.Fatalf("override-excluded doc must stay jobless, got %d/%d", res.Enqueued, jobCount(res.SourceID))
	}

	// Run 3: NO override -> the job appears again. The override was never
	// persisted (once-ness) and the derivation re-offers the document.
	res, err = svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if res.Enqueued != 1 || jobCount(res.SourceID) != 1 {
		t.Fatalf("post-override run must re-offer the job, got %d/%d", res.Enqueued, jobCount(res.SourceID))
	}

	// Run 4: PERSISTED selection excluded, no override -> the gate holds.
	if err := repoObj.SetSelections(ctx, []repo.SelectionInput{{DocumentID: docID, Mode: "excluded"}}); err != nil {
		t.Fatal(err)
	}
	delJobs(res.SourceID)
	res, err = svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 4: %v", err)
	}
	if res.Enqueued != 0 || jobCount(res.SourceID) != 0 {
		t.Fatalf("persisted-excluded doc must stay jobless, got %d/%d", res.Enqueued, jobCount(res.SourceID))
	}
}
