// e2e_it_test.go — the F09 #303 dev E2E + INDEPENDENCE RUN.
//
// Gate: AXIOM_F09_E2E=1 AND AXIOM_TEST_DATABASE_URL (a *_test DSN) AND
// AXIOM_F09_RUNNER_URL (a live processor) AND AXIOM_F09_OS_URL (a live
// OpenSearch). What it proves, against a WORKTREE-BUILT binary:
//
//  1. E2E over the new seam: POST /api/v1/store/ingest (a SourceRevision
//     whose content is a real PDF) → dispatcher claim → runner compute →
//     atomic snapshot+chunks commit → outbox drain → SEARCHABLE
//     (/api/v1/search finds the document, /api/v1/passage resolves).
//  2. Independence: the process is `axiom serve store` — NO sync role,
//     NO Library providers, and the health JSON carries NO zotero field
//     (the store slice never even probes Zotero). The Library is
//     completely stopped; the only Library-side artifact the store
//     touches is the shared database's mirror rows (the documented
//     dual-read).
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type e2eEnv struct {
	dsn     string // scratch DB DSN (owned; dropped in cleanup)
	dbName  string
	baseDSN string
	runner  string
	osURL   string
	osIndex string
	bin     string
	work    string
	apiPort int
	apiURL  string
	pdfPath string
	pdfHash string
	srcID   string
	docUUID string
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestF09StoreIndependenceE2E(t *testing.T) {
	if os.Getenv("AXIOM_F09_E2E") != "1" {
		t.Skip("AXIOM_F09_E2E != 1; skipping the F09 dev E2E")
	}
	baseDSN := os.Getenv("AXIOM_TEST_DATABASE_URL")
	runner := os.Getenv("AXIOM_F09_RUNNER_URL")
	osURL := os.Getenv("AXIOM_F09_OS_URL")
	if baseDSN == "" || runner == "" || osURL == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL / AXIOM_F09_RUNNER_URL / AXIOM_F09_OS_URL incomplete; skipping")
	}
	if !strings.Contains(strings.Split(baseDSN, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-_test database %q", baseDSN)
	}
	ctx := contextBg()

	// Scratch DB (own name, dropped at cleanup).
	dbName := fmt.Sprintf("axiom_f09_e2e_%d_test", os.Getpid())
	e := &e2eEnv{baseDSN: baseDSN, dbName: dbName, runner: runner, osURL: osURL,
		osIndex: fmt.Sprintf("f09-e2e-chunks-%d", os.Getpid())}
	e.dsn = swapPath(baseDSN, dbName)
	admin, err := pgxpool.New(ctx, swapPath(baseDSN, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, dbName); err != nil {
		t.Fatal(err)
	}
	admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	admin.Close()
	t.Cleanup(func() {
		ctx2 := contextBg()
		p, err := pgxpool.New(ctx2, swapPath(baseDSN, "postgres"))
		if err == nil {
			p.Exec(ctx2, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, dbName)
			p.Exec(ctx2, `DROP DATABASE IF EXISTS `+dbName)
			p.Close()
		}
		httpDelete(t, osURL+"/"+e.osIndex)
	})

	// A real EPUB (the runner's own test corpus — EPUB always carries a
	// Tier-1 text layer, so the preflight quality gate passes; the
	// runner-served PDFs of the corpus are scan-class and would exercise
	// the repair track instead).
	src, err := filepath.Abs("../../internal/backfill/testdata/book.epub")
	if err != nil {
		t.Fatal(err)
	}
	pdfBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("pdf fixture: %v", err)
	}
	hash := hashHexOf(pdfBytes)
	e.pdfPath, e.pdfHash = src, hash

	// Build the worktree binary.
	e.work = t.TempDir()
	e.bin = filepath.Join(e.work, "axiom")
	build := exec.Command("go", "build", "-o", e.bin, "../../cmd/axiom")
	build.Env = append(os.Environ(), "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build axiom: %v\n%s", err, out)
	}

	// `axiom serve store` — the independence topology.
	e.apiPort = freePort(t)
	e.apiURL = fmt.Sprintf("http://127.0.0.1:%d", e.apiPort)
	proc := exec.Command(e.bin, "serve", "store")
	// Scrubbed environment: the operator shell may carry production
	// agenix values (source secrets, OS index) — only what this run
	// declares reaches the process.
	env := []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"AXIOM_DATABASE_URL=" + e.dsn,
		fmt.Sprintf("AXIOM_API_PORT=%d", e.apiPort),
		"AXIOM_BIND_ADDR=127.0.0.1",
		"AXIOM_OS_URL=" + osURL,
		"AXIOM_OS_INDEX=" + e.osIndex,
		"AXIOM_PROCESSOR_URLS=" + runner,
		"AXIOM_PROCESSOR_URL=" + runner,
		"AXIOM_QUERY_RUNNER_URL=" + runner,
		"AXIOM_PROCESSOR_SOURCE_BASE_URL=" + e.apiURL,
		"AXIOM_ARTIFACT_ROOT=" + filepath.Join(e.work, "artifacts"),
		"AXIOM_DISPATCHER_ENABLED=1",
		"AXIOM_DISPATCHER_WORKER_ID=f09-e2e",
		"AXIOM_FIXER_INVOKER_ENABLED=0",
		"AXIOM_LIBRARY_IMPORT_PROVIDERS=",
		// Remote delivery: the runner's allowed-roots boundary rejects
		// arbitrary local paths — the signed source URL over THIS api is
		// the sanctioned delivery path (and exercises the revision job's
		// ProcessorSource serving end to end).
		"AXIOM_PROCESSOR_SOURCE_BASE_URL=" + e.apiURL,
		"AXIOM_PROCESSOR_SOURCE_SECRET=f09-e2e-secret",
		"TMPDIR=" + os.Getenv("TMPDIR"),
	}
	proc.Env = env
	logs := &bytes.Buffer{}
	proc.Stdout, proc.Stderr = logs, logs
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		proc.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { proc.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			proc.Process.Kill()
		}
		t.Logf("serve store logs:\n%s", logs.String())
	})

	// Wait for readiness (the server migrates core + both component
	// ledgers on start — the store slice owns that here).
	waitFor(t, 60*time.Second, "store api up", func() error {
		resp, err := http.Get(e.apiURL + "/api/health")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var health struct {
			OK     bool           `json:"ok"`
			Checks map[string]any `json:"checks"`
		}
		if err := json.Unmarshal(body, &health); err != nil {
			return err
		}
		if !health.OK {
			return fmt.Errorf("health not ok: %s", body)
		}
		// INDEPENDENCE witness: a store-slice process has NO zotero check
		// (the Library-side dependency does not exist here) and no library
		// providers are wired.
		if _, has := health.Checks["zotero"]; has {
			return fmt.Errorf("store slice must not carry a zotero health check: %s", body)
		}
		if _, has := health.Checks["postgres"]; !has {
			return fmt.Errorf("store slice must have postgres: %s", body)
		}
		return nil
	})

	// Seed the mirror rows the revision resolves to (shared DB, no
	// Library process — the documented dual-read) and intake.
	pool, err := pgxpool.New(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	e.srcID, e.docUUID = seedE2EMirror(t, pool, hash, src)

	// 1. Intake: the revision with the PDF's identity.
	rev := map[string]any{
		"source_id": e.srcID, "revision_id": "1", "rendition_id": "ATTE2E1",
		"content_hash": hash, "media_type": "application/epub+zip",
		"content_ticket": "zat:" + e.srcID + ":ATTE2E1",
		"bibliography": map[string]any{
			"record_id": "DOCE2E1", "title": "Buchkapitel (F09 E2E)",
			"citation_class": "citable",
		},
	}
	resp := postJSON(t, e.apiURL+"/api/v1/store/ingest", map[string]any{
		"idempotency_key": "f09-e2e-1", "revision": rev,
	})
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("intake status %d: %s", resp.StatusCode, body)
	}

	// Job-state observability between intake and search: fail FAST with
	// the DB's own diagnosis instead of burning the 8-minute poll.
	waitFor(t, 8*time.Minute, "ingested revision becomes searchable", func() error {
		var status, code, msg string
		_ = pool.QueryRow(contextBg(),
			`SELECT status::text, COALESCE(error_code,''), COALESCE(error_message,'') FROM ingest_jobs WHERE intake_kind='revision' LIMIT 1`).
			Scan(&status, &code, &msg)
		if status == "failed" || status == "skipped" {
			return fmt.Errorf("revision job terminal: %s %s %s", status, code, msg)
		}
		resp, err := http.Post(e.apiURL+"/api/v1/search", "application/json",
			strings.NewReader(`{"query":"chapter inhalt","top_n":5,"filters":{"document_ids":["`+e.docUUID+`"]}}`))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return fmt.Errorf("search status %d: %s", resp.StatusCode, body)
		}
		var res struct {
			Hits []struct {
				ChunkID string `json:"chunk_id"`
				Source  struct {
					Title string `json:"title"`
				} `json:"source"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return err
		}
		if len(res.Hits) == 0 {
			return fmt.Errorf("no hits yet")
		}
		// 3. The passage primitive resolves over the revision-intaked doc.
		presp, err := http.Get(e.apiURL + "/api/v1/passage/" + res.Hits[0].ChunkID)
		if err != nil {
			return err
		}
		defer presp.Body.Close()
		pbody, _ := io.ReadAll(presp.Body)
		if presp.StatusCode != 200 {
			return fmt.Errorf("passage status %d: %s", presp.StatusCode, pbody)
		}
		var passage struct {
			ChunkID string `json:"chunk_id"`
			Source  struct {
				Title string `json:"title"`
			} `json:"source"`
		}
		if err := json.Unmarshal(pbody, &passage); err != nil {
			return err
		}
		if passage.ChunkID == "" || !strings.Contains(passage.Source.Title, "F09") {
			return fmt.Errorf("passage hydration incomplete: %s", pbody)
		}
		return nil
	})
}

// seedE2EMirror seeds the mirror rows the revision resolves to (the
// documented dual-read: shared-database rows, no Library process).
func seedE2EMirror(t *testing.T, pool *pgxpool.Pool, hash, pdfPath string) (srcID, docUUID string) {
	t.Helper()
	ctx := contextBg()
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://f09-e2e.local','users/0','srv-e2e') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1,'DOCE2E1',1,'book','Buchkapitel (F09 E2E)') RETURNING id::text`, srcID).Scan(&docUUID); err != nil {
		t.Fatal(err)
	}
	var itemID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1,'DOCE2E1',1,'journalArticle',NULL,'{}','{}') RETURNING id::text`, srcID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docUUID, itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
		VALUES ($1,$2,'ATTE2E1',1,'DOCE2E1','imported_file','application/epub+zip','e2e.epub',$3,$4,true,false)`,
		srcID, docUUID, pdfPath, hash); err != nil {
		t.Fatal(err)
	}
	return srcID, docUUID
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func httpDelete(t *testing.T, url string) {
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func waitFor(t *testing.T, budget time.Duration, what string, probe func() error) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		if lastErr = probe(); lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: %v", what, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func hashHexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

var ctxBg = context.Background()

func contextBg() context.Context { return ctxBg }

func swapPath(dsn, name string) string {
	i := strings.LastIndex(dsn, "/")
	return dsn[:i+1] + name
}
