package sync

// #197: the standing consolidation hook on the Zotero sync completion path.
// Gated like every sync test (Run is DB-bound); the FAKE consolidator pins
// the trigger semantics precisely:
//
//   - a SUCCESSFUL Run schedules exactly one consolidation (fires on
//     completion, after the apply commit);
//   - a burst of Runs collapses into ONE consolidation (debounce);
//   - a FAILED Run never schedules (no consolidation on error);
//   - a consolidator error is logged, never fatal, and the hook stays
//     standing for later syncs;
//   - StopConsolidation cancels a pending debounced run (shutdown path).
//
// The real end-to-end (sync -> real repo merge of seeded duplicates) is
// pinned at the bottom with the actual repo wired as consolidator.
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../axiom_consol_test?sslmode=disable \
//   go test ./internal/sync/ -run TestConsolidationHook -v

import (
	"context"
	"errors"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

type fakeConsolidator struct {
	mu    sync.Mutex
	calls int
	rep   repo.ConsolidationReport
	err   error
}

func (f *fakeConsolidator) ConsolidateEntitiesReport(ctx context.Context) (repo.ConsolidationReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.rep, f.err
}

func (f *fakeConsolidator) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newHookService(t *testing.T, cons Consolidator, debounce time.Duration) *Service {
	t.Helper()
	ctx := context.Background()
	d := openTestDB(t, ctx)
	src := &canonicalFake{serverID: "cons197unit", baseURL: newScriptedBase(), version: 1}
	svc := New(src, repo.New(d.Pool()), src.baseURL, "users/0", log.Default())
	svc.SetConsolidator(cons)
	svc.consolidateDebounce = debounce
	return svc
}

func waitForCalls(t *testing.T, fc *fakeConsolidator, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fc.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("consolidator did not reach %d calls within deadline, got %d", want, fc.count())
}

func quietWait(d time.Duration) { time.Sleep(d) }

func TestConsolidationHookFiresOnSyncCompletion(t *testing.T) {
	fc := &fakeConsolidator{rep: repo.ConsolidationReport{Merged: 3, DuplicateFormsBefore: 3}}
	svc := newHookService(t, fc, 10*time.Millisecond)
	if _, err := svc.Run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	waitForCalls(t, fc, 1)
	// No further spontaneous runs after the debounce settles.
	quietWait(50 * time.Millisecond)
	if n := fc.count(); n != 1 {
		t.Fatalf("one sync must fire exactly one consolidation, got %d", n)
	}
}

func TestConsolidationHookDebouncesSyncBurst(t *testing.T) {
	fc := &fakeConsolidator{}
	svc := newHookService(t, fc, 60*time.Millisecond)
	for i := 0; i < 3; i++ {
		if _, err := svc.Run(context.Background(), nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	// All three completions happened INSIDE the debounce window: exactly
	// one consolidation may fire — after the burst settles.
	waitForCalls(t, fc, 1)
	quietWait(120 * time.Millisecond)
	if n := fc.count(); n != 1 {
		t.Fatalf("a sync burst must collapse into ONE consolidation run, got %d", n)
	}
}

func TestConsolidationHookNoFireOnFailedSync(t *testing.T) {
	fc := &fakeConsolidator{}
	ctx := context.Background()
	d := openTestDB(t, ctx)
	// Empty server id: Run fails BEFORE any apply (source unreachable).
	src := &canonicalFake{baseURL: newScriptedBase(), version: 1}
	svc := New(src, repo.New(d.Pool()), src.baseURL, "users/0", log.Default())
	svc.SetConsolidator(fc)
	svc.consolidateDebounce = 10 * time.Millisecond
	if _, err := svc.Run(ctx, nil); err == nil {
		t.Fatal("run with unreachable source must fail")
	}
	quietWait(80 * time.Millisecond)
	if n := fc.count(); n != 0 {
		t.Fatalf("a failed sync must never fire consolidation, got %d calls", n)
	}
}

func TestConsolidationHookSurvivesConsolidatorError(t *testing.T) {
	fc := &fakeConsolidator{err: errors.New("db weg")}
	svc := newHookService(t, fc, 10*time.Millisecond)
	if _, err := svc.Run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	waitForCalls(t, fc, 1) // fired, errored, service stays alive
	// A subsequent sync still schedules (the hook is standing, not one-shot).
	if _, err := svc.Run(context.Background(), nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	waitForCalls(t, fc, 2)
}

func TestConsolidationHookStopCancelsPendingRun(t *testing.T) {
	fc := &fakeConsolidator{}
	svc := newHookService(t, fc, 80*time.Millisecond)
	if _, err := svc.Run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	svc.StopConsolidation() // shutdown before the debounce elapses
	quietWait(160 * time.Millisecond)
	if n := fc.count(); n != 0 {
		t.Fatalf("StopConsolidation must cancel the pending run, got %d calls", n)
	}
}

// hookSeedSnapshot seeds source -> document -> attachment -> ACTIVE snapshot
// with two same-form entities (the duplicate mass the hook must merge).
// Own base_url per test run (newScriptedBase) — cascade delete cleans up.
func hookSeedSnapshot(t *testing.T, svc *Service, form string) {
	t.Helper()
	ctx := context.Background()
	pool := svc.repo.Pool()
	// The sync test DB is persistent — clear EVERY previous run's chain
	// (documents cascade to attachments -> snapshots -> entities/chunks;
	// the key is test-only, so a cross-source delete is safe).
	if _, err := pool.Exec(ctx, `
		DELETE FROM zotero_documents WHERE zotero_key='DOCHOOK197'`); err != nil {
		t.Fatalf("clean previous seed: %v", err)
	}
	var docID, attID, snapID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, 'DOCHOOK197', 1, 'book', 'Hook Buch') RETURNING id::text`,
		hookSourceID(t, svc)).Scan(&docID); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, preferred)
		VALUES ($1, $2::uuid, 'HOOK197', 1, 'DOCHOOK197', 'linked_file', 'application/pdf', 'hook197.pdf', true)
		RETURNING id::text`, hookSourceID(t, svc), docID).Scan(&attID); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1::uuid, 'hook197hash', 'test', '1', 'p1', $2::uuid, '{}', true)
		RETURNING id::text`, attID, docID).Scan(&snapID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	for _, ref := range []string{"de-a", "de-b"} {
		var entID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form)
			VALUES ($1::uuid, $2, $3, $3) RETURNING id::text`, snapID, ref, form).Scan(&entID); err != nil {
			t.Fatalf("seed entity %s: %v", ref, err)
		}
		var cID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO processing_chunks (snapshot_id, chunk_index, text, token_count)
			VALUES ($1::uuid, (SELECT coalesce(max(chunk_index), -1) + 1 FROM processing_chunks WHERE snapshot_id = $1::uuid), $2, 10)
			RETURNING id::text`, snapID, form+" inhalt").Scan(&cID); err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
			VALUES ($1::uuid, $2::uuid, 0, 1)`, entID, cID); err != nil {
			t.Fatalf("seed mention: %v", err)
		}
	}
}

func hookSourceID(t *testing.T, svc *Service) string {
	t.Helper()
	var id string
	if err := svc.repo.Pool().QueryRow(context.Background(),
		`SELECT id::text FROM zotero_sources WHERE base_url=$1`, svc.baseURL).Scan(&id); err != nil {
		t.Fatalf("source row for %s (run a sync first): %v", svc.baseURL, err)
	}
	return id
}

// The end-to-end proof: sync completes -> the REAL repo consolidator runs
// (debounced) -> the seeded duplicate mass is actually merged in the DB.
func TestConsolidationHookMergesRealDuplicatesIT(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t, ctx)
	src := &canonicalFake{serverID: "cons197it", baseURL: newScriptedBase(), version: 1}
	rep := repo.New(d.Pool())
	svc := New(src, rep, src.baseURL, "users/0", log.Default())
	svc.SetConsolidator(rep) // the REAL consolidation, exactly as main.go wires it
	svc.consolidateDebounce = 20 * time.Millisecond

	// Run 1 creates the source row (the hook fires on an empty graph —
	// a no-op merge, harmless).
	res1, err := svc.Run(ctx, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	// Seed the duplicate mass under the syncer's own source (cascade-clean).
	hookSeedSnapshot(t, svc, "deutschland")
	// Scoped to THIS source: the shared test DB carries other suites'
	// deutschland fixtures, and the hook merges globally (by design).
	countDe := func() int {
		var n int
		if err := d.Pool().QueryRow(ctx, `
			SELECT count(*) FROM processing_entities e
			JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
			JOIN zotero_attachments a ON a.id = s.attachment_id
			WHERE e.canonical_form='deutschland' AND a.source_id=$1`, res1.SourceID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if before := countDe(); before != 2 {
		t.Fatalf("seed must produce 2 duplicate entities under this source, got %d", before)
	}

	// Run 2 completes -> hook fires -> the duplicates merge (debounced).
	// The after-pin is GLOBAL (all active snapshots): consolidation is
	// exact-form global by design, and the deterministic survivor (most
	// chunks) may live under ANOTHER source's snapshot — leftover fixtures
	// from other suites legitimately absorb my pair.
	if _, err := svc.Run(ctx, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var after int
		if err := d.Pool().QueryRow(ctx, `
			SELECT count(*) FROM processing_entities e
			JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
			WHERE e.canonical_form='deutschland'`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after == 1 {
			return // merged by the hook — proof complete
		}
		if time.Now().After(deadline) {
			t.Fatalf("hook did not merge the duplicates within deadline, still %d active deutschland entities", after)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
