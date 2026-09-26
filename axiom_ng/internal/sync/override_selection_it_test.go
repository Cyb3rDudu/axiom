// A2 #166: wiring proof for the selection flow through Service.Run — the
// request override reaches the job gate, is NOT persisted (once-ness), and a
// persisted selection gates later runs without any override. Every gating
// step is non-vacuous: the job row is deleted before it, so "no job" can only
// come from the gate.
package sync

import (
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/mirror"
	"context"
	"encoding/json"
	"log"
	"os"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
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
	src.items = []zoteroprovider.CanonicalItem{
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
	mir := mirror.New(repoObj)
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
	if err := mir.SetSelections(ctx, []mirror.SelectionInput{{DocumentID: docID, Mode: "excluded"}}); err != nil {
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

// #166 arrow-4 witness: a PERSISTED COLLECTION selection gates the real
// Service.Run. The repo-direct cascade ITs bypass the syncer — a regression
// reverting the syncer to doc-only gating would leave them green. One run
// with the collection included enqueues jobs exactly for the collection's
// documents and none for an outside document.
func TestCollectionSelectionGatesSyncIT(t *testing.T) {
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

	pdfIn := t.TempDir() + "/in.pdf"
	pdfOut := t.TempDir() + "/out.pdf"
	os.WriteFile(pdfIn, []byte("coll-in"), 0o600)
	os.WriteFile(pdfOut, []byte("coll-out"), 0o600)

	src := &canonicalFake{serverID: "selcoll", baseURL: newScriptedBase(), version: 3}
	src.items = []zoteroprovider.CanonicalItem{
		// The parent's raw_data carries its collection — rebuildMemberships
		// derives the membership (and thus the collection expansion) from it.
		mkItemJSON("CB1", "book", "", "In Collection", map[string]any{
			"creators":    []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
			"collections": []string{"COLL0001"},
		}),
		mkItemJSON("CA1", "attachment", "CB1", "in.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "in.pdf",
		}),
		mkItemJSON("CB2", "book", "", "Outside", map[string]any{
			"creators": []map[string]string{{"firstName": "Bob", "lastName": "Babbage", "creatorType": "author"}},
		}),
		mkItemJSON("CA2", "attachment", "CB2", "out.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "out.pdf",
		}),
	}
	envFor := func(key, parent, name, pdf string) []byte {
		b, _ := json.Marshal(map[string]any{
			"key": key, "version": 1,
			"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdf}},
			"data":  map[string]any{"key": key, "version": 1, "itemType": "attachment", "parentItem": parent, "contentType": "application/pdf", "filename": name},
		})
		return b
	}
	src.items[1].Envelope = envFor("CA1", "CB1", "in.pdf", pdfIn)
	src.items[3].Envelope = envFor("CA2", "CB2", "out.pdf", pdfOut)
	src.collections = []zoteroprovider.CanonicalCollection{
		{Key: "COLL0001", Name: "Coll", Envelope: json.RawMessage(`{"key":"COLL0001","data":{"key":"COLL0001","name":"Coll","parentCollection":false}}`)},
	}

	repoObj := repo.New(d.Pool())
	mir := mirror.New(repoObj)
	svc := New(src, repoObj, src.baseURL, "users/0", log.Default())

	delJobs := func(sourceID string) {
		if _, err := d.Pool().Exec(ctx, `
			DELETE FROM ingest_jobs j USING zotero_attachments a
			WHERE a.id=j.attachment_id AND a.source_id=$1`, sourceID); err != nil {
			t.Fatal(err)
		}
	}
	docJobs := func(sourceID string) map[string]int {
		rows, err := d.Pool().Query(ctx, `
			SELECT d.zotero_key, count(*) FROM ingest_jobs j
			JOIN zotero_attachments a ON a.id=j.attachment_id
			JOIN zotero_documents d ON d.id=a.document_id
			WHERE a.source_id=$1 GROUP BY d.zotero_key`, sourceID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				t.Fatal(err)
			}
			out[k] = n
		}
		return out
	}

	// Run 1: plain sync — both documents get their jobs.
	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res.Enqueued != 2 {
		t.Fatalf("run 1 must enqueue both jobs, got %d", res.Enqueued)
	}

	// Persist the collection selection (the PRIMARY layer). Reset via defer:
	// the gate is GLOBAL — a leaked row would gate every later test's docs.
	if err := mir.SetSelectionBatch(ctx, nil,
		[]mirror.CollectionSelectionInput{{CollectionKey: "COLL0001", Mode: "included"}}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mir.SetSelectionBatch(ctx, nil,
			[]mirror.CollectionSelectionInput{{CollectionKey: "COLL0001", Mode: "default"}}); err != nil {
			t.Fatal(err)
		}
	}()

	// Run 2: no override — the collection selection alone must gate: a job
	// exactly for the collection's document, none for the outside one.
	delJobs(res.SourceID)
	res, err = svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	jobs := docJobs(res.SourceID)
	if res.Enqueued != 1 || jobs["CB1"] != 1 || jobs["CB2"] != 0 {
		t.Fatalf("collection selection must gate through Service.Run: enqueued=%d jobs=%v (want exactly CB1)", res.Enqueued, jobs)
	}
}

// TestSyncIncludeOverrideEnqueuesJob (#282 review fix): the post-heal
// auto-sync relies on Service.Run with a one-run INCLUDE actually
// enqueueing the included document's attachment — the seam between the
// invoker's targeted sync call and the wave gate's release. No collection
// selections are present here (the documented #166 boundary: with
// collection selections active, an include never resurrects a document
// outside the collection base — that case stays bounded by the wave
// gate's 1h strand window and is documented, not silently enqueued).
func TestSyncIncludeOverrideEnqueuesJob(t *testing.T) {
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

	pdfPath := t.TempDir() + "/healed.pdf"
	os.WriteFile(pdfPath, []byte("healed-bytes"), 0o600)

	src := &canonicalFake{serverID: "healsync", baseURL: newScriptedBase(), version: 2}
	src.items = []zoteroprovider.CanonicalItem{
		mkItemJSON("HB1", "book", "", "Healed Book", map[string]any{
			"creators": []map[string]string{{"firstName": "Grace", "lastName": "Hopper", "creatorType": "author"}},
		}),
		mkItemJSON("HA1", "attachment", "HB1", "healed.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "healed.pdf",
		}),
	}
	env, _ := json.Marshal(map[string]any{
		"key": "HA1", "version": 2,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "HA1", "version": 2, "itemType": "attachment", "parentItem": "HB1", "contentType": "application/pdf", "filename": "healed.pdf"},
	})
	src.items[1].Envelope = env

	repoObj := repo.New(d.Pool())
	svc := New(src, repoObj, src.baseURL, "users/0", log.Default())

	// first sync projects + enqueues (the document enters the world)
	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	var docID string
	if err := d.Pool().QueryRow(ctx,
		`SELECT id::text FROM zotero_documents WHERE zotero_key='HB1' AND source_id=$1`, res.SourceID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	// simulate the healed state: job consumed, NEW attachment version in
	// Zotero (the heal uploaded a new file) → next sync sees a change
	if _, err := d.Pool().Exec(ctx, `DELETE FROM ingest_jobs`); err != nil {
		t.Fatal(err)
	}
	src.version = 3
	src.items[1].Version = 3
	env2, _ := json.Marshal(map[string]any{
		"key": "HA1", "version": 3,
		"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdfPath}},
		"data":  map[string]any{"key": "HA1", "version": 3, "itemType": "attachment", "parentItem": "HB1", "contentType": "application/pdf", "filename": "healed.pdf"},
	})
	src.items[1].Envelope = env2

	// the #282 call shape: targeted include of exactly the healed document
	res2, err := svc.Run(ctx, &SyncOverride{Include: []string{docID}})
	if err != nil {
		t.Fatalf("include run: %v", err)
	}
	if res2.Enqueued < 1 {
		t.Fatalf("include sync must enqueue the healed document's job, got %d", res2.Enqueued)
	}
	var pending int
	if err := d.Pool().QueryRow(ctx, `
		SELECT count(*) FROM ingest_jobs j
		JOIN zotero_attachments a ON a.id=j.attachment_id
		WHERE a.source_id=$1 AND j.status='pending'`, res2.SourceID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending < 1 {
		t.Fatal("the enqueued healed job must be pending in the queue — the wave gate releases on exactly this")
	}
}

// TestSyncIncludeUnderCollectionSelectionInBase (#282 review follow-up):
// with collection selections ACTIVE, a healed document that is IN the
// collection base re-enqueues on its content-hash change — the enqueue is
// driven by the change, not by the include override (which the collection
// cascade deliberately ignores per #166). This pins that the post-heal
// auto-sync keeps working under collection expansion for every document
// still inside the base; only out-of-base heals strand (documented,
// 1h-bounded). The OUT-of-base counter-shape is covered by the selection
// cascade tests themselves.
func TestSyncIncludeUnderCollectionSelectionInBase(t *testing.T) {
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

	pdf := t.TempDir() + "/healed-in-base.pdf"
	os.WriteFile(pdf, []byte("v1"), 0o600)

	src := &canonicalFake{serverID: "healsynccoll", baseURL: newScriptedBase(), version: 2}
	src.items = []zoteroprovider.CanonicalItem{
		mkItemJSON("HCB1", "book", "", "Healed In Collection", map[string]any{
			"creators":    []map[string]string{{"firstName": "Ada", "lastName": "Lovelace", "creatorType": "author"}},
			"collections": []string{"HCOLL1"},
		}),
		mkItemJSON("HCA1", "attachment", "HCB1", "h.pdf", map[string]any{
			"contentType": "application/pdf", "filename": "h.pdf",
		}),
	}
	env := func(ver int) []byte {
		b, _ := json.Marshal(map[string]any{
			"key": "HCA1", "version": ver,
			"links": map[string]any{"enclosure": map[string]any{"href": "file://" + pdf}},
			"data":  map[string]any{"key": "HCA1", "version": ver, "itemType": "attachment", "parentItem": "HCB1", "contentType": "application/pdf", "filename": "h.pdf"},
		})
		return b
	}
	src.items[1].Envelope = env(2)
	src.collections = []zoteroprovider.CanonicalCollection{
		{Key: "HCOLL1", Name: "HColl", Envelope: json.RawMessage(`{"key":"HCOLL1","data":{"key":"HCOLL1","name":"HColl","parentCollection":false}}`)},
	}

	repoObj := repo.New(d.Pool())
	svc := New(src, repoObj, src.baseURL, "users/0", log.Default())

	// collection-include selection on the healed doc's collection —
	// CLEANUP: the sync ITs share the DSN database; a leftover selection
	// row suppresses every other test's documents (out-of-base).
	res, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO zotero_collection_selections (collection_key, mode)
		VALUES ('HCOLL1', 'included')`); err != nil {
		t.Fatal(err)
	}
	// DEFER (not t.Cleanup): cleanup callbacks run AFTER the deferred
	// pool close — the delete would hit a closed pool and silently no-op.
	defer func() {
		_, _ = d.Pool().Exec(ctx, `DELETE FROM zotero_collection_selections WHERE collection_key='HCOLL1'`)
	}()
	var docID string
	if err := d.Pool().QueryRow(ctx,
		`SELECT id::text FROM zotero_documents WHERE zotero_key='HCB1' AND source_id=$1`, res.SourceID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	// consume the job + change the content (the heal's effect)
	if _, err := d.Pool().Exec(ctx, `DELETE FROM ingest_jobs`); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(pdf, []byte("healed-v2"), 0o600)
	src.version = 3
	src.items[1].Version = 3
	src.items[1].Envelope = env(3)

	// the #282 call shape under an ACTIVE collection selection
	res2, err := svc.Run(ctx, &SyncOverride{Include: []string{docID}})
	if err != nil {
		t.Fatalf("include run under collection selection: %v", err)
	}
	if res2.Enqueued < 1 {
		t.Fatalf("in-base healed document must re-enqueue under collection selection (hash change drives it), got %d", res2.Enqueued)
	}
}
