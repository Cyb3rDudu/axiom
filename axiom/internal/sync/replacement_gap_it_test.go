// replacement_gap_it_test.go — #365: a mid-flow file replacement (old
// attachment deleted, new preferred attachment with a new content hash)
// must enqueue EXACTLY ONE job on the new rendition, retire the deleted
// rendition's queued jobs (zombie impossible), and — the production gap —
// a terminally failed intake job for a live, unserved preferred rendition
// must be re-offered by a plain sync (the 2026-10-08 case: the new
// rendition's job was externally terminalized while merely queued behind
// the wave; every later sync replayed the dead intake key and the document
// needed a force-rebuild).
package sync

import (
	"context"
	"os"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom/internal/zoteroprovider"
)

// replacementWorld is one test world: scripted Zotero source + both DBs.
type replacementWorld struct {
	d         *db.DB
	src       *canonicalFake
	newAttID  string
	oldAttID  string
	sourceIDs []string
}

// newReplacementWorld drives import → replace-file (the production
// chronology: delete event + new attachment in one delta, new hash).
func newReplacementWorld(t *testing.T, noopSyncs int) *replacementWorld {
	t.Helper()
	ctx := context.Background()
	d := openTestDB(t, ctx)
	oldPdf := makePdf(t, "old-body")
	newPdf := makePdf(t, "new-body-ocr") // different bytes → different hash

	src := &canonicalFake{serverID: "srv", baseURL: newScriptedBase(), version: 10}
	src.items = []zoteroprovider.CanonicalItem{bookEnv("B1", "Book", nil), pdfAttEnv("A-OLD", "B1", oldPdf)}
	res1, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("import sync: %v", err)
	}
	w := &replacementWorld{d: d, src: src, oldAttID: attIDByKey(t, d, res1.SourceID, "A-OLD")}
	w.sourceIDs = append(w.sourceIDs, res1.SourceID)

	src.version = 11
	src.deleteEvents = []zoteroprovider.DeleteEvent{{Key: "A-OLD", ItemType: "attachment", ParentKey: "B1"}}
	src.items = []zoteroprovider.CanonicalItem{pdfAttEnv("A-NEW", "B1", newPdf)}
	res2, err := runCanon(t, src, d)
	if err != nil {
		t.Fatalf("replacement sync: %v", err)
	}
	w.newAttID = attIDByKey(t, d, res2.SourceID, "A-NEW")
	w.sourceIDs = append(w.sourceIDs, res2.SourceID)

	for i := 0; i < noopSyncs; i++ {
		w.noopSync(t)
	}
	return w
}

// noopSync drives one no-change delta (the wave's v6734/v6763 shape).
func (w *replacementWorld) noopSync(t *testing.T) {
	t.Helper()
	w.src.version++
	w.src.items = nil
	w.src.deleteEvents = []zoteroprovider.DeleteEvent{}
	res, err := runCanon(t, w.src, w.d)
	if err != nil {
		t.Fatalf("no-op sync: %v", err)
	}
	w.sourceIDs = append(w.sourceIDs, res.SourceID)
}

// jobs returns ALL jobs for a rendition key (the revision-intake identity —
// FKs stay NULL until the first claim, which is exactly why production
// misread the new rendition as "zero jobs ever").
func (w *replacementWorld) jobs(t *testing.T, renditionKey string) []repo.Job {
	t.Helper()
	rows, err := w.d.Pool().Query(context.Background(),
		`SELECT id::text, status::text, COALESCE(error_code,'') FROM ingest_jobs
		 WHERE revision_source_id = ANY($1::text[]) AND revision_rendition_id = $2
		 ORDER BY enqueued_at`, w.sourceIDs, renditionKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []repo.Job
	for rows.Next() {
		var j repo.Job
		var ec string
		if err := rows.Scan(&j.ID, &j.Status, &ec); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

// live counts the non-terminal jobs for a rendition key.
func (w *replacementWorld) live(t *testing.T, renditionKey string) int {
	t.Helper()
	n := 0
	for _, j := range w.jobs(t, renditionKey) {
		switch j.Status {
		case "failed", "cancelled", "obsolete", "skipped", "completed":
		default:
			n++
		}
	}
	return n
}

func (w *replacementWorld) debugDump(t *testing.T) {
	if os.Getenv("RG_DEBUG") == "" {
		return
	}
	for _, key := range []string{"A-OLD", "A-NEW"} {
		for _, j := range w.jobs(t, key) {
			t.Logf("JOB %s rendition=%s status=%s", j.ID[:8], key, j.Status)
		}
	}
}

// TestReplacementEnqueuesNewRenditionExactlyOnce: the full production
// sequence — import, replace the file between syncs, two no-op syncs —
// ends with exactly ONE live job on the new attachment; the deleted
// rendition's queued import job was retired by the replacement sync (the
// zombie class), and no new job was minted for it.
func TestReplacementEnqueuesNewRenditionExactlyOnce(t *testing.T) {
	w := newReplacementWorld(t, 2)
	w.debugDump(t)
	if got := w.live(t, "A-NEW"); got != 1 {
		t.Fatalf("new rendition: %d live jobs, want exactly 1", got)
	}
	old := w.jobs(t, "A-OLD")
	if len(old) != 1 {
		t.Fatalf("deleted rendition: %d jobs total, want exactly the 1 import job (no re-mint)", len(old))
	}
	if old[0].Status != "skipped" {
		t.Fatalf("deleted rendition's queued job status = %q, want skipped (retired at replacement, zombie impossible)", old[0].Status)
	}
}

// TestSyncReoffersTerminallyFailedIntake — THE production gap: the new
// rendition's job is terminally failed while merely queued (the external
// stall-reaper shape: attempt 0, no claim ever happened). Both subsequent
// syncs replayed the dead intake key and enqueued nothing; the document
// needed a force-rebuild. A plain sync must re-offer, and — the pinned
// facet — the SAME row must reopen (row count stable across repeated
// kill/heal cycles; the lifetime attempt budget, not a reset, bounds the
// cycle: after max terminal failures at climbing attempts the row stays
// dead and the sweep stops offering).
func TestSyncReoffersTerminallyFailedIntake(t *testing.T) {
	w := newReplacementWorld(t, 0)
	kill := func(attempt int) {
		t.Helper()
		if _, err := w.d.Pool().Exec(context.Background(), `
			UPDATE ingest_jobs SET status='failed', error_code='ingest_stalled',
				error_message='ingest worker stalled', attempt=$2
			WHERE revision_source_id = ANY($1::text[]) AND revision_rendition_id='A-NEW'`,
			w.sourceIDs, attempt); err != nil {
			t.Fatal(err)
		}
	}

	// Cycle 1: the reaper kills the queued sync-key job at attempt 0 (the
	// verbatim production shape). The sweep mints the heal row.
	kill(0)
	w.noopSync(t)
	if live := w.live(t, "A-NEW"); live != 1 {
		t.Fatalf("cycle 1: %d live jobs, want 1 (sweep must re-offer)", live)
	}

	// Cycles 2..n: the same failure strikes the HEAL row — the replay must
	// reopen THAT row (no second mint), and the row count stays stable.
	for cycle, attempt := range []int{0, 1, 2} {
		kill(attempt)
		rowsBefore := len(w.jobs(t, "A-NEW"))
		w.noopSync(t)
		jobs := w.jobs(t, "A-NEW")
		if len(jobs) != rowsBefore {
			t.Fatalf("cycle %d: row count %d -> %d — replay must reopen the SAME row, not mint", cycle+2, rowsBefore, len(jobs))
		}
		if attempt >= 3 {
			continue // ceiling shape handled by the exhausted test
		}
		if live := w.live(t, "A-NEW"); live != 1 {
			t.Fatalf("cycle %d (attempt=%d): %d live jobs, want 1 — the failed heal row must reopen", cycle+2, attempt, live)
		}
	}

	// The lifetime ceiling: at attempt >= max the row keeps its verdict and
	// the sweep stops offering (no infinite retry for failing content).
	kill(3)
	w.noopSync(t)
	if live := w.live(t, "A-NEW"); live != 0 {
		t.Fatalf("exhausted row reopened (%d live) — the attempt ceiling must end the cycle", live)
	}
}

// TestExhaustedIntakeStaysDead — the bounded counterpart: a row at its
// attempt ceiling keeps its verdict (force-rebuild stays the escape for
// genuinely poison content; syncs must not thrash it forever).
func TestExhaustedIntakeStaysDead(t *testing.T) {
	w := newReplacementWorld(t, 0)
	if _, err := w.d.Pool().Exec(context.Background(), `
		UPDATE ingest_jobs SET status='failed', error_code='RETRY_EXHAUSTED',
			error_message='attempt limit reached', attempt=3
		WHERE revision_source_id = ANY($1::text[]) AND revision_rendition_id='A-NEW'`,
		w.sourceIDs); err != nil {
		t.Fatal(err)
	}
	w.noopSync(t)
	if got := w.live(t, "A-NEW"); got != 0 {
		t.Fatalf("exhausted row reopened by sync (%d live) — must stay dead", got)
	}
}
