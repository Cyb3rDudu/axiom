// composition_it_test.go — DB-backed lifecycle witnesses (#298 DoD).
//
// Needs AXIOM_TEST_DATABASE_URL (the dispatcher-IT pattern: a session-unique
// throwaway *_test database, migrated, truncated on cleanup). Covers:
//
//   - FakeBinding: the SAME registry builder (Full/Select) starts the full
//     stack against faked ports (runner client, fixer exec) — the
//     replaceability proof for the injection seams.
//   - OrderedShutdown: dispatcher lease + repair claim + live WS connection
//     all in flight → Stop drains everything cleanly: in-flight rows are
//     left to their recovery owners (never terminalized by the shutdown),
//     the WS connection is closed by the server, no goroutine survives.
//   - StoppedStartLeavesNoGoroutines: a start that fails mid-sequence (http
//     bind conflict as the LAST component) unwinds every started component
//     in-process.
//
// Run with:
//
//	AXIOM_TEST_DATABASE_URL=postgresql://…/scratch_test?sslmode=disable \
//	go test ./internal/composition -run TestIT -v
package composition

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- harness ----------------------------------------------------------------

var compositionTestDBName = fmt.Sprintf("axiom_ng_composition_%d_test", os.Getpid())

type compositionHarness struct {
	pool *pgxpool.Pool
	rep  *repo.Repo
	dsn  string
}

var (
	compHarnessOnce sync.Once
	compHarness     *compositionHarness
)

// openCompositionDB opens the SESSION-shared throwaway DB once (created
// + migrated on first use; per-test state is reset by truncate, the
// dispatcher-IT pattern — cleanup-time drops race backend connection
// teardown with "database is being accessed by other users").
func openCompositionDB(t *testing.T) *compositionHarness {
	t.Helper()
	base := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping composition integration test")
	}
	compHarnessOnce.Do(func() {
		compHarness = createCompositionDB(t, base)
	})
	compHarness.truncate(t)
	return compHarness
}

func createCompositionDB(t *testing.T, base string) *compositionHarness {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// maintenance connection creates the throwaway DB
	maint := cloneDSN(u, "postgres").String()
	ctx := context.Background()
	mp, err := pgxpool.New(ctx, maint)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	var exists bool
	if err := mp.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, compositionTestDBName).Scan(&exists); err != nil {
		t.Fatalf("check db exists: %v", err)
	}
	if !exists {
		if _, err := mp.Exec(ctx, "CREATE DATABASE "+compositionTestDBName); err != nil {
			t.Fatalf("create db: %v", err)
		}
	}
	mp.Close()

	h := &compositionHarness{dsn: cloneDSN(u, compositionTestDBName).String()}
	database, err := db.Open(ctx, h.dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h.pool = database.Pool()
	h.rep = repo.New(h.pool)
	return h
}

// truncate resets every table the composition ITs touch (guarded by the
// _test suffix, same refusal as the dispatcher IT).
func (h *compositionHarness) truncate(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var dbName string
	if err := h.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("read current_database: %v", err)
	}
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("REFUSING to truncate: %q does not end in _test", dbName)
	}
	if _, err := h.pool.Exec(ctx,
		`TRUNCATE ingest_jobs, zotero_attachments, zotero_documents, zotero_items,
		         zotero_item_collections, zotero_collections, zotero_sources,
		         zotero_selections, zotero_collection_selections,
		         repair_cases, zotero_write_audit, processing_snapshots,
		         processing_chunks, processing_artifacts, opensearch_outbox,
		         kg_entity_roots, kg_relation_triples, kg_relation_evidence_docs,
		         kg_superseded_entities, processing_entities, processing_entity_mentions,
		         processing_entity_relationships, processing_chunk_dense_embeddings,
		         processing_chunk_relationships, processing_chunk_sparse_embeddings
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// cloneDSN copies the DSN with a different database name (dispatcher-IT helper shape).
func cloneDSN(u *url.URL, dbname string) *url.URL {
	c := *u
	c.Path = "/" + dbname
	return &c
}

func (h *compositionHarness) seedPendingJob(t *testing.T, key string) string {
	t.Helper()
	ctx := context.Background()
	var srcID, docID, itemID, attID, jobID string
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO zotero_sources (base_url, library_id, server_id) VALUES ($1,'users/0','srv') RETURNING id::text`,
		"http://host:"+key).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		 VALUES ($1,$2,1,'book','Comp IT') RETURNING id::text`, srcID, key).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		 VALUES ($1,$2,1,'book',NULL,$3,$4) RETURNING id::text`,
		srcID, key, `{"key":"`+key+`"}`, `{"key":"`+key+`","version":1,"itemType":"book","title":"Comp IT"}`).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	h.pool.Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docID, itemID)
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
		 VALUES ($1,$2,$3,1,$3,'imported_file','application/pdf','x.pdf','/tmp/x.pdf',$4,true,false)
		 RETURNING id::text`, srcID, docID, key, "sha256:"+key).Scan(&attID); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		VALUES ($1,$2,$3,$4,'pending',3) RETURNING id::text`,
		srcID, docID, attID, "sha256:"+key).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

// seedQueuedRepairCase inserts a queued repair case for a fresh attachment.
func (h *compositionHarness) seedQueuedRepairCase(t *testing.T, key string) string {
	t.Helper()
	ctx := context.Background()
	var srcID, docID, attID string
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO zotero_sources (base_url, library_id, server_id) VALUES ($1,'users/0','srv') RETURNING id::text`,
		"https://zotero.host:"+key).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		 VALUES ($1,$2,1,'book','Repair IT') RETURNING id::text`, srcID, "DOC-"+key).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path)
		 VALUES ($1,$2,$3,1,$4,'imported_file','application/pdf','x.pdf',$5)
		 RETURNING id::text`, srcID, docID, key, "DOC-"+key, "/tmp/does-not-matter.pdf").Scan(&attID); err != nil {
		t.Fatal(err)
	}
	c, _, err := h.rep.CreateRepairCase(ctx, attID, "", "reparierbar", []byte(`{}`))
	if err != nil || c == nil {
		t.Fatalf("CreateRepairCase: %v %v", err, c)
	}
	if err := h.rep.QueueRepairCase(ctx, c.ID, "reparierbar", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (h *compositionHarness) jobStatus(t *testing.T, jobID string) string {
	t.Helper()
	var status string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status::text FROM ingest_jobs WHERE id = $1`, jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func (h *compositionHarness) repairStatus(t *testing.T, caseID string) string {
	t.Helper()
	var status string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status::text FROM repair_cases WHERE id = $1`, caseID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// pollUntil polls cond every 100ms until it returns true or the deadline.
func pollUntil(t *testing.T, what string, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- fake ports --------------------------------------------------------------

// fakeRunner is the fake binding for the dispatcher→runner port: negotiates
// contract v1, accepts every submit, and reports jobs "running" forever
// (the drain witness needs an in-flight lease that never completes).
type fakeRunner struct {
	mu      sync.Mutex
	submits int
	status  string // JobStatus response; "running" default
}

func (f *fakeRunner) Capabilities(ctx context.Context) (*processor.Capabilities, error) {
	caps := &processor.Capabilities{ContractVersions: []string{"1.5"}}
	caps.Processor.Name = "fake-runner"
	caps.Processor.Version = "test"
	caps.Formats = []string{"application/pdf", "application/epub+zip"}
	// The full feature surface the default profile (full-rag-v1) demands —
	// a sparser fake would mark every job NOT_PROCESSABLE at claim time.
	caps.Features = map[string]bool{
		"markdown": true, "page_locators": true, "section_hierarchy": true,
		"images": false, "dense_embeddings": true, "sparse_embeddings": true,
		"entities": true, "entity_relationships": true,
		"query_embedding": true, "reranking": true,
	}
	caps.Models = map[string]any{
		"dense_embedding": map[string]any{"name": "fake-bge", "dimensions": 3},
	}
	caps.Limits.MaxConcurrentJobs = 2
	caps.Limits.MaxSourceBytes = 1 << 30
	return caps, nil
}

func (f *fakeRunner) SubmitProcess(ctx context.Context, req *processor.ProcessRequest) (*processor.ProcessAccepted, error) {
	f.mu.Lock()
	f.submits++
	f.mu.Unlock()
	return &processor.ProcessAccepted{ContractVersion: "1.5", JobID: req.JobID, Status: "accepted"}, nil
}

func (f *fakeRunner) Preflight(ctx context.Context, doc []byte, contentType string) (*processor.PreflightReport, error) {
	return &processor.PreflightReport{}, nil
}

func (f *fakeRunner) JobStatus(ctx context.Context, jobID string) (*processor.JobStatus, error) {
	st := "running"
	f.mu.Lock()
	if f.status != "" {
		st = f.status
	}
	f.mu.Unlock()
	return &processor.JobStatus{ContractVersion: "1.5", JobID: jobID, Status: st, Stage: "fake"}, nil
}

func (f *fakeRunner) JobResult(ctx context.Context, jobID string) ([]byte, error) {
	return nil, fmt.Errorf("fake runner: no result")
}

func (f *fakeRunner) Artifact(ctx context.Context, jobID, ref string) ([]byte, error) {
	return nil, fmt.Errorf("fake runner: no artifacts")
}

func (f *fakeRunner) Cancel(ctx context.Context, jobID string) error { return nil }

func (f *fakeRunner) Ack(ctx context.Context, jobID string, ack processor.Ack) error { return nil }

func (f *fakeRunner) Health(ctx context.Context) error { return nil }

func (f *fakeRunner) LaneCapacity(ctx context.Context) (int, error) { return 2, nil }

// fakeQueryRunnerSrv serves the query-side runner contract over HTTP for
// the search component's *processor.Client (the query port's local binding
// is a plain HTTP client, so the fake is a plain HTTP server). NOT
// cleanup-registered: callers close it BEFORE the settle witness so its
// serve/persistConn goroutines cannot read as leaks.
func fakeQueryRunnerSrv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"contract_versions": []string{"1.5"},
			"processor":         map[string]string{"name": "fake-query", "version": "test"},
			"formats":           []string{"application/pdf"},
			"features":          map[string]bool{"query_embedding": true, "reranking": true},
			"models":            map[string]any{},
			"limits":            map[string]int{"max_concurrent_jobs": 2, "max_source_bytes": 1 << 30},
			"models_warmed":     true,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fullStackCfg builds the everything-enabled config against the harness DB.
func fullStackCfg(t *testing.T, h *compositionHarness, queryURL string) config.Config {
	t.Helper()
	cfg := config.Load()
	cfg.DatabaseURL = h.dsn
	cfg.APIPort = freePort(t)
	cfg.BindAddr = "127.0.0.1"
	cfg.DispatcherEnabled = true
	cfg.FixerInvokerEnabled = true
	cfg.FixerCommand = "/nonexistent-fixer"
	cfg.FixerConcurrency = 1
	cfg.QueryRunnerURL = queryURL
	cfg.ProcessorURL = "http://127.0.0.1:1" // overridden by the fake port anyway
	cfg.IngestFallbackURL = ""
	cfg.ProcessorURLs = []string{"http://127.0.0.1:1"}
	cfg.OpenSearchURL = "" // no outbox drainer against a real OpenSearch in ITs
	cfg.ZoteroWriteKeyFile = writeKeyFile(t)
	cfg.ArtifactRoot = t.TempDir()
	cfg.QuarantineRoot = t.TempDir()
	cfg.RunnerHealthInterval = 0 // no background health probe in ITs
	// Deterministic ITs: no preflight detour (the dev host env may set
	// AXIOM_DISPATCHER_PREFLIGHT), explicit concurrency, fast poll cadence.
	cfg.DispatcherPreflightEnabled = false
	cfg.DispatcherConcurrency = 1
	// Never inherit the dev host's contextual rules: an unsynced throwaway
	// DB would only log the #262 degradation — noise, and env-dependent.
	cfg.ContextualCollectionPaths = nil
	cfg.ContextualTags = nil
	return cfg
}

func writeKeyFile(t *testing.T) string {
	t.Helper()
	p := t.TempDir() + "/write-api-key"
	if err := os.WriteFile(p, []byte("it-test-write-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakePorts binds every seam: fake ingest runner, fake query runner, fake
// fixer exec (blocks until ctx done — the in-flight repair claim witness).
func fakePorts(t *testing.T, runner *fakeRunner, querySrv *httptest.Server) Ports {
	t.Helper()
	return Ports{
		IngestRunner: func(cfg config.Config, logger *log.Logger) (RunnerClient, error) {
			return runner, nil
		},
		QueryRunner: func(cfg config.Config) (*processor.Client, error) {
			return processor.New(processor.Options{BaseURL: querySrv.URL})
		},
		FixerExec: func(ctx context.Context, command string, args []string, budget time.Duration, extraEnv []string) (int, string, error) {
			<-ctx.Done() // hold the claimed case in-flight until shutdown cancels
			return -1, "cancelled by shutdown", ctx.Err()
		},
	}
}

// TestIT_FakeBindingFullRegistry — the #298 fake-binding DoD: the SAME
// registry (Full over the config-derived role set) starts the FULL stack
// against faked ports, reaches internal readiness, serves, and stops clean.
func TestIT_FakeBindingFullRegistry(t *testing.T) {
	h := openCompositionDB(t)
	runner := &fakeRunner{}
	querySrv := fakeQueryRunnerSrv(t)
	cfg := fullStackCfg(t, h, querySrv.URL)

	root, err := Full(cfg, testLogger(), fakePorts(t, runner, querySrv))
	if err != nil {
		t.Fatal(err)
	}
	// Baseline AFTER Full: its construction (zotero ServerID probe) must
	// not leak into the settle delta.
	base := goroutineBaseline(t)
	sigCtx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := root.Start(sigCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer root.Stop(context.Background())

	// INTERNAL readiness aggregation: dispatcher negotiated capabilities.
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer readyCancel()
	if err := root.Ready(readyCtx); err != nil {
		t.Fatalf("ready: %v", err)
	}

	resp, err := noKeepAliveGet(fmt.Sprintf("http://127.0.0.1:%d/api/health", cfg.APIPort))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // drain+close BEFORE Stop: a lingering keep-alive
	// socket would hold a server conn out of Shutdown's idle set.
	if err != nil {
		t.Fatalf("read health body: %v", err)
	}
	if !strings.Contains(string(body), "canonical_name") {
		t.Fatalf("health must keep the frozen identity fields, got: %s", body)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status: %d", resp.StatusCode)
	}

	// Ordered stop; then the drains must have joined and the process quiet.
	// The test-owned fake query server closes first — its serve/keep-alive
	// goroutines are infrastructure, not composition leaks.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	root.Stop(stopCtx)
	querySrv.Close()

	select {
	case <-root.disp.Stopped():
	default:
		t.Fatal("dispatcher Run must have returned after Stop")
	}
	select {
	case <-root.inv.Stopped():
	default:
		t.Fatal("fixer invoker Run must have returned after Stop")
	}
	assertGoroutinesSettled(t, base, 10*time.Second)
}

// noKeepAliveGet issues a GET whose transport never holds keep-alive
// sockets open — the settle witness must not see the TEST's own client
// connections as leaks.
func noKeepAliveGet(url string) (*http.Response, error) {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	return client.Get(url)
}

// TestIT_OrderedShutdownHonorsInFlight — the #298 shutdown DoD: a claimed
// dispatcher lease, a claimed repair case and a live WS connection are ALL
// in flight when Stop runs. The shutdown must drain cleanly: the in-flight
// job stays 'processing' and the repair case stays 'in_repair' (both handed
// to their recovery owners — the #271/#264 guard semantics, never
// terminalized by the shutdown), the WS connection is closed by the server,
// and no goroutine survives.
func TestIT_OrderedShutdownHonorsInFlight(t *testing.T) {
	h := openCompositionDB(t)
	jobID := h.seedPendingJob(t, "SHUT1")

	runner := &fakeRunner{status: "running"} // poll loop never completes the job
	querySrv := fakeQueryRunnerSrv(t)
	cfg := fullStackCfg(t, h, querySrv.URL)
	// Fast, deterministic dispatcher cadence for the IT.
	cfg.DispatcherLeaseDuration = 2 * time.Minute

	root, err := Full(cfg, testLogger(), fakePorts(t, runner, querySrv))
	if err != nil {
		t.Fatal(err)
	}
	// Baseline AFTER Full: its construction (zotero ServerID probe) must
	// not leak into the settle delta.
	base := goroutineBaseline(t)
	sigCtx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := root.Start(sigCtx); err != nil {
		t.Fatalf("start: %v", err)
	}

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer readyCancel()
	if err := root.Ready(readyCtx); err != nil {
		t.Fatalf("ready: %v", err)
	}

	// The dispatcher claims the seeded job FIRST (in-flight lease held by
	// the renewal loop). The repair case is only seeded AFTER the claim:
	// the #282 wave gate holds NEW document claims while a heal is
	// queued/running, so both in-flights could never coexist the other way
	// around — that ordering is the gate working as designed.
	pollUntil(t, "job claimed", 15*time.Second, func() bool {
		return h.jobStatus(t, jobID) == "processing"
	})
	caseID := h.seedQueuedRepairCase(t, "SHUT1")
	// The fixer invoker claims the seeded repair case on its next poll
	// (first poll ran at start, before the case existed; interval 30s) —
	// in-flight exec held by the fake FixerExec.
	pollUntil(t, "repair case claimed", 45*time.Second, func() bool {
		return h.repairStatus(t, caseID) == "in_repair"
	})

	// A live WS connection (loopback peer, no token needed).
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/api/ws", cfg.APIPort)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(map[string]string{"type": "subscribe", "topic": "all"}); err != nil {
		t.Fatalf("ws subscribe: %v", err)
	}
	// The subscribed ack proves the connection is fully live (bus
	// subscription attached), not just upgraded.
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ack map[string]any
	if err := ws.ReadJSON(&ack); err != nil || ack["type"] != "subscribed" {
		t.Fatalf("ws ack: %v %v", err, ack)
	}
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))

	// STOP with everything in flight.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	root.Stop(stopCtx)
	querySrv.Close() // test-owned fake: close before the settle witness

	// WS closed by the server: the read fails with close/EOF, not a timeout.
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			break // closed — exactly what an ordered shutdown owes the peer
		}
	}

	// In-flight artifacts are PRESERVED for their recovery owners: the
	// dispatcher RELEASED the un-attempt-ceiling lease back to pending
	// (#271 P2 — immediately reclaimable, never terminalized by the
	// shutdown; deterministic here: attempt 1 of max 3, 202 observed).
	if st := h.jobStatus(t, jobID); st != "pending" {
		t.Fatalf("in-flight job's lease must be released back to 'pending' for immediate reclaim, got %q", st)
	}
	if st := h.repairStatus(t, caseID); st != "in_repair" {
		t.Fatalf("in-flight repair case must stay 'in_repair' for the stale-reaper, got %q", st)
	}

	// Both drains joined…
	select {
	case <-root.disp.Stopped():
	default:
		t.Fatal("dispatcher drain did not join")
	}
	select {
	case <-root.inv.Stopped():
	default:
		t.Fatal("fixer invoker drain did not join")
	}
	// …and the process is internally quiet (the queries above reuse the
	// harness pool, not a root goroutine).
	assertGoroutinesSettled(t, base, 10*time.Second)
}

// TestIT_StoppedStartLeavesNoGoroutines — a start that fails MID-sequence
// unwinds every already-started component: the http bind conflict fires as
// the LAST component with the full DB-backed stack running beneath it.
func TestIT_StoppedStartLeavesNoGoroutines(t *testing.T) {
	h := openCompositionDB(t)
	runner := &fakeRunner{}
	querySrv := fakeQueryRunnerSrv(t)
	cfg := fullStackCfg(t, h, querySrv.URL)

	// Hold the api port so the http component (last in start order) fails.
	hold, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.APIPort))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()

	root, err := Full(cfg, testLogger(), fakePorts(t, runner, querySrv))
	if err != nil {
		t.Fatal(err)
	}
	// Baseline AFTER Full: its construction (zotero ServerID probe) must
	// not leak into the settle delta.
	base := goroutineBaseline(t)
	sigCtx, stop := context.WithCancel(context.Background())
	defer stop()
	startErr := root.Start(sigCtx)
	if startErr == nil {
		root.Stop(context.Background())
		t.Fatal("start with a bound port must fail")
	}
	querySrv.Close() // test-owned fake: close before the settle witness
	// A start failure must not surface through Fatal (it is not a serve
	// error); nothing is sent, so the receive must time out, not block.
	select {
	case err := <-root.Fatal():
		t.Fatalf("start failure leaked into Fatal: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	assertGoroutinesSettled(t, base, 10*time.Second)
}

// TestIT_StoreSliceArmsProcessorSource — the R2-1 witness: with NO sync
// role selected (the store slice), the processor-source endpoint must
// still be ARMED — the secret wiring sits BEFORE the Library gate. The
// wire answer stays a uniform 404 (no existence oracle by design), so the
// witness reads the logged rejection branch: an armed endpoint answers
// bad_signature, a regressed wiring answers disabled_no_secret.
func TestIT_StoreSliceArmsProcessorSource(t *testing.T) {
	h := openCompositionDB(t)
	querySrv := fakeQueryRunnerSrv(t)
	cfg := fullStackCfg(t, h, querySrv.URL)
	cfg.ProcessorSourceSecret = "it-source-secret"
	cfg.ProcessorSourceBaseURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.APIPort)

	logs := &bytes.Buffer{}
	root, err := Select(cfg, log.New(logs, "", 0), fakePorts(t, &fakeRunner{}, querySrv),
		RoleAPI, RoleStore, RoleEvents, RoleSearch, RoleIngest, RoleDispatcher)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	sigCtx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := root.Start(sigCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer root.Stop(context.Background())

	resp, err := noKeepAliveGet(fmt.Sprintf("http://127.0.0.1:%d/api/processor/source/00000000-0000-0000-0000-000000000000?exp=1&sig=bad", cfg.APIPort))
	if err != nil {
		t.Fatalf("source endpoint: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("source endpoint must 404 (uniform, no oracle), got %d", resp.StatusCode)
	}
	if strings.Contains(logs.String(), "disabled_no_secret") {
		t.Fatalf("store slice must arm the processor-source endpoint (secret wiring before the Library gate); log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "bad_signature") && !strings.Contains(logs.String(), "bad_exp") {
		t.Fatalf("armed endpoint must reject on the signature branch, log: %s", logs.String())
	}
}
