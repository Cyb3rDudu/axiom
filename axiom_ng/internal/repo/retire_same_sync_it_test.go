package repo

import (
	"context"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// TestSyncRetiresDeletedAttachmentSameTxIT is the #184 Durchpfad witness for
// the Mullins zombie class: ONE ApplyCanonicalBatch that projects a Zotero
// deletion (full snapshot without the attachment's parent item) must retire
// the attachment's active snapshot IN THE SAME CALL — the original bug was
// ordering (retire ran before the projections wrote deleted=true, so the
// projecting sync retired nothing; heal runs only ever got retired by
// FOLLOW-UP syncs, and the Mullins run had exactly one). Move or remove the
// reconcile step and this goes red.
func TestSyncRetiresDeletedAttachmentSameTxIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "zombiewitness-hash"
	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-zombie",
		docKey: "ZOMBIEDOC", attKey: "ZOMBIEATT", contentHash: &ch}, "completed", 1)

	var snapID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1, $2, 'p', 'v', 'ph', (SELECT document_id FROM zotero_attachments WHERE id=$1), '{}', true)
		RETURNING id::text`, attID, ch).Scan(&snapID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// ONE apply: full snapshot WITHOUT the parent item — the canonical
	// absence IS the Zotero deletion (exactly how the fix-service delete
	// reaches the projections).
	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lr.rep.ApplyCanonicalBatch(ctx, tx, sourceIDFor(t, lr), zotero.CanonicalBatch{
		FullSnapshot: true, NewVersion: 2,
	}, nil, map[string]AttachmentFileInfo{}, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var active bool
	if err := lr.pool.QueryRow(ctx, `SELECT active FROM processing_snapshots WHERE id=$1`, snapID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("SAME-SYNC-RETIRE verletzt: Snapshot des gelöschten Anhangs ist noch aktiv (Mullins-Zombie-Klasse)")
	}
	var ops []string
	rows, err := lr.pool.Query(ctx, `SELECT operation FROM opensearch_outbox WHERE snapshot_id=$1 ORDER BY created_at, id`, snapID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var op string
		if err := rows.Scan(&op); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	rows.Close()
	if len(ops) == 0 || ops[len(ops)-1] != OutboxOpDelete {
		t.Fatalf("retire muss OS-Tombstone planen, got %v", ops)
	}
}

// sourceIDFor returns the seeded source id of the ONE source in the fixture
// (seed() created exactly this one).
func sourceIDFor(t *testing.T, lr *leaseRepo) string {
	t.Helper()
	var id string
	if err := lr.pool.QueryRow(context.Background(),
		`SELECT id::text FROM zotero_sources LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
