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
	"github.com/jackc/pgx/v5/pgxpool"
)

var _scopedN int

func timeNowNanos() int64 { return time.Now().UnixNano() }

// scriptedSource returns a caller-provided ListResult and deleted keys, so a
// test can drive full/incremental syncs and inspect scoped reconciliation.
type scriptedSource struct {
	serverID string
	baseURL  string
	results  map[int64]zotero.ListResult // by since
	deleted  map[int64][]string
	fallback zotero.ListResult
	fallDel  []string
}

func (s *scriptedSource) ServerID() string { return s.serverID }
func (s *scriptedSource) ListCollections() ([]zotero.Collection, error) {
	return nil, nil
}
func (s *scriptedSource) ListDeletedKeys(since int64) ([]string, int64, error) {
	if k, ok := s.deleted[since]; ok {
		return k, since + 1, nil
	}
	return s.fallDel, since + 1, nil
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

// repoAndPool builds a Repo+Pool for direct repo-level assertions in tests.
func repoAndPool(t *testing.T, d *db.DB) (*repo.Repo, *pgxpool.Pool) {
	t.Helper()
	pool := d.Pool()
	return repo.New(pool), pool
}

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
	src.deleted = map[int64][]string{10: {"A"}}
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

func (s *errDeletedSource) ListDeletedKeys(since int64) ([]string, int64, error) {
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
// while one connection holds the lock, another cannot obtain it; after release
// it can. Uses pg_try_advisory_lock to avoid goroutine/pool timing flakiness.
func TestSourceLockSerializesSyncs(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	sourceID := "serialize-test-source-" + newUUIDShort()

	r1, pool := repoAndPool(t, d)
	release, err := r1.AcquireSourceLock(ctx, sourceID)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer release()

	// Attempt to lock from a separate pool connection: must fail (already held).
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	var obtained bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKeyForTest(sourceID)).Scan(&obtained); err != nil {
		conn.Release()
		t.Fatalf("try lock: %v", err)
	}
	conn.Release()
	if obtained {
		t.Fatal("second connection acquired the lock while the first still held it (not serialising)")
	}

	// After releasing, a second try succeeds.
	release()
	conn2, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn2: %v", err)
	}
	defer conn2.Release()
	if err := conn2.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKeyForTest(sourceID)).Scan(&obtained); err != nil {
		t.Fatalf("try lock2: %v", err)
	}
	if !obtained {
		t.Fatal("second connection could not acquire the lock after the first released")
	}
	// Release to leave the DB clean for other tests.
	conn2.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKeyForTest(sourceID))
}

func lockKeyForTest(sourceID string) int64 {
	var acc int64
	for i := 0; i < len(sourceID); i++ {
		acc = acc*31 + int64(sourceID[i])
	}
	return acc & 0x7FFFFFFFFFFFFFFF
}
