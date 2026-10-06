// tombstone_it_test.go — the #358 held-row reconciliation probes: a
// Zotero delete reaches the mirror (delete event or full-reconcile
// absence), the document becomes a visible TOMBSTONE in the merged
// listing, and the Store phase retires its snapshot + marks the
// projection deleted once and visibly — never an eternal phantom.
package sync

import (
	"context"
	"encoding/json"
	"log"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/library/mirror"
	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom/internal/zoteroprovider"
)

func tombItem(key, parent, title string, v int64) zoteroprovider.CanonicalItem {
	dd := map[string]any{"key": key, "version": v, "itemType": "book", "title": title}
	if parent != "" {
		dd["itemType"] = "attachment"
		dd["parentItem"] = parent
		dd["contentType"] = "application/pdf"
		dd["filename"] = "t.pdf"
	}
	envB, _ := json.Marshal(map[string]any{"key": key, "version": v, "data": dd})
	dataB, _ := json.Marshal(dd)
	return zoteroprovider.CanonicalItem{Key: key, Version: v, ItemType: dd["itemType"].(string), ParentKey: parent, Envelope: envB, Data: dataB}
}

// TestZoteroDeleteBecomesTombstoneIT — the DoD probe: a document deleted
// in Zotero (delta delete event) deactivates in the mirror, the listing
// shows it ONCE as a tombstone (sync_state=tombstoned, outcome=removed),
// and the store phase marks the projection deleted + retires the active
// snapshot.
func TestZoteroDeleteBecomesTombstoneIT(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	// The persistent test DB carries tombstones from earlier runs (same
	// doc keys, different sources); clear the fixture keys AND their
	// orphaned store rows (snapshots lost their FKs with DM06) so
	// "exactly one tombstone" and the identity insert are well-defined.
	for _, stmt := range []string{
		`DELETE FROM processing_snapshots WHERE content_hash IN ('sha256:tomb','sha256:phantom')`,
		`DELETE FROM store_documents WHERE record_key IN ('TB1','PB1')`,
		`DELETE FROM zotero_documents WHERE zotero_key IN ('TB1','PB1')`,
	} {
		if _, err := d.Pool().Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	pdf := makePdf(t, "tomb")

	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zoteroprovider.CanonicalItem{tombItem("TB1", "", "Tomb Book", 10), tombItem("TA1", "TB1", "", 10)}
	src.items[1].Envelope = mustEnclosure(src.items[1], pdf)
	rep := repo.New(d.Pool())
	svc := New(src, mirror.New(rep.Pool()), rep, src.baseURL, "users/0", log.Default())

	res, err := svc.Run(ctx, nil)
	if err != nil || res.Enqueued != 1 {
		t.Fatalf("seed sync: %v %+v", err, res)
	}
	// The processed footprint: an active snapshot on the document.
	var attID, docID string
	if err := d.Pool().QueryRow(ctx,
		`SELECT attachment_id::text, document_id::text FROM store_documents WHERE rendition_key='TA1'`).Scan(&attID, &docID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1::uuid, 'sha256:tomb', 'p', 'v', 'ph', $2::uuid, '{}', true)`, attID, docID); err != nil {
		t.Fatal(err)
	}

	// The Zotero delete: a delta carrying the delete event.
	src.version = 11
	src.items = nil
	src.deleteEvents = []zoteroprovider.DeleteEvent{{Key: "TB1"}}
	res2, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("delete sync: %v", err)
	}
	if res2.Tombstoned != 1 {
		t.Fatalf("delete sync must report 1 tombstoned document, got %d", res2.Tombstoned)
	}

	// The listing shows the tombstone exactly once.
	mir := mirror.New(d.Pool())
	rows, err := mir.ListDocumentsMirror(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tombstones := 0
	for _, z := range rows {
		if z.ZoteroKey == "TB1" {
			tombstones++
			if z.SyncState != "tombstoned" || z.Outcome != "removed" {
				t.Fatalf("deleted doc must list tombstoned/removed, got %s/%s", z.SyncState, z.Outcome)
			}
		}
	}
	if tombstones != 1 {
		t.Fatalf("want exactly one tombstone row for TB1, got %d (of %d rows)", tombstones, len(rows))
	}
	merged := mirror.DocumentListing(rows, map[string]repo.JobState{}, map[string]bool{}, "")
	sawTomb := false
	for _, z := range merged {
		if z.ZoteroKey == "TB1" {
			sawTomb = true
			if z.SyncState != "tombstoned" {
				t.Fatalf("merge must pass tombstones through, got %s", z.SyncState)
			}
		}
	}
	if !sawTomb {
		t.Fatal("tombstone lost in the merge")
	}

	// The Store phase retired the deleted rendition's snapshot and marked
	// the projection deleted (the phantom is GONE, not lingering).
	var active bool
	if err := d.Pool().QueryRow(ctx, `SELECT active FROM processing_snapshots WHERE attachment_id=$1::uuid`, attID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("the deleted rendition's snapshot must retire in the same sync")
	}
	var deleted bool
	if err := d.Pool().QueryRow(ctx, `SELECT deleted FROM store_documents WHERE attachment_id=$1::uuid`, attID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("the deleted rendition's projection row must be marked deleted")
	}

	// The failure-path repair (#358 review C1): simulate a store phase
	// whose deletion mark was lost (rolled back after the mirror commit),
	// then a PLAIN delta sync (nothing changed in Zotero) — the
	// level-triggered re-report must re-apply the mark.
	if _, err := d.Pool().Exec(ctx,
		`UPDATE store_documents SET deleted=false, preferred=true WHERE attachment_id=$1::uuid`, attID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Run(ctx, nil); err != nil {
		t.Fatalf("repair sync: %v", err)
	}
	if err := d.Pool().QueryRow(ctx, `SELECT deleted FROM store_documents WHERE attachment_id=$1::uuid`, attID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("a lost deletion mark must be re-applied by the next sync (level-triggered report)")
	}
}

// TestFullReconcileClearsHeldPhantomsIT — the catch-up probe: items that
// vanished from Zotero WITHOUT a delete event (the pre-mirror phantoms)
// are reconciled away by a FULL sync (since=0): tombstoned in the mirror,
// projections deactivated, store rows retired.
func TestFullReconcileClearsHeldPhantomsIT(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	for _, stmt := range []string{
		`DELETE FROM processing_snapshots WHERE content_hash IN ('sha256:tomb','sha256:phantom')`,
		`DELETE FROM store_documents WHERE record_key IN ('TB1','PB1')`,
		`DELETE FROM zotero_documents WHERE zotero_key IN ('TB1','PB1')`,
	} {
		if _, err := d.Pool().Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	pdf := makePdf(t, "phantom")

	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zoteroprovider.CanonicalItem{tombItem("PB1", "", "Phantom Book", 10), tombItem("PA1", "PB1", "", 10)}
	src.items[1].Envelope = mustEnclosure(src.items[1], pdf)
	rep := repo.New(d.Pool())
	svc := New(src, mirror.New(rep.Pool()), rep, src.baseURL, "users/0", log.Default())

	if _, err := svc.Run(ctx, nil); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	var attID, docID string
	if err := d.Pool().QueryRow(ctx,
		`SELECT attachment_id::text, document_id::text FROM store_documents WHERE rendition_key='PA1'`).Scan(&attID, &docID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1::uuid, 'sha256:phantom', 'p', 'v', 'ph', $2::uuid, '{}', true)`, attID, docID); err != nil {
		t.Fatal(err)
	}

	// The phantom: Zotero no longer lists the item AT ALL (no delete
	// event — it predates the mirror's cursor history).
	src.version = 11
	src.items = nil
	src.deleteEvents = nil
	res, err := svc.Run(ctx, &SyncOverride{Full: true})
	if err != nil {
		t.Fatalf("full reconcile: %v", err)
	}
	if res.Tombstoned != 1 {
		t.Fatalf("full reconcile must tombstone the phantom, got %d", res.Tombstoned)
	}
	var active bool
	if err := d.Pool().QueryRow(ctx, `SELECT active FROM processing_snapshots WHERE attachment_id=$1::uuid`, attID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("the phantom's snapshot must retire on the full reconcile")
	}
	var mirrorDeleted bool
	if err := d.Pool().QueryRow(ctx, `SELECT deleted FROM zotero_documents WHERE zotero_key='PB1'`).Scan(&mirrorDeleted); err != nil {
		t.Fatal(err)
	}
	if !mirrorDeleted {
		t.Fatal("the phantom's mirror row must be deactivated (markMissing)")
	}
}

// mustEnclosure rewrites an item's envelope to point at a local file.
func mustEnclosure(it zoteroprovider.CanonicalItem, path string) json.RawMessage {
	var env map[string]any
	_ = json.Unmarshal(it.Envelope, &env)
	env["links"] = map[string]any{"enclosure": map[string]any{"href": "file://" + path}}
	b, _ := json.Marshal(env)
	return b
}

// TestReparentUpdatesRenditionIdentityIT — the #358 review witness: a
// rendition whose parent record MOVES (same attachment key, new parent
// document) must carry its new identity everywhere — the mirror's
// parent key, the projection's record key — so the next job for the new
// record resolves instead of dying in REVISION_REF_UNRESOLVED forever.
func TestReparentUpdatesRenditionIdentityIT(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	pdf := makePdf(t, "reparent")

	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zoteroprovider.CanonicalItem{tombItem("RPOLD", "", "Reparent Book", 10), tombItem("RPATT", "RPOLD", "", 10)}
	src.items[1].Envelope = mustEnclosure(src.items[1], pdf)
	rep := repo.New(d.Pool())
	svc := New(src, mirror.New(rep.Pool()), rep, src.baseURL, "users/0", log.Default())

	if _, err := svc.Run(ctx, nil); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	// The move: a NEW parent record carries the SAME rendition key.
	src.version = 11
	src.items = []zoteroprovider.CanonicalItem{tombItem("RPNEW", "", "Reparented Book", 11), tombItem("RPATT", "RPNEW", "", 11)}
	src.items[1].Envelope = mustEnclosure(src.items[1], pdf)
	if _, err := svc.Run(ctx, nil); err != nil {
		t.Fatalf("reparent sync: %v", err)
	}

	// Source-scoped asserts: the persistent test DB carries same-key rows
	// from prior runs — identity checks must not read an arbitrary one.
	var srcID string
	if err := d.Pool().QueryRow(ctx,
		`SELECT id::text FROM zotero_sources WHERE base_url=$1`, src.baseURL).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	// Mirror: the attachment row points at the NEW parent.
	var parentKey string
	if err := d.Pool().QueryRow(ctx,
		`SELECT parent_zotero_key FROM zotero_attachments WHERE zotero_key='RPATT' AND source_id=$1::uuid`, srcID).Scan(&parentKey); err != nil {
		t.Fatal(err)
	}
	if parentKey != "RPNEW" {
		t.Fatalf("mirror parent key = %q, want RPNEW (stale reparent)", parentKey)
	}
	// Store: the projection's record key follows the new parent.
	var recordKey, attID string
	if err := d.Pool().QueryRow(ctx,
		`SELECT record_key, attachment_id::text FROM store_documents WHERE rendition_key='RPATT' AND source_id=$1::uuid`, srcID).Scan(&recordKey, &attID); err != nil {
		t.Fatal(err)
	}
	if recordKey != "RPNEW" {
		t.Fatalf("projection record key = %q, want RPNEW (stale reparent — jobs would be unclaimable)", recordKey)
	}

	// The reparent retargeted the still-active job's revision identity
	// (the arbiter refresh on join) — its claim resolves through the
	// record-key guard. ClaimNextJob is GLOBAL (FIFO over the whole
	// queue; the shared test DB carries pending leftovers from sibling
	// ITs), so drain until THIS rendition's job comes up — an
	// identity-scoped assert, never "any claim succeeded".
	var claimed *repo.ClaimedJob
	for range 50 {
		cj, err := rep.ClaimNextJob(ctx, repo.ClaimOptions{
			WorkerID: "reparent", LeaseDuration: 30 * time.Second,
			Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
		})
		if err != nil {
			t.Fatalf("claim after reparent: %v", err)
		}
		if cj == nil {
			break
		}
		if cj.AttachmentID == attID {
			claimed = cj
			break
		}
	}
	if claimed == nil {
		t.Fatalf("the reparented rendition's job never became claimable (stale job not retargeted? attachment %s)", attID)
	}
}
