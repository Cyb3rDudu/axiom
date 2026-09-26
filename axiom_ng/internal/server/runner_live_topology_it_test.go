// runner_live_topology_it_test.go — the #249 solo-mode acceptance, re-homed
// from internal/dispatcher (F09 #303): the store-boundary lint keeps the
// transport package out of Store packages, tests included, and this probe
// asserts the HTTP route (/api/runners/live) end-to-end — transport truth.
// It drives a REAL dispatcher (exported surface only) against a fake
// processor, the event bus, the deriver and the route, exactly as the
// production single-agent wiring does. Gated like the other DB suites:
// AXIOM_TEST_DATABASE_URL (scratch DB).
package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/dispatcher"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// busyRunnerProcessor satisfies the dispatcher's runner surface with a job
// that STAYS running on the "extract" stage — the topology needs a busy
// phase long enough to observe the live view.
type busyRunnerProcessor struct{}

func (f *busyRunnerProcessor) Capabilities(ctx context.Context) (*processor.Capabilities, error) {
	caps := &processor.Capabilities{ContractVersions: []string{"1.0"}, Formats: []string{"application/pdf"}}
	caps.Limits.MaxConcurrentJobs = 1
	return caps, nil
}
func (f *busyRunnerProcessor) SubmitProcess(ctx context.Context, req *processor.ProcessRequest) (*processor.ProcessAccepted, error) {
	return &processor.ProcessAccepted{ContractVersion: "1.0", JobID: req.JobID, Status: "accepted"}, nil
}
func (f *busyRunnerProcessor) Preflight(ctx context.Context, doc []byte, contentType string) (*processor.PreflightReport, error) {
	return &processor.PreflightReport{}, nil
}
func (f *busyRunnerProcessor) JobStatus(ctx context.Context, jobID string) (*processor.JobStatus, error) {
	return &processor.JobStatus{ContractVersion: "1.0", JobID: jobID, Status: "running", Stage: "extract"}, nil
}
func (f *busyRunnerProcessor) JobResult(ctx context.Context, jobID string) ([]byte, error) {
	return nil, nil
}
func (f *busyRunnerProcessor) Artifact(ctx context.Context, jobID, ref string) ([]byte, error) {
	return nil, nil
}
func (f *busyRunnerProcessor) Cancel(ctx context.Context, jobID string) error { return nil }
func (f *busyRunnerProcessor) Ack(ctx context.Context, jobID string, ack processor.Ack) error {
	return nil
}

// openLiveTopologyDB opens + migrates the scratch DB (custody-IT pattern).
func openLiveTopologyDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping live-topology IT")
	}
	ctx := context.Background()
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

// seedLiveJob seeds source/doc/item/attachment/job for one claimable job
// (the dispatcher harness seeding, expressed over exported SQL).
func seedLiveJob(t *testing.T, d *db.DB, key string) {
	t.Helper()
	ctx := context.Background()
	var srcID, docID, itemID, attID string
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_sources (base_url, library_id, server_id) VALUES ($1,'users/0','srv') RETURNING id::text`,
		"http://live-topology:"+key).Scan(&srcID); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		 VALUES ($1,$2,1,'book','Test Book') RETURNING id::text`, srcID, key).Scan(&docID); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		 VALUES ($1,$2,1,'book',NULL,$3,$4) RETURNING id::text`,
		srcID, key, `{"key":"`+key+`"}`, `{"key":"`+key+`","version":1,"itemType":"book","title":"Test Book"}`).Scan(&itemID); err != nil {
		t.Fatalf("insert canonical item: %v", err)
	}
	if _, err := d.Pool().Exec(ctx, `UPDATE zotero_documents SET canonical_item_id=$2 WHERE id=$1`, docID, itemID); err != nil {
		t.Fatalf("link canonical item: %v", err)
	}
	if err := d.Pool().QueryRow(ctx,
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
		 VALUES ($1,$2,$3,1,$3,'imported_file','application/pdf','x.pdf','/tmp/x.pdf',$4,true,false) RETURNING id::text`,
		srcID, docID, key, "sha256:"+key).Scan(&attID); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	if _, err := d.Pool().Exec(ctx,
		`INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		 VALUES ($1,$2,$3,$4,'pending',3)`, srcID, docID, attID, "sha256:"+key); err != nil {
		t.Fatalf("insert job: %v", err)
	}
}

// TestRunnerLiveEndToEndSingleAgent (#249 acceptance): under the supported
// topology — ONE dispatcher agent whose process ALSO serves the API (#248
// runbook) — a REAL claim loop drives the bus, the deriver folds it, and
// /api/runners/live shows the busy runner with book + stage while the job
// computes.
func TestRunnerLiveEndToEndSingleAgent(t *testing.T) {
	d := openLiveTopologyDB(t)
	ctxSeed := context.Background()
	if _, err := d.Pool().Exec(ctxSeed, `TRUNCATE ingest_jobs, zotero_attachments, zotero_documents,
		zotero_items, zotero_sources CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedLiveJob(t, d, "LV1")
	seedLiveJob(t, d, "LV2")

	broker := events.NewBroker()
	view := events.NewRunnerLive(broker, log.New(io.Discard, "", 0))
	srv := New(":0", log.New(io.Discard, "", 0))
	srv.SetRunnerLive(view)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go view.Start(done)
	view.WaitReady()

	// One agent, two lanes (#248): both claims ride the SAME process's bus.
	disp := dispatcher.New(repo.New(d.Pool()), &busyRunnerProcessor{}, dispatcher.Config{
		RunnerName: "solo-runner", Concurrency: 2, PollInterval: 15 * time.Millisecond,
		RenewalInterval: 25 * time.Millisecond, LeaseDuration: 5 * time.Minute,
		AckRetryInterval: 100 * time.Millisecond,
		Profile:          json.RawMessage(`{"profile":"full-rag-v1"}`),
	}, log.New(io.Discard, "", 0))
	disp.SetEventBroker(broker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = disp.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for {
		resp, err := http.Get(ts.URL + "/api/runners/live")
		if err != nil {
			t.Fatalf("live view: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("live view status %d: %s", resp.StatusCode, body)
		}
		var states []events.RunnerStateChanged
		if err := json.Unmarshal(body, &states); err != nil {
			t.Fatalf("live view body: %v", err)
		}
		for _, st := range states {
			if st.RunnerName == "solo-runner" && st.State == "busy" && st.JobID != "" &&
				st.DocumentTitle != "" && st.Stage != "" {
				return // acceptance met: busy runner, book, stage — live
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("live view never showed the busy runner with book+stage; last snapshot: %s", body)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
