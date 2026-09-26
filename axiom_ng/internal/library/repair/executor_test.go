package repair

// executor_test.go — F08 (#302) acceptance pins for the execution seam:
//
//   - the axiom-repair-worker name works end-to-end through the REAL
//     LocalExecutor (process spawn, args contract, output tail);
//   - the legacy axiom-fixer install falls back, is witnessed, and
//     delegates (compat shim pair exactly as install_dist.sh writes
//     them);
//   - the full Bartscher-style chain (#282 semantics) — claim → worker
//     via the canonical name → apply → post-heal auto-sync → re-enqueue
//     → wave-gate release — against the real DB;
//   - the kill-test: a worker crash mid-repair leaves the case in a
//     RETRYABLE state with no zombie lease.
//
// The DB tier is AXIOM_TEST_DATABASE_URL-gated like the rest of the
// suite; the shim/args tests are pure process tests.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/deprecate"
	axiomsync "github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
	"github.com/jackc/pgx/v5/pgxpool"
)

// writeShims mirrors install_dist.sh's fixer arm in a temp dir: the
// canonical axiom-repair-worker wrapper (execs the worker script — in
// tests a stand-in for fix.sh) and the legacy axiom-fixer compat wrapper
// (warns once, delegates). Returns (canonical, legacy).
func writeShims(t *testing.T, dir, workerScript string) (canonical, legacy string) {
	t.Helper()
	if err := os.WriteFile(workerScript, []byte("#!/bin/sh\n"+`mkdir -p "$HOME/.axiom-test-runs/$1"
printf '%%PDF-healed' > "$HOME/.axiom-test-runs/$1/work.pdf"
echo '{"verdict":"healed"}'`+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonical = filepath.Join(dir, "axiom-repair-worker")
	if err := os.WriteFile(canonical, []byte("#!/bin/sh\nexec "+workerScript+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy = filepath.Join(dir, "axiom-fixer")
	if err := os.WriteFile(legacy, []byte(
		"#!/bin/sh\n# ADR 0001 §4 compat alias: delegates to axiom-repair-worker.\n"+
			`echo "axiom: axiom-fixer is deprecated — use axiom-repair-worker (ADR 0001)" >&2`+"\n"+
			"exec "+canonical+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return canonical, legacy
}

// TestLocalExecutorRunsCanonicalWorker — the acceptance line
// "axiom-repair-worker name works": the v1 adapter spawns the canonical
// wrapper and returns its exit code + output tail.
func TestLocalExecutorRunsCanonicalWorker(t *testing.T) {
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\necho canonical-run; test \"$1\" = KCanonical\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ex := LocalExecutor{Command: worker}
	res, err := ex.Execute(context.Background(), RepairRequest{AttachmentKey: "KCanonical", Budget: time.Minute})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("execute: res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "canonical-run") {
		t.Fatalf("output tail missing: %q", res.Output)
	}
}

// TestLocalExecutorArgs — the worker CLI contract through the seam: the
// request fields arrive as the wrapper's flags (key --apply, epub arm
// with source, OCR lang + force mode).
func TestLocalExecutorArgs(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "args.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"[$*]\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ex := LocalExecutor{Command: script}
	res, err := ex.Execute(context.Background(), RepairRequest{AttachmentKey: "K1", Budget: time.Minute})
	if err != nil || !strings.Contains(res.Output, "[K1 --apply]") {
		t.Fatalf("pdf args: %q err=%v", res.Output, err)
	}
	res, err = ex.Execute(context.Background(), RepairRequest{AttachmentKey: "K2", Format: "epub", SourcePath: "/x.epub", Budget: time.Minute})
	if err != nil || !strings.Contains(res.Output, "[K2 --apply --format epub --source /x.epub]") {
		t.Fatalf("epub args: %q err=%v", res.Output, err)
	}
	res, err = ex.Execute(context.Background(), RepairRequest{AttachmentKey: "K3", Language: "deu", OCRForce: true, Budget: time.Minute})
	if err != nil || !strings.Contains(res.Output, "--lang deu") || !strings.Contains(res.Output, "--ocr-mode force") {
		t.Fatalf("ocr args: %q err=%v", res.Output, err)
	}
}

// TestLocalExecutorResolution — the fallback decision table of
// LocalExecutor.command: explicit non-canonical command wins; canonical
// installed → canonical; only legacy installed → legacy + deprecation
// witness; nothing installed → canonical (the spawn error names it).
func TestLocalExecutorResolution(t *testing.T) {
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker.sh")
	canonical, legacy := writeShims(t, dir, worker)

	deprecate.SetSilent(true)
	t.Cleanup(func() { deprecate.SetSilent(false) })
	before := deprecate.Counts()["axiom-fixer-resolution-test"]

	// explicit configured command wins (the IT fake-script pattern)
	if got := (LocalExecutor{Command: worker}).command(); got != worker {
		t.Fatalf("explicit command must win, got %q", got)
	}

	// canonical installed → canonical, no witness growth
	staged := LocalExecutor{CanonicalPath: canonical, LegacyPath: legacy, TestName: "axiom-fixer-resolution-test"}
	if got := staged.command(); got != canonical {
		t.Fatalf("canonical install must resolve canonical, got %q", got)
	}
	if after := deprecate.Counts()["axiom-fixer-resolution-test"]; after != before {
		t.Fatalf("canonical resolution must not witness: %d → %d", before, after)
	}

	// only legacy installed → legacy + witness
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	if got := staged.command(); got != legacy {
		t.Fatalf("legacy-only install must fall back, got %q", got)
	}
	if after := deprecate.Counts()["axiom-fixer-resolution-test"]; after != before+1 {
		t.Fatalf("legacy fallback must witness exactly once: %d → %d", before, after)
	}

	// nothing installed → the canonical path (loud spawn failure names it)
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	if got := staged.command(); got != canonical {
		t.Fatalf("empty install must keep the canonical default, got %q", got)
	}
}

// TestLocalExecutorLegacyShimWarnsOnceAndDelegates — the acceptance line
// "axiom-fixer warns once and delegates": the compat wrapper install_dist
// writes prints ONE deprecation line, then execs the canonical wrapper;
// the healed artifact proves the delegation ran the worker.
func TestLocalExecutorLegacyShimWarnsOnceAndDelegates(t *testing.T) {
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker.sh")
	_, legacy := writeShims(t, dir, worker)
	home := t.TempDir()
	t.Setenv("HOME", home)

	res, err := LocalExecutor{Command: legacy}.Execute(context.Background(), RepairRequest{AttachmentKey: "KDELEGATE", Budget: time.Minute})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("legacy shim: res=%+v err=%v", res, err)
	}
	if n := strings.Count(res.Output, "deprecated"); n != 1 {
		t.Fatalf("compat wrapper must warn exactly once, warned %d×: %q", n, res.Output)
	}
	pdf := filepath.Join(os.Getenv("HOME"), ".axiom-test-runs", "KDELEGATE", "work.pdf")
	if b, rerr := os.ReadFile(pdf); rerr != nil || string(b) != "%PDF-healed" {
		t.Fatalf("delegated worker did not run (artifact %s: %v)", pdf, rerr)
	}
}

// TestWorkerCommandDefaultsAgree — config's literal default and the
// repair package's canonical constant must not drift (config stays
// import-light; this pin is the weld).
func TestWorkerCommandDefaultsAgree(t *testing.T) {
	const configDefault = "/opt/axiom/bin/axiom-repair-worker"
	if configDefault != CanonicalWorkerCommand {
		t.Fatalf("config default %q != repair.CanonicalWorkerCommand %q", configDefault, CanonicalWorkerCommand)
	}
}

// ── DB-gated chain + kill tests ────────────────────────────────────────────

// TestChainClaimWorkerApplySyncReenqueue — the F08 acceptance chain
// (Bartscher-style, #282 semantics): one case driven through
// claim → worker via the COMPAT ALIAS (the acceptance line "using the
// existing fixer binary via alias" — the wrapper pair install_dist.sh
// writes, with the stand-in worker script behind them) → apply →
// post-heal auto-sync → re-enqueue → wave-gate release. The syncer fake
// inserts the ingest_jobs row the real targeted sync would (the
// wave-gate ITs pin the real sync side); the canonical name's own
// end-to-end proof is TestLocalExecutorRunsCanonicalWorker + the
// resolution table above.
func TestChainClaimWorkerApplySyncReenqueue(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	dir := t.TempDir()
	worker := filepath.Join(dir, "fix.sh")
	_, legacy := writeShims(t, dir, worker)
	home := t.TempDir()
	t.Setenv("HOME", home)

	fa := &fakeApply{store: e.store, qroot: t.TempDir()}
	syncer := &chainSyncer{pool: e.pool}
	inv := New(Config{
		Command:     legacy, // the compat alias delegates to the canonical wrapper
		WorkRoot:    filepath.Join(home, ".axiom-test-runs"),
		Interval:    10 * time.Millisecond,
		Timeout:     time.Minute,
		Concurrency: 1,
	}, Deps{Store: e.store, Apply: fa, QuarantineRoot: fa.qroot, Sync: syncer}, nil)

	caseID := e.seedCase(t, "ATT-CHAIN1")

	// the gate holds while the heal is pending (#282: queued/in_repair)
	if held, reason, err := e.store.WaveRepairGate(context.Background()); err != nil || !held {
		t.Fatalf("gate must hold while queued: %v/%q err=%v", held, reason, err)
	}

	inv.processCase(context.Background(), caseID)

	status, _, attempts := e.caseStatus(t, caseID)
	if status != "healed" || attempts != 1 {
		t.Fatalf("chain: status=%s attempts=%d, want healed/1", status, attempts)
	}
	if syncer.calls != 1 {
		t.Fatalf("post-heal sync calls = %d, want exactly 1", syncer.calls)
	}
	// the post-heal enqueue releases the healed-not-enqueued hold
	if held, reason, err := e.store.WaveRepairGate(context.Background()); err != nil || held {
		t.Fatalf("gate must release after the post-heal enqueue: %v/%q err=%v", held, reason, err)
	}
	var jobs int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ingest_jobs j
		JOIN zotero_attachments a ON a.id = j.attachment_id
		JOIN repair_cases c ON c.attachment_id = a.id
		WHERE c.id=$1 AND j.enqueued_at >= c.updated_at`, caseID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("post-heal re-enqueue: %d job(s) after the heal, want 1", jobs)
	}
}

// chainSyncer fakes the #282 HealSyncer for the chain test: one targeted
// call enqueues the healed document's job (the row shape the real sync
// produces — enqueued_at = now() by column default).
type chainSyncer struct {
	pool  *pgxpool.Pool
	calls int
}

func (c *chainSyncer) Run(ctx context.Context, ov *axiomsync.SyncOverride) (axiomsync.Result, error) {
	c.calls++
	if ov == nil || len(ov.Include) != 1 {
		return axiomsync.Result{}, fmt.Errorf("chain syncer: expected include override, got %+v", ov)
	}
	docID := ov.Include[0]
	tag, err := c.pool.Exec(ctx, `
		INSERT INTO ingest_jobs (status, attachment_id, content_hash)
		SELECT 'pending', a.id, 'healed-chain-hash'
		FROM zotero_attachments a WHERE a.document_id=$1::uuid
		ORDER BY a.preferred DESC LIMIT 1`, docID)
	if err != nil || tag.RowsAffected() != 1 {
		return axiomsync.Result{}, fmt.Errorf("chain syncer enqueue (doc %s): %v rows=%d", docID, err, tag.RowsAffected())
	}
	return axiomsync.Result{Enqueued: 1}, nil
}

// TestWorkerCrashMidRepairLeavesCaseRetryableNoZombieLease — the F08
// kill-test: the worker DIES mid-repair (SIGKILL after claim). The case
// must be retryable: the crash path requeues with the attempt burned,
// the orchestrator-death path (stuck in_repair) is handed back by the
// reaper without a zombie lease, and the loop guard still owns the
// escalation.
func TestWorkerCrashMidRepairLeavesCaseRetryableNoZombieLease(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	dir := t.TempDir()
	crasher := filepath.Join(dir, "crash.sh")
	if err := os.WriteFile(crasher, []byte("#!/bin/sh\nkill -9 $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fa := &fakeApply{store: e.store, qroot: t.TempDir()}
	inv := New(Config{
		Command:     crasher,
		WorkRoot:    dir,
		Interval:    10 * time.Millisecond,
		Timeout:     time.Minute,
		Concurrency: 1,
	}, Deps{Store: e.store, Apply: fa, QuarantineRoot: fa.qroot}, nil)

	caseID := e.seedCase(t, "ATT-CRASH1")

	// crash 1: worker dies mid-run → retry policy requeues (attempt 1)
	inv.processCase(context.Background(), caseID)
	status, _, attempts := e.caseStatus(t, caseID)
	if status != "queued" || attempts != 1 {
		t.Fatalf("after crash: status=%s attempts=%d, want queued/1", status, attempts)
	}

	// crash 2 (the harder one): the ORCHESTRATOR dies after claim — the
	// case sits in in_repair with no writer; the reaper must hand it back
	if _, err := e.store.ClaimRepairCase(context.Background(), caseID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE repair_cases SET updated_at = now() - interval '2 hours' WHERE id=$1`, caseID); err != nil {
		t.Fatal(err)
	}
	inv.reapStale(context.Background())
	status, _, attempts = e.caseStatus(t, caseID)
	if status != "queued" || attempts != 2 {
		t.Fatalf("after reaper: status=%s attempts=%d, want queued/2", status, attempts)
	}
	// no zombie: nothing in in_repair for this case
	var inRepair int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM repair_cases WHERE id=$1 AND status='in_repair'`, caseID).Scan(&inRepair); err != nil {
		t.Fatal(err)
	}
	if inRepair != 0 {
		t.Fatal("zombie lease: case still in_repair after reap")
	}
	// the loop guard still owns the escalation: the exhausted claim parks
	if _, err := e.store.ClaimRepairCase(context.Background(), caseID); err == nil {
		t.Fatal("claim past max attempts must refuse (loop guard owns the escalation)")
	}
	final, freason, _ := e.caseStatus(t, caseID)
	if final != "blocked_for_dudu" || !strings.Contains(freason, "loop-guard") {
		t.Fatalf("post-crash escalation: status=%s reason=%q, want blocked_for_dudu/loop-guard", final, freason)
	}
}
