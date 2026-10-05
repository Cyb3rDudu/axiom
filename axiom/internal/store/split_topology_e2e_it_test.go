// split_topology_e2e_it_test.go — the F14 #308 ephemeral split ride:
// the THREE-PROCESS topology as real OS processes (one binary, three
// `serve` roles) against caller-provided ephemeral substrate (scratch
// test database + OpenSearch + a live compute worker + a fake Zotero
// probe), with NO host state: everything the processes touch lives in
// this test's temp/scratch spaces and is torn down at the end.
//
// Gate: AXIOM_F14_E2E=1 AND AXIOM_TEST_DATABASE_URL (a *_test DSN) AND
// AXIOM_F14_OS_URL (a live OpenSearch) AND AXIOM_F14_WORKER_PYTHON (a
// python interpreter able to run the compute worker in reference mode —
// `pip install -r axiom-compute-worker/requirements.txt` with
// PYTHONPATH=repo/axiom-compute-worker suffices). The test SPAWNS its
// own ephemeral reference worker as a child process (the CI
// constellation): no externally-managed runner, no manual setup.
// Postgres and OpenSearch stay caller-provided because CI provides them
// as service containers — everything else the topology touches is
// created and torn down by the test itself.
//
// What it proves over the dev-host split-smoke (scripts/dev/split-*.sh):
//  1. boot: library (internal edge), store (internal edge + dispatcher
//     + signed processor-source serving), api (public edge proxying the
//     component surfaces) all reach health — including the zotero check
//     answering from the library-side dependency.
//  2. ingest through the PUBLIC edge (api → store edge → dispatcher →
//     REMOTE-CLASS compute worker → atomic commit → outbox → index) —
//     the same worker URL the whole topology sees, i.e. the
//     remote-compute equivalence leg: the worker is a separate process
//     (separate container/host in the CI variants) and the job still
//     completes including ack, evidenced by search visibility.
//  3. the contract-class checks of split-smoke.sh: typed search hits
//     (no component-internal field leak), typed passage translation.
//  4. kill probe: SIGTERM to the library PROCESS → the public import
//     status route answers the typed library/unavailable envelope, and
//     the body leaks no component host/port/dial detail.
//  5. teardown completeness: no split port stays listening.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// splitProc is one `serve` process of the topology.
type splitProc struct {
	name string
	port int // the process's public api port
	edge int // internal component edge port (library/store only)
	cmd  *exec.Cmd
	logs *bytes.Buffer
}

func (p *splitProc) url() string { return fmt.Sprintf("http://127.0.0.1:%d", p.port) }

// startRefWorker spawns the ephemeral reference-mode compute worker as a
// child process (own port, own work/cache roots) and returns its base
// URL. This is the REMOTE-CLASS leg made self-sufficient: the worker is
// its own OS process with its own identity; the split topology talks to
// it exclusively over HTTP.
func startRefWorker(t *testing.T, work string) (string, *bytes.Buffer) {
	t.Helper()
	py := os.Getenv("AXIOM_F14_WORKER_PYTHON")
	if py == "" {
		t.Skip("AXIOM_F14_WORKER_PYTHON not set (a python able to run the reference compute worker — CI sets it after installing requirements)")
	}
	repoWorker, err := filepath.Abs("../../../axiom-compute-worker")
	if err != nil {
		t.Fatal(err)
	}
	if fi, serr := os.Stat(filepath.Join(repoWorker, "axiom_compute_worker")); serr != nil || !fi.IsDir() {
		t.Fatalf("compute-worker package not found under %s — the worker spawn needs the repo checkout", repoWorker)
	}
	port := freePort(t)
	root := filepath.Join(work, "ref-worker")
	if err := os.MkdirAll(filepath.Join(root, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-m", "axiom_compute_worker")
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"), // pandoc for the EPUB lane
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"PYTHONPATH=" + repoWorker,
		"AXIOM_PROCESSOR_COMPUTE=reference",
		fmt.Sprintf("AXIOM_PROCESSOR_PORT=%d", port),
		"AXIOM_PROCESSOR_BIND_ADDR=127.0.0.1",
		"AXIOM_PROCESSOR_WORK_ROOT=" + filepath.Join(root, "work"),
		"AXIOM_CAPTION_CACHE_DIR=" + filepath.Join(root, "cache"),
	}
	logs := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitFor(t, 90*time.Second, "reference worker healthy", func() error {
		resp, err := http.Get(base + "/v1/health")
		if err != nil {
			if perr := cmd.Process.Signal(syscall.Signal(0)); perr != nil {
				return fmt.Errorf("worker died: %v; logs:\n%s", perr, tailLogs(logs))
			}
			return fmt.Errorf("%w; worker log tail:\n%s", err, tailLogs(logs))
		}
		resp.Body.Close()
		return nil
	})
	return base, logs
}

// splitFakeZotero answers the Server-ID probe (the zotero health check
// both sync-bearing processes run) and serves ONE canonical book with
// its EPUB attachment (the enclosure href points at the fixture file —
// the sync's file-facts pass hashes exactly that).
func splitFakeZotero(t *testing.T, epubPath string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Zotero-Server-ID", "f14-split-e2e")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Last-Modified-Version", "1")
		if strings.HasSuffix(r.URL.Path, "/items") {
			fmt.Fprintf(w, `[
			  {"key":"DOCF14S1","version":1,
			   "data":{"key":"DOCF14S1","version":1,"itemType":"book","title":"Buchkapitel (F14 Split E2E)",
			           "creators":[{"creatorType":"author","firstName":"Ada","lastName":"Lovelace"}]}},
			  {"key":"ATTF14S1","version":1,
			   "data":{"key":"ATTF14S1","version":1,"itemType":"attachment","parentItem":"DOCF14S1",
			           "contentType":"application/epub+zip","filename":"book.epub","linkMode":"imported_file"},
			   "links":{"enclosure":{"href":"file://%s","type":"application/epub+zip"}}}
			]`, epubPath)
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startSplitProc launches one `serve <role>` process with a scrubbed
// environment (only what this topology declares reaches the process).
func startSplitProc(t *testing.T, e *splitE2EEnv, role string, port, edge int) *splitProc {
	t.Helper()
	p := &splitProc{name: role, port: port, edge: edge, logs: &bytes.Buffer{}}
	p.cmd = exec.Command(e.bin, "serve", role)
	env := []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"AXIOM_DATABASE_URL=" + e.dsn,
		fmt.Sprintf("AXIOM_API_PORT=%d", port),
		"AXIOM_BIND_ADDR=127.0.0.1",
		"AXIOM_OPENSEARCH_URL=" + e.osURL,
		"AXIOM_OS_INDEX=" + e.osIndex,
		"AXIOM_PROCESSOR_URLS=" + e.runner,
		"AXIOM_PROCESSOR_URL=" + e.runner,
		"AXIOM_QUERY_RUNNER_URL=" + e.runner,
		"AXIOM_PROCESSOR_SOURCE_SECRET=f14-split-e2e-secret",
		"AXIOM_ARTIFACT_ROOT=" + filepath.Join(e.work, "artifacts"),
		"AXIOM_QUARANTINE_ROOT=" + filepath.Join(e.work, "quarantine"),
		"AXIOM_FIXER_INVOKER_ENABLED=0",
		"AXIOM_ZOTERO_BASE=" + e.zotero + "/api",
		"AXIOM_ZOTERO_WRITE_KEY_FILE=" + filepath.Join(e.work, "no-write-key"),
		"AXIOM_COMPONENT_TIMEOUT=45s",
		"AXIOM_DISPATCHER_ENABLED=0",
		"AXIOM_LIBRARY_IMPORT_PROVIDERS=",
	}
	switch role {
	case "library":
		env = append(env,
			"AXIOM_LIBRARY_IMPORT_PROVIDERS=fake",
			fmt.Sprintf("AXIOM_LIBRARY_DATABASE_URL=%s", e.libDSN),
			fmt.Sprintf("AXIOM_INTERNAL_LIBRARY_ADDR=127.0.0.1:%d", edge),
		)
	case "store":
		env = append(env,
			"AXIOM_DISPATCHER_ENABLED=1",
			"AXIOM_DISPATCHER_WORKER_ID=f14-split-store",
			// the dispatcher signs processor-source URLs; the worker fetches
			// them from THIS process (it owns the claim loop)
			fmt.Sprintf("AXIOM_PROCESSOR_SOURCE_BASE_URL=http://127.0.0.1:%d", port),
			fmt.Sprintf("AXIOM_INTERNAL_STORE_ADDR=127.0.0.1:%d", edge),
		)
	case "api":
		env = append(env,
			// the api role carries the sync role too (its process hosts
			// the Zotero sync): it needs the Library database as well.
			fmt.Sprintf("AXIOM_LIBRARY_DATABASE_URL=%s", e.libDSN),
			fmt.Sprintf("AXIOM_LIBRARY_URL=http://127.0.0.1:%d", e.libraryEdge),
			fmt.Sprintf("AXIOM_STORE_URL=http://127.0.0.1:%d", e.storeEdge),
		)
	default:
		t.Fatalf("unknown role %q", role)
	}
	p.cmd.Env = env
	// tee to a file under the work root: a wedged boot leaves forensic
	// evidence even while the in-memory buffer is unreachable
	logDir := filepath.Join(e.work, "logs")
	_ = os.MkdirAll(logDir, 0o755)
	logFile, err := os.OpenFile(filepath.Join(logDir, role+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	p.cmd.Stdout = io.MultiWriter(p.logs, logFile)
	p.cmd.Stderr = p.cmd.Stdout
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.cmd.Process != nil {
			p.cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { p.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				p.cmd.Process.Kill()
			}
		}
	})
	return p
}

type splitE2EEnv struct {
	dsn     string
	dbName  string
	libDSN  string
	libName string
	runner  string
	osURL   string
	osIndex string
	bin     string
	work    string
	zotero  string
	api     *splitProc
	library *splitProc
	store   *splitProc
	libraryEdge,
	storeEdge int
	workerLogs *bytes.Buffer
	docUUIDSource,
	docUUID string
	pool *pgxpool.Pool
}

func TestF14SplitTopologyE2E(t *testing.T) {
	if os.Getenv("AXIOM_F14_E2E") != "1" {
		t.Skip("AXIOM_F14_E2E != 1; skipping the F14 split topology E2E")
	}
	baseDSN := os.Getenv("AXIOM_TEST_DATABASE_URL")
	osURL := os.Getenv("AXIOM_F14_OS_URL")
	if baseDSN == "" || osURL == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL / AXIOM_F14_OS_URL incomplete; skipping")
	}
	if !strings.Contains(strings.Split(baseDSN, "?")[0], "_test") {
		t.Fatalf("refusing to run against non-test database %q", baseDSN)
	}
	ctx := contextBg()

	// Scratch DBs (own names, dropped at cleanup) — no host state. TWO of
	// them since #358: the Store database and the LIBRARY database (the
	// Zotero mirror's home — the split the E2E proves for real).
	dbName := fmt.Sprintf("axiom_f14_split_%d_test", os.Getpid())
	libName := fmt.Sprintf("axiom_f14_lib_%d_test", os.Getpid())
	e := &splitE2EEnv{
		osURL:       osURL,
		dbName:      dbName,
		libName:     libName,
		osIndex:     fmt.Sprintf("f14-split-chunks-%d", os.Getpid()),
		libraryEdge: freePort(t),
		storeEdge:   freePort(t),
	}
	e.dsn = swapPath(baseDSN, dbName)
	e.libDSN = swapPath(baseDSN, libName)
	admin, err := pgxpool.New(ctx, swapPath(baseDSN, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, dbName); err != nil {
		t.Fatal(err)
	}
	admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName)
	admin.Exec(ctx, `DROP DATABASE IF EXISTS `+libName)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+libName); err != nil {
		t.Fatalf("create scratch library db: %v", err)
	}
	admin.Close()
	t.Cleanup(func() {
		ctx2 := contextBg()
		if e.pool != nil {
			e.pool.Close()
		}
		p, err := pgxpool.New(ctx2, swapPath(baseDSN, "postgres"))
		if err == nil {
			p.Exec(ctx2, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, dbName)
			p.Exec(ctx2, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, libName)
			p.Exec(ctx2, `DROP DATABASE IF EXISTS `+dbName)
			p.Exec(ctx2, `DROP DATABASE IF EXISTS `+libName)
			p.Close()
		}
		httpDelete(t, osURL+"/"+e.osIndex)
	})

	// Fixture: the runner's own test corpus EPUB (always a Tier-1 text
	// layer, so the preflight quality gate passes).
	src, err := filepath.Abs("../../internal/backfill/testdata/book.epub")
	if err != nil {
		t.Fatal(err)
	}
	epubBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("epub fixture: %v", err)
	}
	hash := hashHexOf(epubBytes)

	// Worktree binary + fake Zotero + state roots + the SELF-SPAWNED
	// ephemeral reference worker (the remote-class leg — no external
	// runner, no manual setup; the CI job only installs requirements).
	e.work = t.TempDir()
	e.runner, e.workerLogs = startRefWorker(t, e.work)

	// The remote-class worker's advertised identity (F10 name): the job's
	// completion is attributed to exactly this worker.
	caps, err := http.Get(e.runner + "/v1/capabilities")
	if err != nil {
		t.Fatalf("runner capabilities: %v", err)
	}
	cbody, _ := io.ReadAll(caps.Body)
	caps.Body.Close()
	var capDoc struct {
		Processor struct {
			Name string `json:"name"`
		} `json:"processor"`
	}
	if err := json.Unmarshal(cbody, &capDoc); err != nil || capDoc.Processor.Name == "" {
		t.Fatalf("runner capabilities unreadable: %s", cbody)
	}
	t.Logf("remote-class worker: %s (capabilities name, F10 identity)", capDoc.Processor.Name)

	e.bin = filepath.Join(e.work, "axiom")
	build := exec.Command("go", "build", "-o", e.bin, "../../cmd/axiom")
	build.Env = append(os.Environ(), "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build axiom: %v\n%s", err, out)
	}
	zot := splitFakeZotero(t, src)
	e.zotero = zot.URL

	// --- the three processes, started SEQUENTIALLY with health waits ---
	// (the split-up.sh discipline): db.Migrate() owns the schema, and on a
	// FRESH scratch database two concurrent migrates collide on CREATE
	// TYPE (duplicate pg_type) — the first process up migrates, the rest
	// no-op. Order: library (owns the api's library edge), store (owns the
	// dispatcher + the api's store edge), api last.
	// Per-process expected CHECK NAMES (DM07 review round 2): a silently
	// lost registration (e.g. ingest-runner never registered) must fail
	// HERE by name again — the public edge only asserts the component
	// probes. The api process is exempt (its checks are the library/store
	// probes, asserted at the public edge below).
	wantChecks := map[string][]string{
		"library": {"postgres", "zotero"},
		"store":   {"postgres", "query-runner", "ingest-runner"},
	}
	startAndWait := func(role string, port, edge int) *splitProc {
		proc := startSplitProc(t, e, role, port, edge)
		waitFor(t, 90*time.Second, role+" process healthy", func() error {
			resp, err := http.Get(proc.url() + "/api/health")
			if err != nil {
				// alive but not answering: surface the boot log tail so a
				// wedged start names its stall, not just "refused"
				return fmt.Errorf("%w; boot log tail:\n%s", err, tailLogs(proc.logs))
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			var h struct {
				OK     bool           `json:"ok"`
				Checks map[string]any `json:"checks"`
			}
			if err := json.Unmarshal(body, &h); err != nil || !h.OK {
				return fmt.Errorf("health not ok: %s", tailLogs(proc.logs))
			}
			for _, name := range wantChecks[role] {
				if v, has := h.Checks[name]; !has || v != "ok" {
					return fmt.Errorf("%s process lost check %s = %v", role, name, v)
				}
			}
			return nil
		})
		return proc
	}
	e.library = startAndWait("library", freePort(t), e.libraryEdge)
	e.store = startAndWait("store", freePort(t), e.storeEdge)
	e.api = startAndWait("api", freePort(t), 0)

	// 1. public-edge health: ok + the component-edge checks (DM07 #316:
	// the credential-free api process proxies its dependency visibility
	// over the component health — each component's own /api/health folds
	// its postgres/zotero/runner checks into ok, which startAndWait
	// above already polices per process). The four LEGACY check names
	// live on the component processes now, not at the edge.
	waitFor(t, 90*time.Second, "split public edge healthy", func() error {
		resp, err := http.Get(e.api.url() + "/api/health")
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
		for _, check := range []string{"library", "store"} {
			if v, has := health.Checks[check]; !has || v != "ok" {
				return fmt.Errorf("health check %s = %v", check, v)
			}
		}
		return nil
	})

	// 2. THE #358 RIDE: the sync through the library process's public
	// edge — the mirror lands in the LIBRARY database, the projection +
	// revision intake in the STORE database (two databases, one sync),
	// and the dispatcher's claim/persist follows on the store side. No
	// seeding, no manual intake: the Zotero observation IS the intake.
	e.pool, err = pgxpool.New(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	libPool, err := pgxpool.New(ctx, e.libDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(libPool.Close)
	syncResp := postJSON(t, e.library.url()+"/api/zotero/sync", map[string]any{})
	if syncResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(syncResp.Body)
		t.Fatalf("sync status %d: %s; library log tail:\n%s", syncResp.StatusCode, body, tailLogs(e.library.logs))
	}
	syncResp.Body.Close()
	// The mirror lives in the LIBRARY database; the durable identities
	// feed the search filter below.
	if err := libPool.QueryRow(ctx,
		`SELECT d.id::text, s.id::text FROM zotero_documents d, zotero_sources s
		 WHERE d.zotero_key='DOCF14S1' AND d.source_id=s.id`).Scan(&e.docUUID, &e.docUUIDSource); err != nil {
		t.Fatalf("library mirror rows after sync: %v", err)
	}
	// The store-side projection + intake exist (the sync's store phase).
	var proj int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM store_documents WHERE rendition_key='ATTF14S1' AND content_hash=$1`, hash).Scan(&proj); err != nil {
		t.Fatal(err)
	}
	if proj != 1 {
		t.Fatalf("store projection after sync = %d, want 1 (the sync's store phase)", proj)
	}
	var pending int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM ingest_jobs WHERE intake_kind='revision' AND status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("revision intake after sync = %d, want 1", pending)
	}

	// 3. the remote-compute leg + contract-class search checks: the job
	// runs on the only configured worker (e.runner), acks, and becomes
	// searchable through the public edge with the TYPED hit shape.
	var chunkID string
	waitFor(t, 8*time.Minute, "split-ingested revision becomes searchable", func() error {
		var status, code, msg, runnerName string
		_ = e.pool.QueryRow(contextBg(),
			`SELECT status::text, COALESCE(error_code,''), COALESCE(error_message,''), COALESCE(runner_name,'') FROM ingest_jobs WHERE intake_kind='revision' LIMIT 1`).
			Scan(&status, &code, &msg, &runnerName)
		if status == "failed" || status == "skipped" {
			// dump the processor-side result for this job: the §-validator
			// rejection names the symptom; the raw chunks name the cause
			var goJobID string
			if err := e.pool.QueryRow(contextBg(),
				`SELECT id FROM ingest_jobs WHERE intake_kind='revision' LIMIT 1`).Scan(&goJobID); err == nil {
				if rres, rerr := http.Get(e.runner + "/v1/jobs/" + goJobID + "/result"); rerr == nil {
					rb, _ := io.ReadAll(rres.Body)
					rres.Body.Close()
					var rdoc struct {
						Chunks []struct {
							Ref     string `json:"ref"`
							Locator struct {
								Type     string `json:"type"`
								CFIStart string `json:"cfi_start"`
								CFIEnd   string `json:"cfi_end"`
							} `json:"locator"`
						} `json:"chunks"`
					}
					if jerr := json.Unmarshal(rb, &rdoc); jerr == nil {
						for i, c := range rdoc.Chunks {
							t.Logf("worker result chunk %d: ref=%s locator=%+v", i, c.Ref, c.Locator)
						}
					}
				}
			}
			return fmt.Errorf("revision job terminal: %s %s %s; store log tail:\n%s; worker log tail:\n%s", status, code, msg, tailLogsN(e.store.logs, 200), tailLogsN(e.workerLogs, 400))
		}
		if status == "completed" && runnerName == "" {
			return fmt.Errorf("completed job carries no runner identity")
		}
		sres, err := http.Post(e.api.url()+"/api/v1/search", "application/json",
			strings.NewReader(`{"query":"chapter inhalt","top_n":5,"filters":{"document_ids":["`+e.docUUID+`"]}}`))
		if err != nil {
			return err
		}
		defer sres.Body.Close()
		body, _ := io.ReadAll(sres.Body)
		if sres.StatusCode != 200 {
			return fmt.Errorf("search status %d: %s; store log tail:\n%s", sres.StatusCode, body, tailLogs(e.store.logs))
		}
		var res struct {
			Hits []struct {
				ChunkID string `json:"chunk_id"`
				Source  struct {
					DocID    string `json:"doc_id"`
					Title    string `json:"title"`
					RecordID string `json:"record_id"`
				} `json:"source"`
				Locator struct {
					Kind string `json:"kind"`
				} `json:"locator"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return err
		}
		if len(res.Hits) == 0 {
			return fmt.Errorf("no hits yet (job status=%s)", status)
		}
		hit := res.Hits[0]
		if hit.Source.DocID == "" || hit.Locator.Kind == "" {
			return fmt.Errorf("hit misses the typed shape (source.doc_id / locator.kind): %s", body)
		}
		// #358 DoD: the title hydrates from the STORE's projection —
		// written by the sync's store phase, never read from the mirror.
		if hit.Source.Title != "Buchkapitel (F14 Split E2E)" {
			return fmt.Errorf("hit title = %q, want the projection-carried bibliography title", hit.Source.Title)
		}
		if hit.Source.RecordID != "" {
			t.Fatalf("search: component-internal field record_id leaked into the public answer: %s", body)
		}
		chunkID = hit.ChunkID
		return nil
	})
	if chunkID == "" {
		t.Fatal("no typed search hit through the split public edge")
	}

	// Passage over the ADR-0001 translation (api → store edge), typed.
	presp, err := http.Get(e.api.url() + "/api/v1/passage/" + chunkID)
	if err != nil {
		t.Fatal(err)
	}
	pbody, _ := io.ReadAll(presp.Body)
	presp.Body.Close()
	if presp.StatusCode != 200 {
		t.Fatalf("passage status %d: %s", presp.StatusCode, pbody)
	}
	var passage struct {
		ChunkID      string `json:"chunk_id"`
		AttachmentID string `json:"attachment_id"`
		RenditionID  string `json:"rendition_id"`
		Neighbors    []any  `json:"neighbors"`
	}
	if err := json.Unmarshal(pbody, &passage); err != nil {
		t.Fatal(err)
	}
	if passage.AttachmentID == "" || passage.Neighbors == nil {
		t.Fatalf("passage misses attachment_id/neighbors: %s", pbody)
	}
	if passage.RenditionID != "" {
		t.Fatalf("passage: component-internal field rendition_id leaked: %s", pbody)
	}

	// 4. kill probe: the library PROCESS dies → the public import status
	// route answers the typed library/unavailable envelope, leak-free.
	libEdgeProbe := fmt.Sprintf("http://127.0.0.1:%d/internal/v1/library/sources/probe", e.libraryEdge)
	if lresp, lerr := http.Get(libEdgeProbe); lerr != nil {
		t.Fatalf("library edge not answering before the kill: %v — library already dead?", lerr)
	} else {
		lresp.Body.Close()
	}
	if err := e.library.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("TERM library: %v", err)
	}
	var kbody []byte
	waitFor(t, 60*time.Second, "typed library/unavailable envelope", func() error {
		resp, err := http.Get(e.api.url() + "/api/v1/library/imports/f14-split-probe")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		kbody, _ = io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusServiceUnavailable {
			return fmt.Errorf("status %d (want 503): %s", resp.StatusCode, kbody)
		}
		return nil
	})
	var env struct {
		Error struct {
			Component string `json:"component"`
			Class     string `json:"class"`
		} `json:"error"`
	}
	if err := json.Unmarshal(kbody, &env); err != nil {
		t.Fatalf("kill probe body unreadable: %s", kbody)
	}
	if env.Error.Component != "library" || env.Error.Class != "unavailable" {
		t.Fatalf("kill probe: not the typed library/unavailable envelope: %s", kbody)
	}
	for _, leak := range []string{"127.0.0.1", "localhost", "dial", "http://"} {
		if strings.Contains(string(kbody), leak) {
			t.Fatalf("kill probe: body leaks topology detail %q: %s", leak, kbody)
		}
	}

	// 5. teardown completeness: stop the rest, no split port stays bound.
	for _, p := range []*splitProc{e.api, e.store} {
		if p.cmd.Process != nil {
			p.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	var mu sync.Mutex
	bound := map[string]bool{}
	waitFor(t, 30*time.Second, "all split ports released", func() error {
		mu.Lock()
		defer mu.Unlock()
		bound = map[string]bool{}
		for name, port := range map[string]int{"api": e.api.port, "library": e.library.port, "store": e.store.port, "library-edge": e.libraryEdge, "store-edge": e.storeEdge} {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				bound[name] = true // still bound
				continue
			}
			l.Close()
		}
		if len(bound) > 0 {
			return fmt.Errorf("ports still bound: %v", bound)
		}
		return nil
	})
}

// tailLogs renders the last lines of a process's captured output (the
// bytes.Buffer is written by the process goroutine; reading it racily at
// failure time is fine — it is best-effort diagnostics).
func tailLogsN(buf *bytes.Buffer, n int) string {
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func tailLogs(buf *bytes.Buffer) string { return tailLogsN(buf, 25) }

