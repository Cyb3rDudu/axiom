// A2 #166 acceptance IT: the selective-sync core round-trip against the real
// schema. THE acceptance case is (b): a held document gets its ingest job on
// re-selection WITHOUT any Zotero-side change — the full derivation offers
// every document on every sync; only the selection gate held it back, and the
// ON CONFLICT (attachment_id, content_hash) dedup keeps re-runs clean.
package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

func TestSelectiveSyncAcceptanceIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "hash-a2"
	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.a2", libraryID: "lib-1",
		docKey: "SELD1", attKey: "SELATT1", contentHash: &ch}, "completed", 1)
	var docID, srcID string
	if err := lr.pool.QueryRow(ctx, `SELECT a.document_id::text, a.source_id::text FROM zotero_attachments a
		JOIN zotero_documents d ON d.id=a.document_id WHERE a.id=$1`, attID).Scan(&docID, &srcID); err != nil {
		t.Fatal(err)
	}

	apply := func(selection map[string]string) int {
		t.Helper()
		tx, err := lr.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		res, err := lr.rep.ApplyCanonicalBatch(ctx, tx, srcID, zotero.CanonicalBatch{NewVersion: 2},
			nil, map[string]AttachmentFileInfo{"SELATT1": {Exists: true, Hash: ch}}, selection)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return res.Enqueued
	}

	// The harness seed creates no canonical zotero_items row for the
	// ATTACHMENT — the derivation would see no processable child and
	// deactivate the document (seed like a real Zotero sync would).
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1, 'SELATT1', 1, 'attachment', 'SELD1',
			'{"key":"SELATT1","data":{"path":"storage:x.pdf"}}',
			'{"key":"SELATT1","version":1,"itemType":"attachment","contentType":"application/pdf","filename":"x.pdf","linkMode":"imported_file"}')`,
		srcID); err != nil {
		t.Fatal(err)
	}

	// The harness seed creates a fixture job row; remove it so the baseline
	// is the pre-selection state (document known, never processed).
	if _, err := lr.pool.Exec(ctx, `DELETE FROM ingest_jobs WHERE attachment_id=$1`, attID); err != nil {
		t.Fatal(err)
	}

	// (a) hold: sync with the document excluded -> NO job, nothing enqueued.
	if n := apply(map[string]string{docID: "excluded"}); n != 0 {
		t.Fatalf("held doc must not enqueue: %d", n)
	}
	var jobs int
	if err := lr.pool.QueryRow(ctx, `SELECT count(*) FROM ingest_jobs WHERE attachment_id=$1`, attID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("held doc must have zero job rows, got %d", jobs)
	}

	// (b) THE acceptance case: re-select WITHOUT any zotero_items change —
	// same version, same hash — and the job appears.
	if n := apply(map[string]string{docID: "included"}); n != 1 {
		t.Fatalf("re-selected doc must enqueue exactly one job, got %d", n)
	}

	// (c) hash dedup: repeated syncs never duplicate.
	if n := apply(nil); n != 0 {
		t.Fatalf("repeat sync must enqueue nothing (ON CONFLICT), got %d", n)
	}
	if n := apply(map[string]string{docID: "included"}); n != 0 {
		t.Fatalf("repeat selected sync must enqueue nothing, got %d", n)
	}

	// (d) job completed -> listing shows synced; excluded -> held.
	if _, err := lr.pool.Exec(ctx, `UPDATE ingest_jobs SET status='completed', updated_at=now() WHERE attachment_id=$1`, attID); err != nil {
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
		docs, err := lr.rep.ListZoteroDocuments(ctx, filter)
		if err != nil {
			t.Fatalf("list %q: %v", filter, err)
		}
		for _, d := range docs {
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
	docs, err := lr.rep.ListZoteroDocuments(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
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
	docs, err = lr.rep.ListZoteroDocuments(ctx, "held")
	if err != nil {
		t.Fatal(err)
	}
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
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "hash-atomic"
	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.atomic", libraryID: "lib-1",
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
