// #276: ChunkCaptionsByIDs against the real schema — the batch hydration
// behind the caption observability on /api/search and /api/passage.
package repo

import (
	"context"
	"testing"
)

func TestChunkCaptionsByIDsIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "hash-1"
	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-1",
		docKey: "CAPDOC", attKey: "CAPATT", contentHash: &ch}, "completed", 1)

	ins := `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1, $2, 'p', 'v', 'ph', (SELECT document_id FROM zotero_attachments WHERE id=$1), '{}', true)
		RETURNING id::text`
	var snap string
	if err := lr.pool.QueryRow(ctx, ins, attID, "ch-new").Scan(&snap); err != nil {
		t.Fatal(err)
	}
	insChunk := `INSERT INTO processing_chunks
		(snapshot_id, chunk_index, text, image_refs, image_captions, figure_captions)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb) RETURNING id::text`
	var capChunk, plainChunk string
	if err := lr.pool.QueryRow(ctx, insChunk, snap, 0,
		"Text ![Folie 3](image_0.jpg) mehr ![Folie 4](image_1.jpg)",
		`["image-0000","image-0001"]`,
		`{"image-0000":"erste","image-0001":"zweite"}`,
		`{"image-0001":"Abb. 2"}`).Scan(&capChunk); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, insChunk, snap, 1, "nur Text", `[]`, `{}`, `{}`).Scan(&plainChunk); err != nil {
		t.Fatal(err)
	}

	got, err := lr.rep.ChunkCaptionsByIDs(ctx, []string{capChunk, plainChunk})
	if err != nil {
		t.Fatal(err)
	}
	cc := got[capChunk]
	if len(cc.ImageRefs) != 2 || cc.ImageRefs[0] != "image-0000" || cc.ImageRefs[1] != "image-0001" {
		t.Fatalf("refs wrong: %+v", cc)
	}
	if cc.Machine["image-0000"] != "erste" || cc.Machine["image-0001"] != "zweite" {
		t.Fatalf("machine captions wrong: %+v", cc)
	}
	if cc.Figures["image-0001"] != "Abb. 2" {
		t.Fatalf("figure captions wrong: %+v", cc)
	}
	if plain := got[plainChunk]; len(plain.ImageRefs) != 0 || len(plain.Machine) != 0 || len(plain.Figures) != 0 {
		t.Fatalf("uncaptioned chunk must hydrate empty: %+v", plain)
	}

	// empty batch + unknown ids: empty map, no error
	if m, err := lr.rep.ChunkCaptionsByIDs(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("empty batch must be (empty, nil): %v %v", m, err)
	}
	if m, err := lr.rep.ChunkCaptionsByIDs(ctx, []string{"12345678-1234-4123-8123-123456789012"}); err != nil || len(m) != 0 {
		t.Fatalf("unknown id must be (empty, nil): %v %v", m, err)
	}
	_ = ch
}
