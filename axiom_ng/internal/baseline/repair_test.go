// repair_test.go — repair readback + custody fake-worker probe (#295 Ziel 3).
//
// The repair API is dead in the plain dev env by design (write-key file
// points at nothing → routes unwired). This probe therefore starts a
// SECOND instance of the frozen release RAG (same dev DB, same dev index,
// dispatcher OFF) with:
//
//   - AXIOM_ZOTERO_BASE pointing at a fake Zotero local API the suite
//     runs in-process (records every mutation it sees), and
//   - a generated write-key file, so the repair surface wires up.
//
// Everything the probe touches is dev-owned: a minted fake document +
// attachment row (fixed keys) and a suite-created "healed" PDF — real
// Zotero rows and real storage files are never involved, and the custody
// move quarantine-src never sees a production file. The minted rows are
// removed again; the probe is idempotent across runs.
package baseline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
)

const (
	// fixed, obviously-fake identities: minted rows are cleaned up per run
	glDocKey  = "GLDNBASEDOC1"
	glAttKey  = "GLDNBASEATT1"
	glCaseKey = "GOLDEN_BASELINE_PROBE"
	probePort = 8113
)

// mintCleanupSQL removes every minted row (audit → case → attachments →
// document; child tables first). Shared by mintRepairCase (pre-clean of a
// previous run) and cleanupMint (post-run leave-no-trace).
const mintCleanupSQL = `
		DELETE FROM zotero_write_audit WHERE case_id IN
		  (SELECT id FROM repair_cases WHERE suspicion_class='` + glCaseKey + `');
		DELETE FROM repair_cases WHERE suspicion_class='` + glCaseKey + `';
		DELETE FROM zotero_attachments WHERE zotero_key IN ('` + glAttKey + `','GLDNBASEEPUB1');
		DELETE FROM zotero_documents WHERE zotero_key='` + glDocKey + `';`

// fakeZotero implements exactly the local-API surface the frozen RAG's
// write client and health check touch: ServerID probe, item version GET,
// item DELETE, item create POST, file authorize (answers exists:1 — a
// legitimate protocol path that ends the upload after item creation).
type fakeZotero struct {
	srv *httptest.Server
	mu  sync.Mutex // handler goroutine appends, test goroutine reads

	deleted []string
	created []string
}

func newFakeZotero() *fakeZotero {
	f := &fakeZotero{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Zotero-Server-ID", "fake-zotero-baseline")
		switch {
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/items/"):
			f.mu.Lock()
			f.deleted = append(f.deleted, filepath.Base(r.URL.Path))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items"):
			f.mu.Lock()
			f.created = append(f.created, "item")
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"successful":{"0":{"key":"FAKENEWATT1"}},"unchanged":{},"failed":{}}`)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/file"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"exists":1}`) // staged-identical path: phases 2+3 skipped
		default: // GET item version + ServerID probe
			w.Header().Set("Last-Modified-Version", "42")
			w.WriteHeader(http.StatusOK)
		}
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeZotero) sawDelete(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.deleted, key)
}

func (f *fakeZotero) sawCreate() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created) > 0
}

// startRepairRAG spawns the frozen release RAG with the repair surface
// wired against the fake Zotero. Returns a stop function.
func startRepairRAG(t *testing.T, fakeURL string) (base string, stop func()) {
	t.Helper()
	state := stateDir()
	requireReleaseMode(t)
	// locate the frozen RAG by glob, not by hardcoded filename — a
	// re-freeze (new release generation) must not have to touch this file
	cands, err := filepath.Glob(filepath.Join(state, "release", "axiom-ng-*"))
	if err != nil {
		t.Fatal(err)
	}
	var bins []string
	for _, c := range cands {
		if strings.HasSuffix(c, ".sha256") {
			continue
		}
		if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() {
			bins = append(bins, c)
		}
	}
	if len(bins) != 1 {
		t.Fatalf("expected exactly one release RAG binary under %s/release (axiom-ng-*, got %v) — run scripts/dev/dev-up.sh --release", state, bins)
	}
	bin := bins[0]

	keyFile := filepath.Join(state, "baseline", "write-key")
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("baseline-probe-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(state, "logs", "rag-repair-probe.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`
		set -a
		. %[1]s
		. %[2]s
		set +a
		unset AXIOM_DATABASE_URL
		AXIOM_DATABASE_URL="$(printf '%%s' "$_PROD_DSN" | sed -E 's#/axiom_db([?]|$)#/axiom_dev\1#')"
		[ -n "$AXIOM_DATABASE_URL" ] || exit 3
		# hard assert (same convention as dev-up.sh): a rewrite that silently
		# did not stick would point this second RAG at axiom_db — fail LOUDLY
		# before exec, never serve from prod
		case "$AXIOM_DATABASE_URL" in
		*"/axiom_dev" | *"/axiom_dev"[?]*) ;;
		*"/axiom_db"*)
			echo "repair probe: DATABASE_URL still points at axiom_db — rewrite failed" >&2
			exit 4
			;;
		*)
			echo "repair probe: DATABASE_URL rewrite failed" >&2
			exit 4
			;;
		esac
		# the smuggled prod DSN has done its job — the long-running probe
		# process must not carry it in its environment
		unset _PROD_DSN
		AXIOM_API_PORT=%[3]d
		AXIOM_BIND_ADDR=127.0.0.1
		AXIOM_OS_INDEX=axiom-dev-chunks-v1
		AXIOM_PROCESSOR_URLS=http://127.0.0.1:8112
		AXIOM_PROCESSOR_URL=http://127.0.0.1:8112
		AXIOM_QUERY_RUNNER_URL=http://127.0.0.1:8112
		AXIOM_PROCESSOR_SOURCE_BASE_URL=http://127.0.0.1:%[3]d
		AXIOM_PROCESSOR_RUNNER_NAME=axiom-dev-repair-probe
		AXIOM_ARTIFACT_ROOT="%[4]s"/artifacts
		AXIOM_QUARANTINE_ROOT="%[4]s"/quarantine
		AXIOM_RUNNER_DIR="%[5]s"/axiom_ng_runner
		AXIOM_FIXER_INVOKER_ENABLED=0
		AXIOM_ZOTERO_BASE="%[6]s"
		AXIOM_ZOTERO_WRITE_KEY_FILE="%[7]s"
		AXIOM_DISPATCHER_ENABLED=0
		AXIOM_DISPATCHER_WORKER_ID=axiom-dev-repair-probe
		export AXIOM_DATABASE_URL AXIOM_API_PORT AXIOM_BIND_ADDR AXIOM_OS_INDEX \
			AXIOM_PROCESSOR_URLS AXIOM_PROCESSOR_URL AXIOM_QUERY_RUNNER_URL \
			AXIOM_PROCESSOR_SOURCE_BASE_URL AXIOM_PROCESSOR_RUNNER_NAME \
			AXIOM_ARTIFACT_ROOT AXIOM_QUARANTINE_ROOT AXIOM_RUNNER_DIR \
			AXIOM_FIXER_INVOKER_ENABLED AXIOM_ZOTERO_BASE AXIOM_ZOTERO_WRITE_KEY_FILE \
			AXIOM_DISPATCHER_ENABLED AXIOM_DISPATCHER_WORKER_ID
		exec "%[8]s"
	`, envFile("AXIOM_DEV_RAG_ENV", "/run/agenix/axiom-rag.env"),
		envFile("AXIOM_DEV_RAG_API_ENV", "/run/agenix/axiom-rag-api.env"),
		probePort, state, repoRoot(), fakeURL, keyFile, bin)

	// _PROD_DSN smuggles the env-file DSN through: sourcing happens in the
	// child, the parent never sees secrets. Exit code 4 = DSN guard tripped
	// (see script); 3 = empty rewrite result.
	cmd := exec.Command("/bin/bash", "-c", script)
	cmd.Env = append(os.Environ(), "_PROD_DSN="+prodDSN(t))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start repair RAG: %v", err)
	}
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()

	base = fmt.Sprintf("http://127.0.0.1:%d", probePort)
	stop = func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = logFile.Close()
	}
	// orphan guard: ANY failure below (including t.Fatalf inside this
	// function — the caller's defer stop() never registers) must still take
	// the child down. t.Cleanup runs on every test exit path.
	t.Cleanup(stop)

	// wait for health (the fake answers the zotero check); transport errors
	// while the process is still binding are tolerated and retried
	for i := 0; i < 90; i++ {
		var h struct {
			OK bool `json:"ok"`
		}
		if code := httpMaybeJSON(t, "GET", base+"/api/health", nil, &h, true); code == 200 && h.OK {
			// repair surface must be wired now: the queue endpoint must answer
			var q map[string]any
			if code := httpJSON(t, "GET", base+"/api/repair/queue", nil, &q); code == 200 {
				return base, stop
			}
		}
		select {
		case <-done:
			t.Fatalf("repair RAG died during startup — see %s/logs/rag-repair-probe.log", state)
		case <-time.After(1 * time.Second):
		}
	}
	t.Fatalf("repair RAG never became healthy on :%d — see %s/logs/rag-repair-probe.log", probePort, state)
	return "", nil
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

func envFile(varname, def string) string {
	if v := os.Getenv(varname); v != "" {
		return v
	}
	return def
}

func repoRoot() string {
	// test cwd is axiom_ng/internal/baseline
	r, _ := filepath.Abs("../../..")
	return r
}

// prodDSN reads the rag env file ONLY to extract its DATABASE_URL for the
// dev rewrite — the probe itself talks exclusively to axiom_dev.
func prodDSN(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("/bin/bash", "-c",
		`set -a; . "`+envFile("AXIOM_DEV_RAG_ENV", "/run/agenix/axiom-rag.env")+`"; set +a; printf '%s' "$AXIOM_DATABASE_URL"`).Output()
	if err != nil {
		t.Fatalf("read DSN from rag env: %v", err)
	}
	return string(out)
}

// mintRepairCase creates the dev-owned fake document/attachment/case rows
// (cleaning any previous run first) and returns the case id.
func mintRepairCase(t *testing.T, dsn, srcPDF string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	cleanup := mintCleanupSQL
	if _, err := d.Pool().Exec(ctx, cleanup); err != nil {
		t.Fatalf("cleanup previous mint: %v", err)
	}

	var srcID string
	if err := d.Pool().QueryRow(ctx, `SELECT id FROM zotero_sources LIMIT 1`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	var docID, attID string
	if err := d.Pool().QueryRow(ctx, `
		INSERT INTO zotero_documents
		  (source_id, zotero_key, zotero_version, item_type, title, creators, metadata, tags, collections, deleted, citation_class)
		VALUES ($1, $2, 999, 'report', 'Golden Baseline Custody Probe — dev fixture', '[]', '{}',
		        '[{"tag":"neutral"}]', '[]', false, 'citable')
		RETURNING id`, srcID, glDocKey).Scan(&docID); err != nil {
		t.Fatalf("mint document: %v", err)
	}
	if err := d.Pool().QueryRow(ctx, `
		INSERT INTO zotero_attachments
		  (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode,
		   content_type, filename, local_path, preferred, deleted)
		VALUES ($1, $2, $3, 999, $4, 'imported_file',
		   'application/pdf', 'golden-baseline-probe.pdf', $5, true, false)
		RETURNING id`, srcID, docID, glAttKey, glDocKey, srcPDF).Scan(&attID); err != nil {
		t.Fatalf("mint attachment: %v", err)
	}
	// EPUB sibling — NOT decoration: the frozen repairItemFor scan (see
	// repair_api.go repairItemFor) mis-orders content_type/epub-subquery, so a
	// document WITHOUT an epub sibling is silently dropped from the queue
	// listing (baseline debt, documented in #295). The sibling keeps the
	// queue-item path servable under the freeze bits.
	if _, err := d.Pool().Exec(ctx, `
		INSERT INTO zotero_attachments
		  (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode,
		   content_type, filename, local_path, preferred, deleted)
		VALUES ($1, $2, 'GLDNBASEEPUB1', 999, $3, 'imported_file',
		   'application/epub+zip', 'golden-baseline-probe.epub', $4, false, false)`,
		srcID, docID, glDocKey, filepath.Join(filepath.Dir(srcPDF), "golden-baseline-probe.epub")); err != nil {
		t.Fatalf("mint epub sibling: %v", err)
	}
	var caseID string
	if err := d.Pool().QueryRow(ctx, `
		INSERT INTO repair_cases (attachment_id, document_id, status, suspicion_class)
		VALUES ($1, $2, 'queued', $3) RETURNING id::text`, attID, docID, glCaseKey).Scan(&caseID); err != nil {
		t.Fatalf("mint repair case: %v", err)
	}
	return caseID
}

// cleanupMint removes every minted row and the quarantine record.
func cleanupMint(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Logf("cleanup mint: %v", err)
		return
	}
	defer d.Close()
	if _, err := d.Pool().Exec(ctx, mintCleanupSQL); err != nil {
		t.Logf("cleanup mint: %v", err)
	}
	_ = os.Remove(filepath.Join(stateDir(), "quarantine", "manual", glAttKey+".json"))
	_ = os.Remove(filepath.Join(stateDir(), "baseline", "write-key"))
}

// devDSNDB returns the database name of a URL DSN ("" for non-URLs).
func devDSNDB(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Path == "" {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// TestRequireDevDSNGuard — M1 regression: the repair probe's write path
// refuses every DSN whose database is not axiom_dev, BEFORE any
// connection/write (pure URL parse).
func TestRequireDevDSNGuard(t *testing.T) {
	for _, dsn := range []string{
		"postgresql://u:p@127.0.0.1:5432/axiom_dev?sslmode=disable",
		"postgresql://u:p@127.0.0.1:5432/axiom_dev",
	} {
		if db := devDSNDB(dsn); db != "axiom_dev" {
			t.Errorf("devDSNDB(%q) = %q, want axiom_dev", dsn, db)
		}
	}
	for _, dsn := range []string{
		"postgresql://u:p@127.0.0.1:5432/axiom_db?sslmode=disable", // prod
		"postgresql://u:p@127.0.0.1:5432/axiom_db/",                // trailing slash (rewrite no-op case)
		"postgresql://u:p@127.0.0.1:5432/other_db",
		"not-a-url",
		"",
	} {
		if db := devDSNDB(dsn); db == "axiom_dev" {
			t.Errorf("devDSNDB(%q) accepted as axiom_dev — guard would pass", dsn)
		}
	}
}

// requireDevDSN validates that the DSN points at the dev database BEFORE
// any write reaches it (mintRepairCase inserts, cleanupMint deletes). The
// same name-assert convention as env.sh/dev-up.sh/the probe child script —
// the sanctioned caller (make golden-baseline → env.sh) already guarantees
// it; this guard closes the tool itself, so an out-of-band invocation like
// `AXIOM_DATABASE_URL=<prod> go test -run TestLiveRepair…` fails BEFORE the
// first INSERT, not after (hivemind review M1). Pure URL parse — no
// connection is opened on the failure path.
func requireDevDSN(t *testing.T, dsn string) string {
	t.Helper()
	if db := devDSNDB(dsn); db != "axiom_dev" {
		t.Fatalf("live DSN targets %q — the repair probe writes (mint/cleanup) and may ONLY run against axiom_dev (source scripts/dev/env.sh)", db)
	}
	return dsn
}

// TestLiveRepairAndCustodyGolden — the full fake-worker walk against the
// frozen repair surface: queue readback → claim → verdict → readback →
// requeue, then the manual custody protocol (quarantine → delete →
// create/upload → healed record) with the idempotent refusal.
func TestLiveRepairAndCustodyGolden(t *testing.T) {
	liveEnabled(t)
	assertFreezeBits(t)

	dsn := requireDevDSN(t, fingerprintDSN())
	if dsn == "" {
		t.Fatal("live mode without AXIOM_DATABASE_URL")
	}

	// suite-owned "broken original" — never a real Zotero storage file
	work := t.TempDir()
	srcPDF := filepath.Join(work, "golden-baseline-probe.pdf")
	if err := os.WriteFile(srcPDF, []byte("%PDF-1.4\n% golden baseline custody probe\n%%EOF\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := newFakeZotero()
	defer fake.srv.Close()

	// fresh custody record for a rerun (idempotence guard keys on it)
	_ = os.Remove(filepath.Join(stateDir(), "quarantine", "manual", glAttKey+".json"))

	caseID := mintRepairCase(t, dsn, srcPDF)
	// leave-no-trace: minted rows (incl. a QUEUED case — the #282 wave gate
	// defers every ingest claim while any repair case is queued, and the
	// dev env runs no fixer to drain it) and the custody record must be gone
	// when the probe ends, or the ingest probe (and the dev dispatcher)
	// deadlocks behind this fixture.
	t.Cleanup(func() { cleanupMint(t, dsn) })
	base, _ := startRepairRAG(t, fake.srv.URL) // stop rides on t.Cleanup

	// --- worker surface: queue readback -----------------------------------
	var queue struct {
		Cases []struct {
			SuspicionClass string `json:"suspicion_class"`
			LocalPath      string `json:"local_path"`
			Title          string `json:"title"`
		} `json:"cases"`
	}
	if code := httpJSON(t, "GET", base+"/api/repair/queue", nil, &queue); code != 200 {
		t.Fatalf("queue status %d", code)
	}
	found := false
	for _, c := range queue.Cases {
		if c.SuspicionClass == glCaseKey {
			found = true
			if c.LocalPath != srcPDF {
				t.Errorf("queue local_path = %q, want %q", c.LocalPath, srcPDF)
			}
			if !strings.Contains(c.Title, "Golden Baseline Custody Probe") {
				t.Errorf("queue title = %q", c.Title)
			}
		}
	}
	if !found {
		t.Fatal("minted dev case not served by the repair queue (readback)")
	}

	// --- fake worker: claim → verdict → readback → requeue ----------------
	var claimed map[string]any
	if code := httpJSON(t, "POST", base+"/api/repair/cases/"+caseID+"/claim", nil, &claimed); code != 200 {
		t.Fatalf("claim status %d: %v", code, claimed)
	}
	if s, _ := claimed["status"].(string); s != "in_repair" {
		t.Fatalf("claim: status %q, want in_repair", s)
	}

	var verdict struct {
		Effective string `json:"effective"`
	}
	if code := postForm(t, base+"/api/repair/cases/"+caseID+"/verdict",
		map[string]string{"verdict": "human_review", "score": "0.5", "plan": "{}"}, &verdict); code != 200 {
		t.Fatalf("verdict status %d", code)
	}
	if verdict.Effective != "blocked_for_dudu" {
		t.Fatalf("verdict: effective %q, want blocked_for_dudu (below auto-apply gate)", verdict.Effective)
	}

	var cases struct {
		Cases []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Reason string `json:"blocked_reason"`
		} `json:"cases"`
	}
	if code := httpJSON(t, "GET", base+"/api/repair/cases", nil, &cases); code != 200 {
		t.Fatalf("cases status %d", code)
	}
	var readback string
	for _, c := range cases.Cases {
		if c.ID == caseID {
			readback = c.Status + "|" + c.Reason
		}
	}
	if !strings.HasPrefix(readback, "blocked_for_dudu|") {
		t.Fatalf("cases readback = %q, want blocked_for_dudu with reason", readback)
	}

	if code := httpJSON(t, "POST", base+"/api/repair/cases/"+caseID+"/requeue",
		map[string]any{"reason": "golden baseline probe rerun", "orphan_resolved": ""}, nil); code != 200 {
		t.Fatalf("requeue status %d", code)
	}
	if code := httpJSON(t, "GET", base+"/api/repair/queue", nil, &queue); code != 200 {
		t.Fatalf("queue re-read status %d", code)
	}
	found = false
	for _, c := range queue.Cases {
		if c.SuspicionClass == glCaseKey {
			found = true
		}
	}
	if !found {
		t.Fatal("requeue did not re-serve the case in the queue")
	}

	// --- custody: quarantine → delete → create/upload → healed -------------
	var cust map[string]any
	if code := postMultipart(t, base+"/api/repair/custody", map[string]string{
		"attachment_key": glAttKey,
		"reason":         "golden baseline probe",
	}, "healed_file", "golden-baseline-healed.pdf",
		[]byte("%PDF-1.4\n% healed by golden baseline probe\n%%EOF\n"), &cust); code != 200 {
		t.Fatalf("custody status %d: %v", code, cust)
	}

	if !fake.sawDelete(glAttKey) {
		t.Error("custody: fake Zotero never saw the DELETE of the quarantined original")
	}
	if !fake.sawCreate() {
		t.Error("custody: fake Zotero never saw the healed attachment CREATE")
	}
	if newKey, _ := cust["new_attachment_key"].(string); newKey != "FAKENEWATT1" {
		t.Errorf("custody: new_attachment_key = %q, want the fake's key", newKey)
	}
	rec, _ := cust["record"].(map[string]any)
	if rec == nil {
		t.Fatalf("custody: record missing in response: %v", cust)
	}
	if s, _ := rec["status"].(string); s != "healed" {
		t.Errorf("custody: record.status = %q, want healed", s)
	}

	// frozen custody semantics: the original is COPIED into quarantine
	// (quarantine.go io.Copy — the RAG never unlinks Zotero storage; the
	// Zotero item delete is the owner's business). Assert the copy, not a move.
	if _, err := os.Stat(srcPDF); err != nil {
		t.Errorf("custody: original vanished from source path (freeze behavior is copy, not move): %v", err)
	}
	qPath, _ := rec["quarantine_path"].(string)
	if qPath == "" {
		t.Fatal("custody: record.quarantine_path missing")
	}
	qBytes, err := os.ReadFile(qPath)
	if err != nil {
		t.Fatalf("custody: quarantined original not readable at %s: %v", qPath, err)
	}
	srcBytes, _ := os.ReadFile(srcPDF)
	if !bytes.Equal(qBytes, srcBytes) {
		t.Error("custody: quarantined copy differs from the original")
	}
	manualRec := filepath.Join(stateDir(), "quarantine", "manual", glAttKey+".json")
	raw, err := os.ReadFile(manualRec)
	if err != nil {
		t.Fatalf("custody: manual record file missing: %v", err)
	}
	var mr struct {
		Status           string `json:"status"`
		AttachmentKey    string `json:"attachment_key"`
		QuarantinePath   string `json:"quarantine_path"`
		NewAttachmentKey string `json:"new_attachment_key"`
		Steps            []struct {
			Action string `json:"action"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &mr); err != nil {
		t.Fatal(err)
	}
	stepActions := map[string]bool{}
	for _, s := range mr.Steps {
		stepActions[s.Action] = true
	}
	for _, want := range []string{"quarantine", "delete_attachment", "create_attachment", "healed"} {
		if !stepActions[want] {
			t.Errorf("custody record: step %q missing (have %v)", want, mr.Steps)
		}
	}

	// idempotence: a completed repair refuses a second run (409)
	if code := postMultipart(t, base+"/api/repair/custody", map[string]string{
		"attachment_key": glAttKey,
		"reason":         "golden baseline probe (second run must refuse)",
	}, "healed_file", "x.pdf", []byte("%PDF-1.4\n%%EOF\n"), nil); code != 409 {
		t.Errorf("custody re-run: status %d, want 409 idempotent refusal", code)
	}

	goldenCompare(t, "custody_protocol.json", map[string]any{
		"new_attachment_key": cust["new_attachment_key"],
		"filename":           cust["filename"],
		"record_status":      rec["status"],
		"steps":              stepNames(mr.Steps),
	})
}

func stepNames(steps []struct {
	Action string `json:"action"`
}) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Action)
	}
	return out
}

// postForm sends a multipart/form-data request without a file part
// (thin wrapper over postMultipart — one code path owns the transport).
func postForm(t *testing.T, url string, form map[string]string, out any) int {
	t.Helper()
	return postMultipart(t, url, form, "", "", nil, out)
}

func postMultipart(t *testing.T, url string, fields map[string]string, fileField, filename string, file []byte, out any) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if fileField != "" {
		fw, err := mw.CreateFormFile(fileField, filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw.Bytes(), out); err != nil {
			t.Logf("%s: response is not decodable JSON (status %d, %d bytes): %s",
				url, resp.StatusCode, raw.Len(), raw.Bytes()[:min(raw.Len(), 200)])
		}
	}
	return resp.StatusCode
}
