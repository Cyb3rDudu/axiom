// A1 #165: ChunkLiveness against the real schema (test DB). The OS index only
// carries active-snapshot chunks — the probe distinguishes superseded from
// unknown for the /api/passage 404 hint.
package repo

import (
	"context"
	"testing"
)

func TestChunkLivenessIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "hash-1"
	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-1",
		docKey: "PASSAGEDOC", attKey: "PASSAGEATT", contentHash: &ch}, "completed", 1)

	// two snapshots on the attachment: an inactive (superseded) and the active
	ins := `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1, $2, 'p', 'v', 'ph', (SELECT document_id FROM zotero_attachments WHERE id=$1), '{}', $3)
		RETURNING id::text`
	var snapOld, snapNew string
	if err := lr.pool.QueryRow(ctx, ins, attID, "ch-old", false).Scan(&snapOld); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, ins, attID, "ch-new", true).Scan(&snapNew); err != nil {
		t.Fatal(err)
	}
	insChunk := `INSERT INTO processing_chunks (snapshot_id, chunk_index, text) VALUES ($1, $2, $3) RETURNING id::text`
	var chunkOld, chunkNew string
	if err := lr.pool.QueryRow(ctx, insChunk, snapOld, 0, "alt").Scan(&chunkOld); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, insChunk, snapNew, 0, "neu").Scan(&chunkNew); err != nil {
		t.Fatal(err)
	}

	got, err := lr.rep.ChunkLiveness(ctx, chunkOld)
	if err != nil || got == nil {
		t.Fatalf("inactive chunk must resolve: %v %v", got, err)
	}
	if got.Active || got.SnapshotID != snapOld || got.AttachmentID != attID {
		t.Fatalf("inactive resolution wrong: %+v", got)
	}

	got, err = lr.rep.ChunkLiveness(ctx, chunkNew)
	if err != nil || got == nil || !got.Active || got.SnapshotID != snapNew {
		t.Fatalf("active resolution wrong: %v %v", got, err)
	}

	got, err = lr.rep.ChunkLiveness(ctx, "12345678-1234-4123-8123-123456789012")
	if err != nil || got != nil {
		t.Fatalf("unknown chunk must be (nil, nil): %v %v", got, err)
	}
}
