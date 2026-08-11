// Gate-1 contract-completeness integration tests: typed FrozenInput / full
// metadata snapshot, deterministic profile-hash + idempotency key, immediate
// pending cancellation, lock-and-read of dependent rows during claim (so a
// concurrent change is seen, never a mixed-state snapshot), and NULL-FK jobs
// remaining readable through GetJob/ListJobs. Run against the isolated
// axiom_ng_repo_test database; every test truncates only after the harness
// re-verifies current_database() ends in "_test".
package repo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// seedWithCanonical is like seed but additionally creates a canonical
// zotero_items row for the document (with rich raw_data) and links it, so the
// metadata_snapshot is populated from the lossless mirror plus normalized fields.
func (lr *leaseRepo) seedWithCanonical(t *testing.T, spec seedSpec, maxAttempts int) (attachmentID, jobID string) {
	t.Helper()
	ctx := context.Background()
	var srcID, docID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ($1, $2, 'srv-1') RETURNING id::text`, spec.sourceBaseURL, spec.libraryID).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title,
			creators, publication_year, publication_date, publisher, isbn, doi, url, language,
			abstract_note, tags, collections, metadata, deleted)
		VALUES ($1, $2, 7, 'book', 'Full Title',
			'[{"creatorType":"author","firstName":"Ada","lastName":"Lovelace"}]',
			1843, '1843', 'Taylor', '978-0-12345', '10.1000/xyz', 'https://x', 'en',
			'Sample abstract', '[{"tag":"math"}]', '["c1"]',
			'{"edition":"1","volume":"v1","issue":"i2","pages":"1-10","issn":"0000-0000","extra":"x","relations":{}}',
			false) RETURNING id::text`, srcID, spec.docKey).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	var itemID string
	richRaw := `{"key":"` + spec.docKey + `","version":7,"itemType":"book","title":"Full Title","creators":[{"creatorType":"author","firstName":"Ada","lastName":"Lovelace"}],"date":"1843","language":"en","publisher":"Taylor","isbn":"978-0-12345","DOI":"10.1000/xyz","url":"https://x","abstractNote":"Sample abstract","tags":[{"tag":"math"}],"collections":["c1"],"edition":"1","pages":"1-10","ISSN":"0000-0000"}`
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1, $2, 7, 'book', NULL, $3, $4)
		RETURNING id::text`, srcID, spec.docKey,
		`{"key":"`+spec.docKey+`"}`,
		richRaw,
	).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docID, itemID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path,
			content_hash, file_size, mtime_ms, preferred, deleted)
		VALUES ($1, $2, $3, 9, $4, 'imported_file', 'application/pdf',
			'full.pdf', '/zotero/storage/9/p.pdf', $5, 1024, 1786336894000, true, false) RETURNING id::text`,
		srcID, docID, spec.attKey, spec.docKey, spec.contentHash).Scan(&attachmentID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		VALUES ($1, $2, $3, $4, 'pending', $5) RETURNING id::text`,
		srcID, docID, attachmentID, spec.contentHash, maxAttempts).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	return attachmentID, jobID
}

// frozenInput decodes a ClaimedJob's InputSnapshot into FrozenInput.
func frozenInput(t *testing.T, cj *ClaimedJob) FrozenInput {
	t.Helper()
	var fi FrozenInput
	if err := json.Unmarshal(cj.InputSnapshot, &fi); err != nil {
		t.Fatalf("decode frozen input: %v", err)
	}
	return fi
}

// TestFrozenInputIsContractComplete verifies the snapshot mirrors the
// PROCESSOR_CONTRACT.md request structure losslessly: server_id, document and
// attachment zotero keys/versions, full file facts, and the complete
// metadata_snapshot (lossless from raw_data plus normalized fields).
func TestFrozenInputIsContractComplete(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	attID, jobID := lr.seedWithCanonical(t, seedSpec{
		sourceBaseURL: "http://localhost:40", libraryID: "users/0",
		docKey: "UDOC", attKey: "UATT", contentHash: h("sha256:u1"), preferred: true,
	}, 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if cj == nil || cj.JobID != jobID {
		t.Fatalf("expected claim %s, got %v", jobID, cj)
	}
	fi := frozenInput(t, cj)

	if fi.ContractVersion != "1.0" {
		t.Errorf("contract_version = %q, want 1.0", fi.ContractVersion)
	}
	if fi.JobID != jobID {
		t.Errorf("job_id = %q", fi.JobID)
	}
	if fi.Source.Type != "zotero" {
		t.Errorf("source.type = %q", fi.Source.Type)
	}
	if fi.Source.ServerID == nil || *fi.Source.ServerID != "srv-1" {
		t.Errorf("source.server_id = %v, want srv-1", fi.Source.ServerID)
	}
	// Distinct document vs attachment keys: a cross-wired doc/att identity would
	// be caught here.
	if fi.Document.ZoteroKey != "UDOC" || fi.Document.ZoteroVersion != 7 {
		t.Errorf("document zotero identity = %s@%d, want UDOC@7", fi.Document.ZoteroKey, fi.Document.ZoteroVersion)
	}
	if fi.Attachment.AttachmentID != attID {
		t.Errorf("attachment id = %s", fi.Attachment.AttachmentID)
	}
	if fi.Attachment.ZoteroKey != "UATT" || fi.Attachment.ZoteroVersion != 9 {
		t.Errorf("attachment zotero identity = %s@%d, want UATT@9", fi.Attachment.ZoteroKey, fi.Attachment.ZoteroVersion)
	}
	if fi.Attachment.ContentHash == nil || *fi.Attachment.ContentHash != "sha256:u1" {
		t.Errorf("attachment.content_hash = %v", fi.Attachment.ContentHash)
	}
	if fi.Attachment.SizeBytes == nil || *fi.Attachment.SizeBytes != 1024 {
		t.Errorf("attachment.size_bytes = %v", fi.Attachment.SizeBytes)
	}
	if fi.Attachment.MtimeMS == nil || *fi.Attachment.MtimeMS != 1786336894000 {
		t.Errorf("attachment.mtime_ms = %v", fi.Attachment.MtimeMS)
	}

	// The processing block carries the structured profile object (not a
	// stringified string) and the derived hash.
	var prof map[string]any
	if err := json.Unmarshal(fi.Processing.Profile, &prof); err != nil {
		t.Fatalf("processing.profile is not JSON: %v", err)
	}
	if prof["profile"] != "full-rag-v1" {
		t.Errorf("processing.profile.profile = %v, want full-rag-v1", prof["profile"])
	}
	if fi.Processing.ForceRebuild {
		t.Error("processing.force_rebuild should be false")
	}
	if fi.Processing.ProfileHash == "" {
		t.Error("processing.profile_hash is empty")
	}

	// metadata_snapshot is the LOSSESS canonical raw_data: compare semantically
	// against the full expected raw map (creators/tags/ISSN/edition/pages/date all
	// preserved) and assert no normalized-only key leaked in that would indicate a
	// merge implementation.
	var ms map[string]any
	if err := json.Unmarshal(fi.Document.MetadataSnapshot, &ms); err != nil {
		t.Fatalf("decode metadata_snapshot: %v", err)
	}
	want := map[string]any{
		"title":     "Full Title",
		"itemType":  "book",
		"language":  "en",
		"publisher": "Taylor",
		"isbn":      "978-0-12345",
		"DOI":       "10.1000/xyz",
		"date":      "1843",
		"edition":   "1",
		"pages":     "1-10",
		"ISSN":      "0000-0000",
	}
	for k, v := range want {
		if ms[k] != v {
			t.Errorf("metadata_snapshot[%s] = %q, want %q (lossless raw_data)", k, ms[k], v)
		}
	}
	if ms["creators"] == nil {
		t.Error("metadata_snapshot missing creators")
	}
	if ms["tags"] == nil || ms["collections"] == nil {
		t.Error("metadata_snapshot missing tags/collections")
	}
	// The normalized projection columns use different key names (publication_year)
	// and would only appear if the snapshot were merge-built. Their absence proves
	// lossless raw_data was returned as-is.
	if _, present := ms["publication_year"]; present {
		t.Error("metadata_snapshot leaked a normalized-only key; not the lossless raw_data")
	}
}

// TestRequestCancellationImmediatePending verifies a pending job is cancelled in
// the same SQL operation, while a claimed job only records the request.
func TestRequestCancellationImmediatePending(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	_, jPend := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:41", libraryID: "users/0",
		docKey: "V1", attKey: "V1", contentHash: h("sha256:v1"), preferred: true,
	}, "pending", 3)
	if err := lr.rep.RequestCancellation(ctx, jPend); err != nil {
		t.Fatal(err)
	}
	r := lr.rowOf(t, jPend)
	if r.status != "cancelled" {
		t.Fatalf("pending cancel status = %s, want cancelled (immediate)", r.status)
	}
	if r.completedAt == nil {
		t.Fatal("immediate pending cancel must set completed_at")
	}

	_, jClaim := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:42", libraryID: "users/1",
		docKey: "V2", attKey: "V2", contentHash: h("sha256:v2"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if cj == nil || cj.JobID != jClaim {
		t.Fatalf("expected claim %s, got %v", jClaim, cj)
	}
	if err := lr.rep.RequestCancellation(ctx, jClaim); err != nil {
		t.Fatal(err)
	}
	r2 := lr.rowOf(t, jClaim)
	if r2.status != "claimed" {
		t.Fatalf("claimed job after cancel-request status = %s, want claimed", r2.status)
	}
	if r2.cancelRequested == nil {
		t.Fatal("claimed job cancel request was not recorded")
	}
}

// TestNullParentJobsReadable proves that after NULL-parent jobs are terminalized
// to skipped, GetJob and ListJobs still work and expose the nullable FKs as nil
// (no scan error). The REST /api/ingest/jobs null-FK 200 response is covered in
// internal/server/reconcil_test.go.
func TestNullParentJobsReadable(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	j := lr.seedPendingOnly(t, nil, nil, nil, 3)
	_ = lr.claim(t, defaultClaim("worker-a"))
	if r := lr.rowOf(t, j); r.status != "skipped" {
		t.Fatalf("NULL-FK job status = %s, want skipped", r.status)
	}

	got, err := lr.rep.GetJob(ctx, j)
	if err != nil {
		t.Fatalf("GetJob on NULL-FK job: %v", err)
	}
	if got.SourceID != nil || got.DocumentID != nil || got.AttachmentID != nil {
		t.Fatalf("GetJob nullable FKs = %v/%v/%v, want all nil", got.SourceID, got.DocumentID, got.AttachmentID)
	}

	list, err := lr.rep.ListJobs(ctx, 50)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	found := false
	for _, lj := range list {
		if lj.ID == j {
			found = true
			if lj.SourceID != nil || lj.AttachmentID != nil {
				t.Fatalf("ListJobs nullable FKs not nil for %s", j)
			}
		}
	}
	if !found {
		t.Fatalf("ListJobs did not include %s", j)
	}
}

// TestClaimWaitsForConcurrentAttachmentChange proves the claim locks the
// attachment row: an independent transaction mutating the attachment hash while
// the claim is waiting is seen by the claim (the claim blocks then observes the
// new hash and marks the job obsolete), never committing a FrozenInput built
// from a pre-change hash.
func TestClaimWaitsForConcurrentAttachmentChange(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:43", libraryID: "users/0",
		docKey: "W1", attKey: "W1", contentHash: h("sha256:w1"), preferred: true,
	}, "pending", 3)

	conn, err := lr.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM zotero_attachments WHERE zotero_key='W1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	type claimRes struct {
		cj  *ClaimedJob
		err error
	}
	done := make(chan claimRes, 1)
	go func() {
		cj, err := lr.rep.ClaimNextJob(ctx, defaultClaim("worker-b"))
		done <- claimRes{cj: cj, err: err}
	}()

	select {
	case res := <-done:
		t.Fatalf("claim completed while attachment row was locked (should block): err=%v cj=%v", res.err, res.cj)
	case <-time.After(300 * time.Millisecond):
		// still blocked -> good
	}

	if _, err := tx.Exec(ctx, `UPDATE zotero_attachments SET content_hash='sha256:changed' WHERE zotero_key='W1'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("claim after attachment release failed: %v", res.err)
	}
	// force-rebuild false; job content_hash 'sha256:w1' no longer matches the
	// new 'sha256:changed', so the claim must mark it obsolete, never claim.
	if res.cj != nil {
		t.Fatalf("claim succeeded despite concurrent hash change (mixed state committed): %v", res.cj)
	}
	if r := lr.rowOf(t, jobID); r.status != "skipped" {
		t.Fatalf("job status = %s, want skipped (obsolete after concurrent hash change)", r.status)
	}
}

// TestForceRebuildIdempotencyDistinct verifies a force-rebuild job derives an
// idempotency key that cannot collide with a normal job, so a rebuild never
// reuses an old processor result.
// TestForceRebuildIdempotencyDistinct verifies a force-rebuild job derives an
// idempotency key that cannot collide with a normal job, AND that two separate
// force jobs for the SAME attachment/hash/profile still get distinct keys
// (via job_id), so a rebuild never reuses an old processor result.
func TestForceRebuildIdempotencyDistinct(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)

	// One normal (non-force) job on an attachment, plus TWO force jobs for the
	// SAME attachment (allowed: the partial unique idempotency index only applies
	// to force_rebuild=false rows, and there is a single non-force job).
	_, jNorm := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:44", libraryID: "users/0",
		docKey: "X1", attKey: "X1", contentHash: h("sha256:x1"), preferred: true,
	}, "pending", 3) // jNorm is the single non-force job
	jForce1 := lr.seedExtraJob(t, jNorm, true) // first force job, same attachment
	jForce2 := lr.seedExtraJob(t, jNorm, true) // second force job, same attachment

	cjN := lr.claim(t, defaultClaim("worker-a")) // FIFO: jNorm first (non-force)
	if cjN == nil || cjN.JobID != jNorm {
		t.Fatalf("expected normal claim %s, got %v", jNorm, cjN)
	}
	cjF1 := lr.claim(t, defaultClaim("worker-b"))
	if cjF1 == nil || cjF1.JobID != jForce1 {
		t.Fatalf("expected forced claim %s, got %v", jForce1, cjF1)
	}
	cjF2 := lr.claim(t, defaultClaim("worker-c"))
	if cjF2 == nil || cjF2.JobID != jForce2 {
		t.Fatalf("expected forced claim %s, got %v", jForce2, cjF2)
	}

	if cjF1.IdempotencyKey == nil || cjF2.IdempotencyKey == nil || cjN.IdempotencyKey == nil {
		t.Fatal("idempotency keys missing")
	}
	// Force vs normal differ (force marker present/absent).
	if *cjF1.IdempotencyKey == *cjN.IdempotencyKey {
		t.Fatalf("force-rebuild idempotency key collides with normal job: %s", *cjF1.IdempotencyKey)
	}
	if strings.Contains(*cjN.IdempotencyKey, "force-") {
		t.Fatalf("normal job idempotency key must not carry force marker: %s", *cjN.IdempotencyKey)
	}
	// Two FORCE jobs for the SAME attachment/hash/profile MUST differ: job_id is
	// the disambiguator. If job_id were omitted, both would be ...:force-<same>
	// and a rebuild would reuse an old processor result.
	if *cjF1.IdempotencyKey == *cjF2.IdempotencyKey {
		t.Fatalf("two force jobs for same attachment share idempotency key: %s", *cjF2.IdempotencyKey)
	}
	if !strings.Contains(*cjF1.IdempotencyKey, "force-") || !strings.Contains(*cjF2.IdempotencyKey, "force-") {
		t.Fatalf("force-rebuild key lacks force marker: %s / %s", *cjF1.IdempotencyKey, *cjF2.IdempotencyKey)
	}
}
