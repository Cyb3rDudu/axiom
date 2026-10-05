// A2 #166 acceptance IT: the selective-sync core round-trip against the real
// schema. THE acceptance case is (b): a held document gets its ingest job on
// re-selection WITHOUT any Zotero-side change — the full derivation offers
// every document on every sync; only the selection gate held it back, and the
// ON CONFLICT (attachment_id, content_hash) dedup keeps re-runs clean.
package mirror

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSelectiveSyncAcceptanceIT(t *testing.T) {
	lr := openMirrorDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "hash-a2"
	attID, jobID := lr.seed(t, mirrorSeedSpec{sourceBaseURL: "https://zoteroprovider.a2", libraryID: "lib-1",
		docKey: "SELD1", attKey: "SELATT1", contentHash: &ch, preferred: true}, "completed", 1)
	var docID, srcID string
	if err := lr.pool.QueryRow(ctx, `SELECT a.document_id::text, a.source_id::text FROM zotero_attachments a
		JOIN zotero_documents d ON d.id=a.document_id WHERE a.id=$1`, attID).Scan(&docID, &srcID); err != nil {
		t.Fatal(err)
	}

	listing := func(filter string) []ZoteroDocumentState {
		t.Helper()
		rows, err := lr.rep.ListDocumentsMirror(ctx)
		if err != nil {
			t.Fatalf("mirror listing %q: %v", filter, err)
		}
		var attIDs, docIDs []string
		for _, z := range rows {
			if z.AttachmentID != "" {
				attIDs = append(attIDs, z.AttachmentID)
			}
			docIDs = append(docIDs, z.DocumentID)
		}
		jobs, serving, err := lr.store.DocumentJobStates(ctx, attIDs, docIDs)
		if err != nil {
			t.Fatalf("store job states: %v", err)
		}
		return DocumentListing(rows, jobs, serving, filter)
	}

	// The harness seed created a fixture job row (completed baseline);
	// pin its FKs as the claim would so the listing's job lookup resolves.
	if _, err := lr.pool.Exec(ctx, `UPDATE ingest_jobs SET status='completed', attachment_id=$2::uuid,
		document_id=(SELECT document_id FROM store_documents WHERE attachment_id=$2::uuid),
		source_id=(SELECT source_id FROM store_documents WHERE attachment_id=$2::uuid),
		updated_at=now() WHERE id=$1`, jobID, attID); err != nil {
		t.Fatal(err)
	}

	// A document WITHOUT any attachment row must not break the listing
	// (NULL scan) — it lists as held with attachment=nil.
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, 'SELD2', 1, 'book', 'No Attachment Doc')`, srcID); err != nil {
		t.Fatal(err)
	}

	stateOf := func(filter, docID string) (string, *AttachmentState) {
		t.Helper()
		for _, d := range listing(filter) {
			if d.DocumentID == docID {
				return d.SyncState, d.Attachment
			}
		}
		return "", nil
	}

	state, att := stateOf("", docID)
	if state != "synced" || att == nil {
		t.Fatalf("completed job must list synced with attachment, got %q %+v", state, att)
	}
	// locate the no-attachment doc by its key via the full listing
	docs := listing("")
	var noAttID string
	for _, d := range docs {
		if d.ZoteroKey == "SELD2" {
			noAttID = d.DocumentID
			if d.SyncState != "held" || d.Attachment != nil {
				t.Fatalf("no-attachment doc must list held with nil attachment, got %q %+v", d.SyncState, d.Attachment)
			}
		}
	}
	if noAttID == "" {
		t.Fatal("no-attachment doc missing from listing")
	}

	// processing/pending are driven through the newest-job mapping too.
	if _, err := lr.pool.Exec(ctx, `UPDATE ingest_jobs SET status='claimed', updated_at=now() WHERE attachment_id=$1`, attID); err != nil {
		t.Fatal(err)
	}
	if state, _ = stateOf("processing", docID); state != "processing" {
		t.Fatalf("claimed job must list processing, got %q", state)
	}
	if _, err := lr.pool.Exec(ctx, `UPDATE ingest_jobs SET status='pending', updated_at=now() WHERE attachment_id=$1`, attID); err != nil {
		t.Fatal(err)
	}
	if state, _ = stateOf("pending", docID); state != "pending" {
		t.Fatalf("pending job must list pending, got %q", state)
	}
	if _, err := lr.pool.Exec(ctx, `UPDATE ingest_jobs SET status='completed', updated_at=now() WHERE attachment_id=$1`, attID); err != nil {
		t.Fatal(err)
	}

	if err := lr.rep.SetSelections(ctx, []SelectionInput{{DocumentID: docID, Mode: "excluded"}}); err != nil {
		t.Fatal(err)
	}
	docs = listing("held")
	found := false
	for _, d := range docs {
		if d.DocumentID == docID {
			found = true
			if d.Attachment == nil || d.Attachment.ZoteroKey != "SELATT1" {
				t.Fatalf("attachment info missing: %+v", d.Attachment)
			}
		}
	}
	if !found {
		t.Fatal("excluded doc must appear under sync_state=held")
	}

	// Selection persistence round-trip: repo semantics + reset to default.
	modes, err := lr.rep.SelectionModes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if modes[docID] != "excluded" {
		t.Fatalf("selection map wrong: %v", modes)
	}
	if err := lr.rep.SetSelections(ctx, []SelectionInput{{DocumentID: docID, Mode: "default"}}); err != nil {
		t.Fatal(err)
	}
	modes, _ = lr.rep.SelectionModes(ctx)
	if _, ok := modes[docID]; ok {
		t.Fatal("default mode must remove the row")
	}
}

// EffectiveSelection unit pins: nil paths, override wins over persisted.
func TestEffectiveSelection(t *testing.T) {
	if EffectiveSelection(nil, nil, nil) != nil {
		t.Error("no selection, no override = no gate")
	}
	m := EffectiveSelection(map[string]string{"d1": "excluded"}, []string{"d1"}, []string{"d2"})
	if m["d1"] != "included" || m["d2"] != "excluded" {
		t.Fatalf("override must win: %v", m)
	}
	if !JobGated(m, "d2") || JobGated(m, "d1") {
		t.Fatal("gate decision wrong")
	}
	if JobGated(nil, "any") {
		t.Fatal("nil selection never gates")
	}
}

// Hivemind Fix-Auftrag 1 (#166): SetSelectionBatch is ALL-OR-NOTHING. A
// bogus document id (FK 23503) mid-batch must roll back the whole write —
// a half-applied selection would silently flip the other layer's sync
// semantics. The valid-batch control first proves the empty end state is
// not vacuous.
func TestSetSelectionBatchAtomicityIT(t *testing.T) {
	lr := openMirrorDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "hash-atomic"
	attID, _ := lr.seed(t, mirrorSeedSpec{sourceBaseURL: "https://zoteroprovider.atomic", libraryID: "lib-1",
		docKey: "ATOM1", attKey: "ATOMATT1", contentHash: &ch}, "completed", 1)
	var docID string
	if err := lr.pool.QueryRow(ctx, `SELECT document_id::text FROM zotero_attachments WHERE id=$1`, attID).Scan(&docID); err != nil {
		t.Fatal(err)
	}

	// Control: a valid batch persists BOTH layers.
	if err := lr.rep.SetSelectionBatch(ctx,
		[]SelectionInput{{DocumentID: docID, Mode: "included"}},
		[]CollectionSelectionInput{{CollectionKey: "ATOMIC01", Mode: "included"}}); err != nil {
		t.Fatalf("valid batch: %v", err)
	}
	if m, _ := lr.rep.SelectionModes(ctx); m[docID] != "included" {
		t.Fatalf("control: document row must persist, got %v", m)
	}
	if m, _ := lr.rep.CollectionSelectionModes(ctx); m["ATOMIC01"] != "included" {
		t.Fatalf("control: collection row must persist, got %v", m)
	}

	// Reset both to default, then the probe: valid doc entry + valid
	// collection entry + BOGUS document id in ONE batch.
	if err := lr.rep.SetSelectionBatch(ctx,
		[]SelectionInput{{DocumentID: docID, Mode: "default"}},
		[]CollectionSelectionInput{{CollectionKey: "ATOMIC01", Mode: "default"}}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	err := lr.rep.SetSelectionBatch(ctx,
		[]SelectionInput{
			{DocumentID: docID, Mode: "included"},
			{DocumentID: "00000000-0000-4000-8000-00000000dead", Mode: "included"},
		},
		[]CollectionSelectionInput{{CollectionKey: "ATOMIC01", Mode: "included"}})
	if err == nil {
		t.Fatal("bogus document id must fail the batch")
	}
	var fk *pgconn.PgError
	if !errors.As(err, &fk) || fk.Code != "23503" {
		t.Fatalf("want FK 23503, got %v", err)
	}
	if m, _ := lr.rep.SelectionModes(ctx); len(m) != 0 {
		t.Fatalf("document rows must NOT persist after the failed batch: %v", m)
	}
	if m, _ := lr.rep.CollectionSelectionModes(ctx); len(m) != 0 {
		t.Fatalf("collection rows must NOT persist after the failed batch: %v", m)
	}
}
