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
// needed a force-rebuild. A plain sync must re-offer: the failed row
// reopens (attempts remain, content unserved) and exactly one live job
// exists again.
func TestSyncReoffersTerminallyFailedIntake(t *testing.T) {
	w := newReplacementWorld(t, 0)

	// The stall-reaper's terminal write on the queued new-rendition job
	// (verbatim shape from production: attempt 0, infra-flavored error).
	if _, err := w.d.Pool().Exec(context.Background(), `
		UPDATE ingest_jobs SET status='failed', error_code='ingest_stalled',
			error_message='ingest worker stalled', attempt=0
		WHERE revision_source_id = ANY($1::text[]) AND revision_rendition_id='A-NEW'`,
		w.sourceIDs); err != nil {
		t.Fatal(err)
	}

	// The two production no-op syncs: each must re-offer the dead row.
	w.noopSync(t)
	w.noopSync(t)

	// The dead sync-key row stays as history; the sweep mints the heal row.
	// Exactly ONE live job after two no-op syncs (idempotent sweep — the
	// second sync replays the heal key, no second row).
	if live := w.live(t, "A-NEW"); live != 1 {
		jobs := w.jobs(t, "A-NEW")
		t.Fatalf("new rendition after terminal failure + 2 syncs: %d live jobs (total rows %d), want exactly 1 — the sweep must re-offer exactly once", live, len(jobs))
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
