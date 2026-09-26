// intake_persist_it_test.go — the F09 #303 acceptance, durable half:
// ingest of a source revision → claim (FK resolve + revision freeze) →
// PersistResult → ONE active snapshot with its chunks and an OpenSearch
// outbox row — the #264-era atomic commit carried over the revision lane
// UNCHANGED (same single transaction, same fenced MarkCompletedTx).
package repo

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

// revisionResult builds a minimal VALID processor result for a
// revision-claimed job (the persist-harness fixture shape: one chunk with
// a dense embedding, one entity, one artifact — stats matching).
func revisionResult(jobID, attachmentID, contentHash, profileHash string, dims int) *processor.Result {
	denseVals := make([]float32, dims)
	for i := range denseVals {
		denseVals[i] = 0.1 * float32(i+1)
	}
	return &processor.Result{
		ContractVersion: "1.0",
		JobID:           jobID,
		Status:          "completed",
		Source:          processor.ResultSource{AttachmentID: attachmentID, ContentHash: contentHash, Verified: true},
		Processor: processor.ResultProcessor{
			Name: "revision-it-runner", Version: "0.1.0",
			Profile: "full-rag-v1", ProfileHash: profileHash,
			Models: map[string]string{"dense_embedding": "reference-bge-m3"},
		},
		Artifacts: []processor.Artifact{{
			Ref: "markdown", Kind: "markdown", MediaType: "text/markdown; charset=utf-8",
			SHA256: "d23412f5", SizeBytes: 100, Retention: "durable",
		}},
		Manifest: map[string]any{"source_page_count": 1},
		Chunks: []processor.Chunk{{
			Ref: "chunk-0000", Index: 0, Text: "revision intake persist proof",
			Locator:    &processor.Locator{Type: "page_span", PhysicalPageStart: ptrInt(0), PhysicalPageEnd: ptrInt(0), PageLabelStart: "1", PageLabelEnd: "1", Source: "marker_paginate", PageSource: "pdf_label_sane"},
			Structure:  processor.ChunkStructure{SectionTitles: []string{"Intro"}, StartParagraphIndex: ptrInt(0), EndParagraphIndex: ptrInt(0)},
			TokenCount: 4,
			Embeddings: processor.ChunkEmbeddings{Dense: &processor.DenseEmbedding{Model: "reference-bge-m3", Dimensions: dims, Values: denseVals}},
		}},
		Stats: processor.Stats{Chunks: 1, Entities: 0, Artifacts: 1, EntityRelationships: 0, ChunkRelationships: 0},
	}
}

// TestRevisionIntakePersistsSnapshotChunksOutboxIT — the acceptance chain.
func TestRevisionIntakePersistsSnapshotChunksOutboxIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixturesPlus(t)
	ctx := context.Background()

	hash := revision.HashContent([]byte("revision persist bytes"))
	var srcID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://revpersist.local','users/0','srv-1') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	var docID, itemID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1,'DOCREV1',1,'book','Revision Persist') RETURNING id::text`, srcID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1,'DOCREV1',1,'book',NULL,'{}','{}') RETURNING id::text`, srcID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docID, itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
		VALUES ($1,$2,'ATTREV1',1,'DOCREV1','imported_file','application/pdf','r.pdf','/tmp/r.pdf',$3,true,false)`,
		srcID, docID, hash); err != nil {
		t.Fatal(err)
	}

	// 1. Mint via the revision intake.
	rev := revision.SourceRevision{
		SourceID: srcID, RevisionID: "1", RenditionID: "ATTREV1",
		ContentHash: hash, MediaType: revision.MediaTypePDF,
		Bibliography:  revision.Bibliography{RecordID: "DOCREV1", Title: "Revision Persist", CitationClass: "citable"},
		ContentTicket: "zat:" + srcID + ":ATTREV1",
	}
	revJSON, _ := json.Marshal(rev)
	job, minted, err := lr.rep.EnqueueRevisionIntake(ctx, IntakeRequest{
		IdempotencyKey: "revpersist-1", RevisionSourceID: srcID, RevisionRecordID: "DOCREV1",
		RevisionRenditionID: "ATTREV1", RevisionNo: "1", ContentHash: hash, RevisionJSON: revJSON,
	})
	if err != nil || !minted {
		t.Fatalf("mint: %v %v", job, err)
	}

	// 2. Claim — the revision lane resolves the FKs and freezes.
	claimed, err := lr.rep.ClaimNextJob(ctx, ClaimOptions{
		WorkerID: "revpersist", LeaseDuration: 30 * time.Second,
		Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
	})
	if err != nil || claimed == nil || claimed.JobID != job.ID {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := lr.rep.MarkProcessing(ctx, claimed.LeaseRef); err != nil {
		t.Fatal(err)
	}

	// 3. Persist the verified compute result — the atomic commit.
	resBytes, _ := json.Marshal(revisionResult(claimed.JobID, claimed.AttachmentID, hash, derefPStr(claimed.ProfileHash), 3))
	snapID, err := lr.rep.PersistResult(ctx, claimed.JobID, resBytes, PersistOptions{
		CapDim: 3,
		Artifacts: []ArtifactRecord{{
			Ref: "markdown", Kind: "markdown", MediaType: "text/markdown; charset=utf-8",
			SHA256: "d23412f5", SizeBytes: 100, Retention: "durable", StoragePath: "/tmp/md.md",
		}},
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}

	// 4. The acceptance asserts: ONE active snapshot, its chunk, the OS
	// outbox row — and the job completed (all inside the one commit).
	var active int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_snapshots WHERE document_id=$1 AND active`, claimed.DocumentID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active snapshots = %d, want 1", active)
	}
	var chunks int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM processing_chunks WHERE snapshot_id=$1`, snapID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != 1 {
		t.Fatalf("chunks = %d, want 1", chunks)
	}
	var ops []string
	rows, err := lr.pool.Query(ctx, `SELECT operation FROM opensearch_outbox WHERE snapshot_id=$1`, snapID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var op string
		_ = rows.Scan(&op)
		ops = append(ops, op)
	}
	rows.Close()
	if len(ops) == 0 || ops[len(ops)-1] != "index" {
		t.Fatalf("outbox must plan an index op for the new snapshot, got %v", ops)
	}
	var status string
	if err := lr.pool.QueryRow(ctx, `SELECT status::text FROM ingest_jobs WHERE id=$1`, claimed.JobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("job status %q, want completed", status)
	}

	// 5. The REVISION promise (review F2.4): a second revision of the same
	// rendition (new content) atomically retires the old snapshot — old
	// active=false, new active=true, tombstone for the old in the SAME
	// commit.
	hash2 := revision.HashContent([]byte("revision persist bytes v2"))
	if _, err := lr.pool.Exec(ctx,
		`UPDATE zotero_attachments SET content_hash=$1 WHERE zotero_key='ATTREV1'`, hash2); err != nil {
		t.Fatal(err)
	}
	rev2 := revision.SourceRevision{
		SourceID: srcID, RevisionID: "2", RenditionID: "ATTREV1",
		ContentHash: hash2, MediaType: revision.MediaTypePDF,
		Bibliography:  revision.Bibliography{RecordID: "DOCREV1", Title: "Revision Persist v2", CitationClass: "citable"},
		ContentTicket: "zat:" + srcID + ":ATTREV1",
	}
	rev2JSON, _ := json.Marshal(rev2)
	job2, minted2, err := lr.rep.EnqueueRevisionIntake(ctx, IntakeRequest{
		IdempotencyKey: "revpersist-2", RevisionSourceID: srcID, RevisionRecordID: "DOCREV1",
		RevisionRenditionID: "ATTREV1", RevisionNo: "2", ContentHash: hash2, RevisionJSON: rev2JSON,
	})
	if err != nil || !minted2 {
		t.Fatalf("mint v2: %v %v", job2, err)
	}
	claimed2, err := lr.rep.ClaimNextJob(ctx, ClaimOptions{
		WorkerID: "revpersist2", LeaseDuration: 30 * time.Second,
		Profile: json.RawMessage(`{"profile":"full-rag-v1"}`),
	})
	if err != nil || claimed2 == nil || claimed2.JobID != job2.ID {
		t.Fatalf("claim v2: %v %v", claimed2, err)
	}
	if err := lr.rep.MarkProcessing(ctx, claimed2.LeaseRef); err != nil {
		t.Fatal(err)
	}
	res2Bytes, _ := json.Marshal(revisionResult(claimed2.JobID, claimed2.AttachmentID, hash2, derefPStr(claimed2.ProfileHash), 3))
	snap2ID, err := lr.rep.PersistResult(ctx, claimed2.JobID, res2Bytes, PersistOptions{
		CapDim: 3,
		Artifacts: []ArtifactRecord{{
			Ref: "markdown", Kind: "markdown", MediaType: "text/markdown; charset=utf-8",
			SHA256: "d23412f5", SizeBytes: 100, Retention: "durable", StoragePath: "/tmp/md2.md",
		}},
	})
	if err != nil {
		t.Fatalf("persist v2: %v", err)
	}
	var oldActive, newActive bool
	if err := lr.pool.QueryRow(ctx, `SELECT active FROM processing_snapshots WHERE id=$1`, snapID).Scan(&oldActive); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `SELECT active FROM processing_snapshots WHERE id=$1`, snap2ID).Scan(&newActive); err != nil {
		t.Fatal(err)
	}
	if oldActive || !newActive {
		t.Fatalf("revision replacement: old snapshot active=%v new active=%v — want false/true (one commit)", oldActive, newActive)
	}
	var ops2 []string
	rows2, err := lr.pool.Query(ctx, `SELECT operation FROM opensearch_outbox WHERE snapshot_id=$1`, snapID)
	if err != nil {
		t.Fatal(err)
	}
	for rows2.Next() {
		var op string
		_ = rows2.Scan(&op)
		ops2 = append(ops2, op)
	}
	rows2.Close()
	if len(ops2) < 2 || ops2[len(ops2)-1] != "delete" {
		t.Fatalf("replacement must tombstone the old snapshot (delete op last), got %v", ops2)
	}
}

func derefPStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
