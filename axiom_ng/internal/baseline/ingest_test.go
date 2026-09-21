// ingest_test.go — full-path ingest probe (#295 Ziel 3): one document
// through Enqueue (force-rebuild API) → Preflight → Dispatch → Runner →
// Snapshot → OS-Commit, against the freeze bits (release RAG + release
// runner env).
//
// The probe rebuilds a small, fixed EPUB document and asserts the outcome
// CLASS: job reaches the terminal success state, exactly one active
// snapshot remains (0011/0019 invariant), the chunk count matches the
// frozen expectation, and the OpenSearch commit agrees (per-doc count).
// checkIngestOutcome is pure and unit-probed: a truncated path (stuck
// job / missing OS commit) must fail it (DoD Mutations-Sonde).
package baseline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
)

type ingestFixture struct {
	DocumentID string `json:"document_id"`
	Expect     struct {
		Chunks         int      `json:"chunks"`
		TagsContain    []string `json:"tags_contain"`
		CitationClass  string   `json:"citation_class"`
		PollTimeoutSec int      `json:"poll_timeout_sec"`
	} `json:"expect"`
}

// ingestOutcome is what the live probe observes; the pure checker turns
// it into pass/fail so the mutation probes can exercise the teeth.
type ingestOutcome struct {
	JobID       string `json:"job_id"`
	JobStatus   string `json:"job_status"`
	SnapshotCnt int    `json:"active_snapshots"`
	ChunkCount  int    `json:"chunks"`
	OSCount     int    `json:"os_count"`
	Tags        string `json:"tags"`
	Citation    string `json:"citation_class"`
}

// checkIngestOutcome validates the full-path outcome class. Every field
// is checked; the returned slices name every violation.
func checkIngestOutcome(o ingestOutcome, f ingestFixture) []string {
	var v []string
	if !strings.EqualFold(o.JobStatus, "completed") {
		v = append(v, fmt.Sprintf("job %s: status %q, want terminal \"completed\"", o.JobID, o.JobStatus))
	}
	if o.SnapshotCnt != 1 {
		v = append(v, fmt.Sprintf("active snapshots = %d, want exactly 1 (0011/0019 invariant)", o.SnapshotCnt))
	}
	if o.ChunkCount != f.Expect.Chunks {
		v = append(v, fmt.Sprintf("chunks = %d, want %d", o.ChunkCount, f.Expect.Chunks))
	}
	if o.OSCount != f.Expect.Chunks {
		v = append(v, fmt.Sprintf("os commit = %d docs for document, want %d (OS-Commit step missing/incomplete)", o.OSCount, f.Expect.Chunks))
	}
	for _, tag := range f.Expect.TagsContain {
		if !strings.Contains(o.Tags, tag) {
			v = append(v, fmt.Sprintf("document tags %q missing %q", o.Tags, tag))
		}
	}
	if f.Expect.CitationClass != "" && o.Citation != f.Expect.CitationClass {
		v = append(v, fmt.Sprintf("citation_class = %q, want %q", o.Citation, f.Expect.CitationClass))
	}
	return v
}

func TestLiveIngestGolden(t *testing.T) {
	liveEnabled(t)
	assertFreezeBits(t)

	var f ingestFixture
	raw, err := os.ReadFile("fixtures/ingest_probe.json")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	timeout := time.Duration(f.Expect.PollTimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	// Enqueue: force-rebuild drives the full path on the existing document.
	// A rebuild may legitimately be in flight (e.g. the dev RAG's boot sync
	// enqueued one) — adopt it: wait for the current job to reach terminal,
	// then run our own. The outcome asserts are idempotent re-run checks.
	var job struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	for attempt := 0; ; attempt++ {
		code := httpJSON(t, "POST",
			ragBase()+"/api/ingest/documents/"+f.DocumentID+"/force-rebuild", nil, &job)
		if code == 202 {
			break
		}
		if code != 409 || attempt >= 2 {
			t.Fatalf("force-rebuild status %d (body %+v)", code, job)
		}
		t.Logf("rebuild in flight (409) — waiting for the current job to finish")
		waitForDocJobTerminal(t, f.DocumentID, timeout)
	}

	// Dispatch → Runner → Snapshot: poll the job listing until terminal.
	deadline := time.Now().Add(timeout)
	status := job.Status
	for time.Now().Before(deadline) {
		var jobs struct {
			Jobs []struct {
				ID     string `json:"ID"`
				Status string `json:"Status"`
			} `json:"jobs"`
		}
		if code := httpJSON(t, "GET", ragBase()+"/api/ingest/jobs?limit=20", nil, &jobs); code != 200 {
			t.Fatalf("jobs status %d", code)
		}
		for _, j := range jobs.Jobs {
			if j.ID == job.ID {
				status = j.Status
			}
		}
		if strings.EqualFold(status, "completed") || strings.EqualFold(status, "failed") ||
			strings.EqualFold(status, "cancelled") || strings.EqualFold(status, "skipped") {
			break
		}
		time.Sleep(3 * time.Second)
	}

	// Outcome class from DB + OS (structure of the freeze path).
	outcome := ingestOutcome{JobID: job.ID, JobStatus: status}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, fingerprintDSN())
	if err != nil {
		t.Fatalf("open dev db: %v (source scripts/dev/env.sh)", err)
	}
	defer d.Close()

	if err := d.Pool().QueryRow(ctx, `
		SELECT count(*) FROM processing_snapshots WHERE document_id=$1 AND active`,
		f.DocumentID).Scan(&outcome.SnapshotCnt); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool().QueryRow(ctx, `
		SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots s ON s.id=c.snapshot_id
		WHERE s.document_id=$1 AND s.active`,
		f.DocumentID).Scan(&outcome.ChunkCount); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool().QueryRow(ctx, `
		SELECT tags::text, citation_class FROM zotero_documents WHERE id=$1`,
		f.DocumentID).Scan(&outcome.Tags, &outcome.Citation); err != nil {
		t.Fatal(err)
	}

	// OS-Commit: the dev index must hold exactly the active chunk set.
	var osResp struct {
		Count int `json:"count"`
	}
	if code := httpJSON(t, "POST",
		osBase()+"/"+osDevIndex()+"/_count",
		map[string]any{"query": map[string]any{"match": map[string]any{"document_id": f.DocumentID}}},
		&osResp); code != 200 {
		t.Fatalf("os count status %d", code)
	}
	outcome.OSCount = osResp.Count

	// goldenCompare: structural outcome class ONLY (the job id is unique
	// per run and would break the two-runs-byte-identical DoD)
	compared := map[string]any{
		"job_status":       outcome.JobStatus,
		"active_snapshots": outcome.SnapshotCnt,
		"chunks":           outcome.ChunkCount,
		"os_count":         outcome.OSCount,
		"tags":             outcome.Tags,
		"citation_class":   outcome.Citation,
	}
	goldenCompare(t, "ingest_outcome.json", compared)
	if v := checkIngestOutcome(outcome, f); len(v) > 0 {
		t.Errorf("ingest probe degraded:\n  %s", strings.Join(v, "\n  "))
	}
}

// waitForDocJobTerminal polls the job listing until the newest job of the
// document reaches a terminal state (completed/failed/cancelled/skipped).
func waitForDocJobTerminal(t *testing.T, documentID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var listing struct {
			Jobs []struct {
				DocumentID string `json:"DocumentID"`
				Status     string `json:"Status"`
			} `json:"jobs"`
		}
		if code := httpJSON(t, "GET", ragBase()+"/api/ingest/jobs?limit=50", nil, &listing); code == 200 {
			// newest first (ListJobs ORDER BY enqueued_at DESC): the document's
			// first listing entry IS the in-flight job — only ITS terminal state
			// counts (old completed jobs must not short-circuit the wait)
			for _, j := range listing.Jobs {
				if j.DocumentID != documentID {
					continue
				}
				s := strings.ToLower(j.Status)
				if s == "completed" || s == "failed" || s == "cancelled" || s == "skipped" {
					return
				}
				break // newest job of the doc is not terminal — keep waiting
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("document %s: job never reached terminal state within %s", documentID, timeout)
}

func osBase() string {
	if v := os.Getenv("AXIOM_OPENSEARCH_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:9200"
}

func osDevIndex() string {
	if v := os.Getenv("AXIOM_OS_INDEX"); v != "" {
		return v
	}
	return "axiom-dev-chunks-v1"
}

// --- Mutations-Sonde: a truncated ingest path must fail the checker -----

func ingestFixtureForProbes() ingestFixture {
	return ingestFixture{DocumentID: "probe-doc", Expect: struct {
		Chunks         int      `json:"chunks"`
		TagsContain    []string `json:"tags_contain"`
		CitationClass  string   `json:"citation_class"`
		PollTimeoutSec int      `json:"poll_timeout_sec"`
	}{Chunks: 3, TagsContain: []string{"KIIN_VL"}, CitationClass: "contextual"}}
}

func healthyIngestOutcome() ingestOutcome {
	return ingestOutcome{
		JobID: "job-1", JobStatus: "completed", SnapshotCnt: 1,
		ChunkCount: 3, OSCount: 3,
		Tags:     `[{"tag": "contextual"}, {"tag": "KIIN_VL"}]`,
		Citation: "contextual",
	}
}

// TestIngestProbeTruncatedOSCommit — DoD: a run whose OS commit never
// landed (OS count 0 while chunks exist) must fail.
func TestIngestProbeTruncatedOSCommit(t *testing.T) {
	o := healthyIngestOutcome()
	o.OSCount = 0 // gekappter Ingest-Schritt: Snapshot ok, OS-Commit fehlt
	if v := checkIngestOutcome(o, ingestFixtureForProbes()); len(v) == 0 {
		t.Fatal("missing OS commit did NOT fail the ingest checker — no teeth")
	}
}

// TestIngestProbeStuckJob — DoD: a job stuck pre-terminal must fail.
func TestIngestProbeStuckJob(t *testing.T) {
	o := healthyIngestOutcome()
	o.JobStatus = "processing" // runner never came back
	if v := checkIngestOutcome(o, ingestFixtureForProbes()); len(v) == 0 {
		t.Fatal("stuck job did NOT fail the ingest checker — no teeth")
	}
}

// TestIngestProbeChunkClassDrift — chunk count drift (chunker change)
// must fail.
func TestIngestProbeChunkClassDrift(t *testing.T) {
	o := healthyIngestOutcome()
	o.ChunkCount = 4 // chunker produced a different class
	if v := checkIngestOutcome(o, ingestFixtureForProbes()); len(v) == 0 {
		t.Fatal("chunk class drift did NOT fail the ingest checker — no teeth")
	}
}
