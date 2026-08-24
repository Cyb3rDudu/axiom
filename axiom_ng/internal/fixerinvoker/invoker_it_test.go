package fixerinvoker

// The integration tier (AXIOM_TEST_DATABASE_URL gated, same proviso as the
// repo/lease suites): drives one attachment event end-to-end through the
// REAL invoker + REAL repo state machine with a fake fixer script and fake
// Zotero-write deps — case created → invoker claims → fix.sh invoked →
// DB status transitions verified, including the timeout and the
// double-invocation case (the DB claim is the lock that holds).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repair"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

type itEnv struct {
	pool *pgxpool.Pool
	rep  *repo.Repo
}

func openDB(t *testing.T) *itEnv {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping fixer invoker integration test")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(d.Close)
	// harness guard: never run against a non-test database
	var cur string
	if err := d.Pool().QueryRow(ctx, `SELECT current_database()`).Scan(&cur); err != nil {
		t.Fatal(err)
	}
	if !containsTest(cur) {
		t.Fatalf("refusing to run against non-test database %q", cur)
	}
	return &itEnv{pool: d.Pool(), rep: repo.New(d.Pool())}
}

func containsTest(name string) bool {
	return strings.Contains(name, "test") || strings.Contains(name, "Test")
}

// seedCase inserts source/document/attachment and returns a QUEUED repair
// case id plus the attachment key.
func (e *itEnv) seedCase(t *testing.T, attKey string) string {
	t.Helper()
	ctx := context.Background()
	var srcID, docID, attID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ($1, $2, 'test-server') RETURNING id::text`,
		"https://zotero.invoker-test", "lib-"+attKey).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title, creators, publication_year)
		VALUES ($1, $2, 1, 'book', 'Invoker Test', '[{"first":"A","last":"Autor"}]', 2024) RETURNING id::text`,
		srcID, "DOC-"+attKey).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	// a REAL source pdf: repair.Quarantine reads it during the custody run
	srcPDF := filepath.Join(t.TempDir(), "src.pdf")
	if err := os.WriteFile(srcPDF, []byte("%PDF-original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path)
		VALUES ($1, $2, $3, 1, $4, 'imported_file', 'application/pdf', 'x.pdf', $5)
		RETURNING id::text`, srcID, docID, attKey, "DOC-"+attKey, srcPDF).Scan(&attID); err != nil {
		t.Fatal(err)
	}
	c, err := e.rep.CreateRepairCase(ctx, attID, "", "reparierbar", []byte(`{}`))
	if err != nil || c == nil {
		t.Fatalf("CreateRepairCase: %v %v", err, c)
	}
	if err := e.rep.QueueRepairCase(ctx, c.ID, "reparierbar", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (e *itEnv) caseStatus(t *testing.T, caseID string) (status, reason string, attempts int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status::text, blocked_reason, attempts FROM repair_cases WHERE id=$1`, caseID).
		Scan(&status, &reason, &attempts); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *itEnv) truncate(t *testing.T) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(), `TRUNCATE repair_cases, zotero_write_audit,
		zotero_attachments, zotero_documents, zotero_sources CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
}

// fakeApply fakes ONLY the Zotero write side (no live Zotero in tests);
// quarantine runs for real against a temp root and the case-state/audit
// methods go to the REAL repo — the DB transitions under test are real.
type fakeApply struct {
	rep *repo.Repo

	mu       sync.Mutex
	calls    []string
	qroot    string
	healedID []byte
}

func (f *fakeApply) Quarantine(root, key, src string) (string, error) {
	f.record("quarantine")
	return repair.Quarantine(root, key, src)
}
func (f *fakeApply) DeleteAttachment(key string) error {
	f.record("delete")
	return nil
}
func (f *fakeApply) CreateAttachmentWithFile(parent, filename string, pdf []byte) (string, error) {
	f.mu.Lock()
	f.healedID = append([]byte(nil), pdf...)
	f.mu.Unlock()
	f.record("create")
	return "NEWKEY1", nil
}
func (f *fakeApply) MarkRepairFailed(ctx context.Context, caseID, reason string) error {
	f.record("failed")
	return f.rep.MarkRepairFailed(ctx, caseID, reason)
}
func (f *fakeApply) MarkRepairHealed(ctx context.Context, caseID string) error {
	f.record("healed")
	return f.rep.MarkRepairHealed(ctx, caseID)
}
func (f *fakeApply) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	f.record("audit:" + action)
	return f.rep.AuditWrite(ctx, caseID, attachmentID, action, detail)
}
func (f *fakeApply) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}
func (f *fakeApply) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// newTestInvoker builds a real Invoker around the DB with a fake fixer
// script + fake apply deps. The fake script writes the healed pdf for the
// success variant.
func newTestInvoker(t *testing.T, e *itEnv, scriptBody string, timeout time.Duration) (*Invoker, *fakeApply) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-fixer.sh")
	out := filepath.Join(t.TempDir(), "workroot")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	fa := &fakeApply{rep: e.rep, qroot: t.TempDir()}
	inv := New(Config{
		Command:     script,
		WorkRoot:    out,
		Interval:    10 * time.Millisecond,
		Timeout:     timeout,
		Concurrency: 1,
	}, Deps{Rep: e.rep, Apply: fa, QuarantineRoot: fa.qroot}, nil)
	return inv, fa
}

// TestInvokerHealsEndToEnd — the happy path: queued case → claim → fake
// fixer (exit 0 + healed pdf) → custody sequence → healed in the DB, one
// attempt burned, audit rows written.
func TestInvokerHealsEndToEnd(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	// success fake: writes WorkRoot/<key>/work.pdf then exits 0
	out := t.TempDir()
	inv, fa := newTestInvoker(t, e, fmt.Sprintf(
		`mkdir -p "%s/$1"; printf '%%s' '%%PDF-healed' > "%s/$1/work.pdf"; exit 0`, out, out), time.Minute)
	inv.cfg.WorkRoot = out
	caseID := e.seedCase(t, "ATT-OK1")
	inv.processCase(context.Background(), caseID)

	status, reason, attempts := e.caseStatus(t, caseID)
	if status != "healed" {
		t.Fatalf("status = %s (reason %q), want healed", status, reason)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	want := []string{"quarantine", "audit:quarantine", "delete", "audit:delete_attachment", "create", "audit:create_attachment", "healed"}
	if got := fa.snapshot(); !equalStrings(got, want) {
		t.Fatalf("custody order: got %v want %v", got, want)
	}
	if string(fa.healedID) != "%PDF-healed" {
		t.Fatalf("healed pdf not uploaded, got %q", fa.healedID)
	}
	var audits int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM zotero_write_audit WHERE case_id=$1`, caseID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 3 {
		t.Fatalf("audit rows = %d, want 3", audits)
	}
}

// TestInvokerFailureRetriesThenParks — the retry/escalation policy: first
// failure requeues (attempt 1), second failure (attempt 2 = max) parks the
// case failed with a clear reason.
func TestInvokerFailureRetriesThenParks(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	inv, _ := newTestInvoker(t, e, "echo kaputt; exit 7\n", time.Minute)
	caseID := e.seedCase(t, "ATT-FAIL1")

	inv.processCase(context.Background(), caseID)
	status, reason, attempts := e.caseStatus(t, caseID)
	if status != "queued" || attempts != 1 {
		t.Fatalf("after 1st failure: status=%s attempts=%d reason=%q, want queued/1", status, attempts, reason)
	}
	if reason == "" {
		t.Fatal("requeued case must carry the failure reason")
	}

	inv.processCase(context.Background(), caseID)
	status, reason, attempts = e.caseStatus(t, caseID)
	if status != "failed" || attempts != 2 {
		t.Fatalf("after 2nd failure: status=%s attempts=%d, want failed/2", status, attempts)
	}
	if reason == "" {
		t.Fatal("terminal failure must carry a reason for dudu")
	}
}

// TestInvokerTimeoutRequeues — a hung fixer is killed by the backstop and
// the case is retried, never lost (owner nail 5).
func TestInvokerTimeoutRequeues(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	inv, _ := newTestInvoker(t, e, "sleep 30\n", 200*time.Millisecond)
	caseID := e.seedCase(t, "ATT-SLOW1")
	start := time.Now()
	inv.processCase(context.Background(), caseID)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("backstop did not kill the hung fixer")
	}
	status, reason, _ := e.caseStatus(t, caseID)
	if status != "queued" {
		t.Fatalf("timeout must requeue, got %s (%q)", status, reason)
	}
	if reason == "" || !contains(reason, "timeout") {
		t.Fatalf("timeout reason missing: %q", reason)
	}
}

// TestInvokerDoubleInvocationSingleClaim — the DB claim is the lock: two
// concurrent invocations of the same case → exactly one runs the custody
// sequence, attempts stays 1.
func TestInvokerDoubleInvocationSingleClaim(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	out := t.TempDir()
	inv, fa := newTestInvoker(t, e, fmt.Sprintf(
		`mkdir -p "%s/$1"; printf '%%s' '%%PDF-healed' > "%s/$1/work.pdf"; exit 0`, out, out), time.Minute)
	inv.cfg.WorkRoot = out
	caseID := e.seedCase(t, "ATT-RACE1")

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv.processCase(context.Background(), caseID)
		}()
	}
	wg.Wait()

	status, _, attempts := e.caseStatus(t, caseID)
	if status != "healed" {
		t.Fatalf("status = %s, want healed (one winner)", status)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1", attempts)
	}
	if n := count(fa.snapshot(), "create"); n != 1 {
		t.Fatalf("create called %d×, want exactly 1", n)
	}
}

// TestInvokerExit0WithoutHealedPdfFails — exit 0 alone is NOT success: the
// healed pdf must exist under WorkRoot/<key>/work.pdf (the masking lesson:
// never trust a green exit without the artifact).
func TestInvokerExit0WithoutHealedPdfFails(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	inv, _ := newTestInvoker(t, e, "exit 0\n", time.Minute)
	caseID := e.seedCase(t, "ATT-EMPTY1")
	inv.processCase(context.Background(), caseID)
	status, reason, _ := e.caseStatus(t, caseID)
	if status != "queued" {
		t.Fatalf("exit-0-without-pdf must fail-or-requeue, got %s (%q)", status, reason)
	}
	if !contains(reason, "keine geheilte pdf") {
		t.Fatalf("reason must name the missing pdf: %q", reason)
	}
}

// TestRequeueStaleRepairCasesRecoversDeadInvoker — lease recovery: an
// in_repair claim older than the stale window goes back to queued; the
// loop guard still caps later attempts.
func TestRequeueStaleRepairCasesRecoversDeadInvoker(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	inv, _ := newTestInvoker(t, e, "exit 0\n", time.Minute)
	caseID := e.seedCase(t, "ATT-STALE1")
	// simulate a dead invoker: claim, then age the claim beyond the window
	if _, err := e.rep.ClaimRepairCase(context.Background(), caseID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE repair_cases SET updated_at = now() - interval '2 hours' WHERE id=$1`, caseID); err != nil {
		t.Fatal(err)
	}
	inv.reapStale(context.Background())
	status, _, attempts := e.caseStatus(t, caseID)
	if status != "queued" || attempts != 1 {
		t.Fatalf("after reap: status=%s attempts=%d, want queued/1", status, attempts)
	}
	// fresh claims are NOT reaped (updated_at recent)
	if _, err := e.rep.ClaimRepairCase(context.Background(), caseID); err != nil {
		t.Fatal(err)
	}
	inv.cfg.StaleAfter = time.Hour
	inv.reapStale(context.Background())
	if status, _, _ := e.caseStatus(t, caseID); status != "in_repair" {
		t.Fatalf("fresh claim must survive the reaper, got %s", status)
	}
}

// TestInvokerAttachmentGoneParksCase — W3a mirror: a case whose attachment
// vanished at the source is parked blocked, not re-served forever.
func TestInvokerAttachmentGoneParksCase(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	inv, _ := newTestInvoker(t, e, "exit 0\n", time.Minute)
	caseID := e.seedCase(t, "ATT-GONE1")
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE zotero_attachments SET deleted = true WHERE zotero_key=$1`, "ATT-GONE1"); err != nil {
		t.Fatal(err)
	}
	inv.processCase(context.Background(), caseID)
	status, reason, _ := e.caseStatus(t, caseID)
	if status != "blocked_for_dudu" || !contains(reason, "attachment-gone") {
		t.Fatalf("attachment-gone must park, got %s (%q)", status, reason)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func count(hay []string, needle string) int {
	n := 0
	for _, s := range hay {
		if s == needle {
			n++
		}
	}
	return n
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

var _ = repair.ApplyDeps(nil) // interface pin

// TestInvokerLoopGuardEscalation — the escalation step of the contract:
// with one attempt pre-spent on the attachment, the first failure requeues
// (case attempts=1 < 2), and the SECOND claim hits the loop guard and
// parks the case blocked_for_dudu — max 2 executions per attachment,
// enforced mechanically, escalation visible to dudu.
func TestInvokerLoopGuardEscalation(t *testing.T) {
	e := openDB(t)
	e.truncate(t)
	inv, _ := newTestInvoker(t, e, "exit 9\n", time.Minute)
	caseID := e.seedCase(t, "ATT-ESC1")
	// pre-spend one attachment attempt (an earlier case on the same attachment)
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE zotero_attachments SET repair_attempts=1 WHERE zotero_key='ATT-ESC1'`); err != nil {
		t.Fatal(err)
	}

	inv.processCase(context.Background(), caseID)
	status, reason, _ := e.caseStatus(t, caseID)
	if status != "queued" || !contains(reason, "fixer exit 9") {
		t.Fatalf("after 1st failure: status=%s reason=%q, want queued/requeued", status, reason)
	}

	inv.processCase(context.Background(), caseID)
	status, reason, _ = e.caseStatus(t, caseID)
	if status != "blocked_for_dudu" || !contains(reason, "loop-guard") {
		t.Fatalf("2nd claim must escalate via loop guard, got %s (%q)", status, reason)
	}
}
