package sync

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

var _scopedN int

func timeNowNanos() int64 { return time.Now().UnixNano() }

// scriptedSource returns a caller-provided ListResult and deleted keys, so a
// test can drive full/incremental syncs and inspect scoped reconciliation.
type scriptedSource struct {
	serverID string
	baseURL  string
	results  map[int64]zotero.ListResult // by since
	deleted  map[int64][]zotero.DeleteEvent
	fallback zotero.ListResult
	fallDel  []zotero.DeleteEvent
	fetch    map[string]zotero.Item // parent key -> reconstructed item for FetchParent
}

func (s *scriptedSource) ServerID() string { return s.serverID }
func (s *scriptedSource) ListCollections() ([]zotero.Collection, error) {
	return nil, nil
}
func (s *scriptedSource) ListDeletedKeys(since int64) ([]zotero.DeleteEvent, int64, error) {
	if k, ok := s.deleted[since]; ok {
		return k, since + 1, nil
	}
	if s.fallDel != nil {
		return s.fallDel, since + 1, nil
	}
	return nil, since + 1, nil
}
func (s *scriptedSource) FetchParent(parentKey string) (*zotero.Item, error) {
	if s.fetch == nil {
		return nil, nil
	}
	it, ok := s.fetch[parentKey]
	if !ok {
		return nil, nil
	}
	return &it, nil
}
func (s *scriptedSource) ResolveAttachmentPath(key string) (string, error) {
	return "", nil
}
func (s *scriptedSource) ListPDFItems(since int64) (zotero.ListResult, error) {
	if r, ok := s.results[since]; ok {
		return r, nil
	}
	return s.fallback, nil
}

func pdfItem(key, title, attKey, pdfPath string, ver int64) zotero.Item {
	return zotero.Item{
		Key: key, Version: ver, ItemType: "book", Title: title,
		Attachments: []zotero.Attachment{{
			Key: attKey, Version: ver, ParentKey: key,
			ContentType: "application/pdf", Filename: "a.pdf", LocalPath: pdfPath,
		}},
	}
}

func makePdf(t *testing.T, text string) string {
	p := t.TempDir() + "/" + text + ".pdf"
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func openTestDB(t *testing.T, ctx context.Context) *db.DB {
	dsn := os.Getenv("AXIOMNG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// attFlags loads (deleted, preferred) per attachment key for a doc.
func attFlags(t *testing.T, d *db.DB, ctx context.Context, sourceID, docKey string) map[string][2]bool {
	t.Helper()
	rows, err := d.Pool().Query(ctx, `
		SELECT zotero_key, deleted, preferred FROM zotero_attachments
		WHERE source_id = $1 AND parent_zotero_key = $2`, sourceID, docKey)
	if err != nil {
		t.Fatalf("query atts: %v", err)
	}
	defer rows.Close()
	out := map[string][2]bool{}
	for rows.Next() {
		var k string
		var del, pref bool
		if err := rows.Scan(&k, &del, &pref); err != nil {
			t.Fatal(err)
		}
		out[k] = [2]bool{del, pref}
	}
	return out
}

func docDeleted(t *testing.T, d *db.DB, ctx context.Context, sourceID, docKey string) bool {
	t.Helper()
	var del bool
	if err := d.Pool().QueryRow(ctx,
		`SELECT deleted FROM zotero_documents WHERE source_id=$1 AND zotero_key=$2`,
		sourceID, docKey).Scan(&del); err != nil {
		t.Fatalf("doc flag: %v", err)
	}
	return del
}

func runSyncAt(t *testing.T, src zotero.Source, d *db.DB) string {
	t.Helper()
	r := repo.New(d.Pool())
	base := "http://test/api"
	if ss, ok := src.(*scriptedSource); ok && ss.baseURL != "" {
		base = ss.baseURL
	}
	svc := New(src, r, base, "users/0", log.Default())
	res, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res.SourceID
}

// newScriptedBase returns a unique source base URL so each test is isolated
// from data left behind by earlier runs against the persistent test database.
func newScriptedBase() string {
	_scopedN++
	return fmt.Sprintf("http://test/%d", timeNowNanos()) + strconv.Itoa(_scopedN)
}

// forceCursor sets the stored library version for a source so the next sync
// treats it as incremental from that version.
func forceCursor(t *testing.T, d *db.DB, sourceID string, v int64) error {
	_, err := d.Pool().Exec(context.Background(),
		`UPDATE zotero_sources SET last_modified_version = $2 WHERE id = $1`, sourceID, v)
	return err
}

// TestEmptyDeltaLeavesStateUnchanged: an empty incremental delta must not mark
// any attachment deleted nor change any preferred flag.
func TestEmptyDeltaLeavesStateUnchanged(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)

	pdfA := makePdf(t, "a")
	src := &scriptedSource{serverID: "srv", baseURL: newScriptedBase(), fallback: zotero.ListResult{
		Items:        []zotero.Item{pdfItem("A", "A", "A-PDF", pdfA, 1)},
		AffectedKeys: []string{"A"},
		NewVersion:   10,
	}}
	sourceID := runSyncAt(t, src, d)

	// Empty incremental delta: nothing touched.
	src.fallback = zotero.ListResult{NewVersion: 11}
	if err := forceCursor(t, d, sourceID, 10); err != nil {
		t.Fatal(err)
	}
	runSyncAt(t, src, d)

	flags := attFlags(t, d, ctx, sourceID, "A")
	a := flags["A-PDF"]
	if a[0] {
		t.Errorf("empty delta must not mark attachment deleted")
	}
	if !a[1] {
		t.Errorf("empty delta must leave preferred as-is (want true)")
	}
}

// repoAndPool is no longer used after the lock test moved to independent pools.
// newUUIDShort returns a unique non-empty string for tests that key state by id.
func newUUIDShort() string {
	_scopedN++
	return fmt.Sprintf("t-%d-%d", time.Now().UnixNano(), _scopedN)
}

// runSyncAt uses SourceVersion from DB as since; across tests the DB persists,
// so we set the cursor explicitly for the incremental step.

// TestDeltaForADoesNotTouchB: a delta that only touches document A must leave
// document B's attachments untouched.
func TestDeltaForADoesNotTouchB(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)

	pdfA := makePdf(t, "a")
	pdfB := makePdf(t, "b")
	src := &scriptedSource{serverID: "srv", baseURL: newScriptedBase(), fallback: zotero.ListResult{
		Items:        []zotero.Item{pdfItem("A", "A", "A-PDF", pdfA, 1), pdfItem("B", "B", "B-PDF", pdfB, 1)},
		AffectedKeys: []string{"A", "B"},
		NewVersion:   10,
	}}
	sourceID := runSyncAt(t, src, d)
	if err := forceCursor(t, d, sourceID, 10); err != nil {
		t.Fatal(err)
	}

	// Incremental only on A (its attachment removed in Zotero).
	src.results = map[int64]zotero.ListResult{}
	src.fallback = zotero.ListResult{
		Items:        []zotero.Item{pdfItem("A", "A", "A-PDF", pdfA, 2)}, // A keeps (unchanged)
		AffectedKeys: []string{"A"},
		NewVersion:   11,
	}
	runSyncAt(t, src, d)

	flagsB := attFlags(t, d, ctx, sourceID, "B")
	if b := flagsB["B-PDF"]; b[0] {
		t.Errorf("document B attachment must NOT be deleted by a delta on A")
	}
}

// TestRemovedAttachmentScopedToParent: removing an attachment of B only deletes
// B's attachment, not A's.
func TestRemovedAttachmentScopedToParent(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)

	pdfA := makePdf(t, "a")
	pdfB := makePdf(t, "b")
	src := &scriptedSource{serverID: "srv", baseURL: newScriptedBase(), fallback: zotero.ListResult{
		Items: []zotero.Item{
			pdfItem("A", "A", "A-PDF", pdfA, 1),
			pdfItem("B", "B", "B-PDF", pdfB, 1),
		},
		AffectedKeys: []string{"A", "B"},
		NewVersion:   10,
	}}
	sourceID := runSyncAt(t, src, d)
	if err := forceCursor(t, d, sourceID, 10); err != nil {
		t.Fatal(err)
	}

	// B loses its processable attachment entirely (now only B touched).
	src.fallback = zotero.ListResult{
		Items:        []zotero.Item{pdfItem("A", "A", "A-PDF", pdfA, 2)}, // A unsichtbar? no: A unchanged
		AffectedKeys: []string{"B"},                                      // only B marked affected
		NewVersion:   11,
	}
	runSyncAt(t, src, d)

	flagsA := attFlags(t, d, ctx, sourceID, "A")
	flagsB := attFlags(t, d, ctx, sourceID, "B")
	if a := flagsA["A-PDF"]; a[0] {
		t.Errorf("A's attachment must survive a B-scoped removal")
	}
	if b := flagsB["B-PDF"]; !b[0] {
		t.Errorf("B's attachment must be marked deleted when B has no attachment left")
	}
}

// TestDeletedParentMarksDocAndAttachmentsDeleted.
func TestDeletedParentMarksDocAndAttachmentsDeleted(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)

	pdfA := makePdf(t, "a")
	src := &scriptedSource{serverID: "srv", baseURL: newScriptedBase(), fallback: zotero.ListResult{
		Items:        []zotero.Item{pdfItem("A", "A", "A-PDF", pdfA, 1)},
		AffectedKeys: []string{"A"},
		NewVersion:   10,
	}}
	sourceID := runSyncAt(t, src, d)
	if err := forceCursor(t, d, sourceID, 10); err != nil {
		t.Fatal(err)
	}

	// Zotero reports A deleted.
	src.deleted = map[int64][]zotero.DeleteEvent{10: {{Key: "A", ItemType: "book", ParentKey: ""}}}
	src.fallback = zotero.ListResult{NewVersion: 11}
	runSyncAt(t, src, d)

	if !docDeleted(t, d, ctx, sourceID, "A") {
		t.Errorf("deleted parent's document must be marked deleted")
	}
	flags := attFlags(t, d, ctx, sourceID, "A")
	if a := flags["A-PDF"]; !a[0] {
		t.Errorf("deleted parent's attachments must be marked deleted")
	}
}

// TestPreferredChangeOnANotB: switching the preferred file of A (because only an
// EPUB remains) must not change B's preferred flag.
func TestPreferredChangeOnANotB(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)

	pdfA := makePdf(t, "a")
	pdfB := makePdf(t, "b")
	os.WriteFile(pdfA, []byte("a"), 0o600)
	os.WriteFile(pdfB, []byte("b"), 0o600)
	src := &scriptedSource{serverID: "srv", baseURL: newScriptedBase(), fallback: zotero.ListResult{
		Items: []zotero.Item{
			{Key: "A", Version: 1, ItemType: "book", Title: "A", Attachments: []zotero.Attachment{
				{Key: "A-PDF", Version: 1, ParentKey: "A", ContentType: "application/pdf", Filename: "a.pdf", LocalPath: pdfA},
			}},
			pdfItem("B", "B", "B-PDF", pdfB, 1),
		},
		AffectedKeys: []string{"A", "B"},
		NewVersion:   10,
	}}
	sourceID := runSyncAt(t, src, d)
	if err := forceCursor(t, d, sourceID, 10); err != nil {
		t.Fatal(err)
	}

	// A's PDF is removed; only its EPUB remains, so A's preferred switches to
	// the EPUB. B is untouched.
	epub := t.TempDir() + "/a.epub"
	os.WriteFile(epub, []byte("e"), 0o600)
	src.fallback = zotero.ListResult{
		Items: []zotero.Item{
			{Key: "A", Version: 2, ItemType: "book", Title: "A", Attachments: []zotero.Attachment{
				{Key: "A-EPUB", Version: 2, ParentKey: "A", ContentType: "application/epub+zip", Filename: "a.epub", LocalPath: epub},
			}},
		},
		AffectedKeys: []string{"A"},
		NewVersion:   11,
	}
	runSyncAt(t, src, d)

	// A's X: the prior PDF must be marked deleted and A's current file is the
	// EPUB, which must be preferred.
	flagsA := attFlags(t, d, ctx, sourceID, "A")
	if a := flagsA["A-EPUB"]; a[0] || !a[1] {
		t.Errorf("A.EPUB should be active and preferred (del=%v pref=%v)", a[0], a[1])
	}
	if a := flagsA["A-PDF"]; !a[0] {
		t.Errorf("A.PDF should be marked deleted after preferred switch")
	}
	// B must be entirely unaffected.
	flagsB := attFlags(t, d, ctx, sourceID, "B")
	if b := flagsB["B-PDF"]; b[0] || !b[1] {
		t.Errorf("B.PDF preferred must remain intact when only A changed (del=%v pref=%v)", b[0], b[1])
	}
}

// errDeletedSource returns an error from ListDeletedKeys to simulate a malformed
// trash feed; the sync must abort rather than advance its cursor.
type errDeletedSource struct {
	scriptedSource
}

func (s *errDeletedSource) ListDeletedKeys(since int64) ([]zotero.DeleteEvent, int64, error) {
	return nil, 0, errMalformedTrash
}

var errMalformedTrash = fmt.Errorf("zotero trash decode: malformed JSON")

// TestMalformedTrashAbortsSync: a malformed trash response must abort the sync
// (not silently advance the cursor).
func TestMalformedTrashAbortsSync(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	src := &errDeletedSource{}
	src.serverID = "srv"
	src.baseURL = newScriptedBase()
	src.fallback = zotero.ListResult{NewVersion: 10}

	r := repo.New(d.Pool())
	svc := New(src, r, src.baseURL, "users/0", log.Default())
	if _, err := svc.Run(ctx); err == nil {
		t.Fatal("expected error when trash listing fails; cursor must not advance")
	}
}

// TestVanishedDocumentMarkedDeletedOnFullSync: on a full sync a document that
// no longer appears in Zotero at all is marked deleted with its attachments.
func TestVanishedDocumentMarkedDeletedOnFullSync(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)

	pdfA := makePdf(t, "a")
	src := &scriptedSource{serverID: "srv", baseURL: newScriptedBase(), fallback: zotero.ListResult{
		Items:        []zotero.Item{pdfItem("A", "A", "A-PDF", pdfA, 1)},
		AffectedKeys: []string{"A"},
		NewVersion:   5,
	}}
	sourceID := runSyncAt(t, src, d)

	// Full sync again (cursor reset) but A no longer exists in Zotero.
	if err := forceCursor(t, d, sourceID, 0); err != nil {
		t.Fatal(err)
	}
	src.fallback = zotero.ListResult{NewVersion: 6} // no docs at all
	runSyncAt(t, src, d)

	if !docDeleted(t, d, ctx, sourceID, "A") {
		t.Errorf("vanished document must be marked deleted on full sync")
	}
	flags := attFlags(t, d, ctx, sourceID, "A")
	if a := flags["A-PDF"]; !a[0] {
		t.Errorf("vanished document's attachment must be marked deleted")
	}
}

// TestSourceLockSerializesSyncs verifies the advisory lock serialises syncs:
// while one connection holds the lock, an independent pool's connection cannot
// obtain it; after release no lock persists (a fresh try succeeds from a
// separate session). Uses two independent pools and pg_try_advisory_lock to
// avoid the same-session/reentrant false pass.
func TestSourceLockSerializesSyncs(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("AXIOMNG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOMNG_TEST_DATABASE_URL not set; skipping integration test")
	}
	sourceID := "serialize-test-source-" + newUUIDShort()

	// Two independent pools (separate Postgres sessions).
	poolA, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open poolA: %v", err)
	}
	defer poolA.Close()
	poolB, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open poolB: %v", err)
	}
	defer poolB.Close()

	r1 := repo.New(poolA.Pool())
	release, err := r1.AcquireSourceLock(ctx, sourceID)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	// From pool B (independent session) the lock must be held.
	if b, err := tryAdvisoryLock(t, ctx, poolB, sourceID); err != nil {
		t.Fatalf("try lock B: %v", err)
	} else if b {
		t.Fatal("independent pool acquired the lock while held (not serialising)")
	}

	// Release: pool A's session-level lock must be explicitly dropped.
	release()

	// From pool B a fresh try must now succeed (lock actually released).
	if b, err := tryAdvisoryLock(t, ctx, poolB, sourceID); err != nil {
		t.Fatalf("try lock B after release: %v", err)
	} else if !b {
		t.Fatal("independent pool could not acquire after release; session-level lock leaked")
	}
	// Unlock what we just acquired on B to keep the DB clean.
	if err := unlockLock(t, ctx, poolB, sourceID); err != nil {
		t.Fatalf("cleanup unlock B: %v", err)
	}

	// And pool A (fresh connection) must also be free of the lock.
	if b, err := tryAdvisoryLock(t, ctx, poolA, sourceID); err != nil {
		t.Fatalf("try lock A after release: %v", err)
	} else if !b {
		t.Fatal("poolA session still holds the leaked lock")
	}
	if err := unlockLock(t, ctx, poolA, sourceID); err != nil {
		t.Fatalf("cleanup unlock A: %v", err)
	}
}

func tryAdvisoryLock(t *testing.T, ctx context.Context, d *db.DB, sourceID string) (bool, error) {
	t.Helper()
	conn, err := d.Pool().Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var obtained bool
	err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKeyForTest(sourceID)).Scan(&obtained)
	return obtained, err
}

func unlockLock(t *testing.T, ctx context.Context, d *db.DB, sourceID string) error {
	conn, err := d.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	_, err = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKeyForTest(sourceID))
	return err
}

func lockKeyForTest(sourceID string) int64 {
	var acc int64
	for i := 0; i < len(sourceID); i++ {
		acc = acc*31 + int64(sourceID[i])
	}
	return acc & 0x7FFFFFFFFFFFFFFF
}

// keyPDF makes an item for a doc with `atts` attachments and returns the item.
func itemWithAtts(key string, atts []zotero.Attachment, ver int64) zotero.Item {
	return zotero.Item{Key: key, Version: ver, ItemType: "book", Title: key, Attachments: atts}
}

// TestDeletePreferredAttachmentEnqueuesReplacement: deleting a document's
// preferred PDF leaves the parent otherwise unchanged; the EPUB sibling must
// become preferred and a single new EPUB job must be enqueued.
func TestDeletePreferredAttachmentEnqueuesReplacement(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)

	pdfPath := makePdf(t, "a")
	epubPath := t.TempDir() + "/a.epub"
	os.WriteFile(epubPath, []byte("e"), 0o600)

	prefPDF := zotero.Attachment{Key: "A-PDF", Version: 1, ParentKey: "A", ContentType: "application/pdf", Filename: "a.pdf", LocalPath: pdfPath}
	epub := zotero.Attachment{Key: "A-EPUB", Version: 1, ParentKey: "A", ContentType: "application/epub+zip", Filename: "a.epub", LocalPath: epubPath}

	src := &scriptedSource{serverID: "srv", baseURL: newScriptedBase(), fallback: zotero.ListResult{
		Items:        []zotero.Item{itemWithAtts("A", []zotero.Attachment{prefPDF, epub}, 1)},
		AffectedKeys: []string{"A"},
		NewVersion:   10,
	}}
	sourceID := runSyncAt(t, src, d)
	if err := forceCursor(t, d, sourceID, 10); err != nil {
		t.Fatal(err)
	}

	// Now the preferred PDF is deleted (trash). The parent is otherwise
	// unchanged; FetchParent re-returns it with only the EPUB.
	src.deleted = map[int64][]zotero.DeleteEvent{10: {{Key: "A-PDF", ItemType: "attachment", ParentKey: "A"}}}
	src.fetch = map[string]zotero.Item{"A": itemWithAtts("A", []zotero.Attachment{epub}, 2)}
	src.fallback = zotero.ListResult{NewVersion: 11} // nothing else changed

	runSyncAt(t, src, d)

	flags := attFlags(t, d, ctx, sourceID, "A")
	if a := flags["A-PDF"]; !a[0] {
		t.Errorf("deleted preferred PDF should be marked deleted")
	}
	if a := flags["A-EPUB"]; a[0] || !a[1] {
		t.Errorf("remaining EPUB should be active and preferred (del=%v pref=%v)", a[0], a[1])
	}

	// Exactly one new job for the EPUB replacement.
	jobs, err := repo.New(d.Pool()).ListJobsByAttachment(ctx, attIDByKey(t, d, sourceID, "A-EPUB"))
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected exactly 1 EPUB job, got %d", len(jobs))
	}
	if jobs[0].Status != "pending" {
		t.Errorf("expected pending EPUB job, got %q", jobs[0].Status)
	}
}

// TestDeleteNonPreferredAttachmentNoNewJob: deleting a non-preferred attachment
// must not create a new processing job (the preferred one is unchanged).
func TestDeleteNonPreferredAttachmentNoNewJob(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)

	pdfPath := makePdf(t, "a")
	epubPath := t.TempDir() + "/a.epub"
	os.WriteFile(epubPath, []byte("e"), 0o600)
	prefPDF := zotero.Attachment{Key: "A-PDF", Version: 1, ParentKey: "A", ContentType: "application/pdf", Filename: "a.pdf", LocalPath: pdfPath}
	epub := zotero.Attachment{Key: "A-EPUB", Version: 1, ParentKey: "A", ContentType: "application/epub+zip", Filename: "a.epub", LocalPath: epubPath}

	src := &scriptedSource{serverID: "srv", baseURL: newScriptedBase(), fallback: zotero.ListResult{
		Items:        []zotero.Item{itemWithAtts("A", []zotero.Attachment{prefPDF, epub}, 1)},
		AffectedKeys: []string{"A"},
		NewVersion:   10,
	}}
	sourceID := runSyncAt(t, src, d)
	// Count jobs created by the initial sync.
	before, err := repo.New(d.Pool()).CountJobsForSource(ctx, sourceID)
	if err != nil {
		t.Fatalf("count jobs before: %v", err)
	}
	if err := forceCursor(t, d, sourceID, 10); err != nil {
		t.Fatal(err)
	}

	// Delete the non-preferred EPUB; the PDF remains preferred.
	src.deleted = map[int64][]zotero.DeleteEvent{10: {{Key: "A-EPUB", ItemType: "attachment", ParentKey: "A"}}}
	src.fetch = map[string]zotero.Item{"A": itemWithAtts("A", []zotero.Attachment{prefPDF}, 2)}
	src.fallback = zotero.ListResult{NewVersion: 11}

	runSyncAt(t, src, d)

	after, err := repo.New(d.Pool()).CountJobsForSource(ctx, sourceID)
	if err != nil {
		t.Fatalf("count jobs after: %v", err)
	}
	if after != before {
		t.Errorf("deleting a non-preferred attachment must not create a new job (before=%d after=%d)", before, after)
	}
	flags := attFlags(t, d, ctx, sourceID, "A")
	if a := flags["A-PDF"]; a[0] || !a[1] {
		t.Errorf("preferred PDF must remain active and preferred (del=%v pref=%v)", a[0], a[1])
	}
}

func attIDByKey(t *testing.T, d *db.DB, sourceID, attKey string) string {
	t.Helper()
	var id string
	if err := d.Pool().QueryRow(context.Background(),
		`SELECT id::text FROM zotero_attachments WHERE source_id=$1 AND zotero_key=$2`,
		sourceID, attKey).Scan(&id); err != nil {
		t.Fatalf("attachment id: %v", err)
	}
	return id
}
