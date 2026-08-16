package processor

// R4 (#134) LIVE ingest-failover proof: a REAL axiom_ng_runner (reference
// compute) accepts and completes a REAL document while the primary runner is
// dead — "jobs laufen lokal weiter". Self-contained: spawns the runner,
// generates a real PDF via the runner venv's pymupdf, drives submit→poll→
// result→ack through the FailoverClient.
//
// Run with:
//   AXIOM_FAILOVER_LIVE=1 \
//   AXIOM_RUNNER_PYTHON=/Users/dudu/Code/axiom/axiom_ng_runner/.venv/bin/python \
//   go test ./internal/processor/ -run TestIT_LiveFailoverRealRunner -v

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestIT_LiveFailoverRealRunner(t *testing.T) {
	py := os.Getenv("AXIOM_RUNNER_PYTHON")
	if os.Getenv("AXIOM_FAILOVER_LIVE") != "1" || py == "" {
		t.Skip("AXIOM_FAILOVER_LIVE=1 + AXIOM_RUNNER_PYTHON=<venv python> required")
	}
	ctx := context.Background()

	// Workdir: PDF source + runner work root.
	dir := t.TempDir()
	pdf := filepath.Join(dir, "live.pdf")
	gen := exec.Command(py, "-c",
		`import pymupdf; d=pymupdf.open(); p=d.new_page(); p.insert_text((72,90),"Failover Live Proof Kapitel",fontsize=12); d.save("`+pdf+`"); d.close()`)
	gen.Env = append(os.Environ(), "PYTHONPATH=")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("pdf gen: %v: %s", err, out)
	}
	sum := sha256.Sum256(mustReadFile(t, pdf))
	st, _ := os.Stat(pdf)

	// Fallback = real local runner on a free port.
	port := freePort(t)
	fallbackURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	run := exec.Command(py, "-m", "axiom_ng_runner")
	run.Dir = mustRepoRoot(t)
	run.Env = append(os.Environ(),
		"AXIOM_PROCESSOR_COMPUTE=reference",
		fmt.Sprintf("AXIOM_PROCESSOR_PORT=%d", port),
		"AXIOM_PROCESSOR_WORK_ROOT="+filepath.Join(dir, "work"),
		"AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS="+dir,
	)
	if err := run.Start(); err != nil {
		t.Fatalf("runner start: %v", err)
	}
	t.Cleanup(func() {
		_ = run.Process.Kill()
		_, _ = run.Process.Wait()
	})
	waitHealthy(t, fallbackURL)
	// Primary = a released port (connection refused).
	dead := freePort(t)
	deadURL := fmt.Sprintf("http://127.0.0.1:%d", dead)

	var logBuf bytes.Buffer
	primary, _ := New(Options{BaseURL: deadURL})
	fb, _ := New(Options{BaseURL: fallbackURL})
	fc := NewFailover(primary, fb, log.New(&logBuf, "", 0))

	req := &ProcessRequest{
		ContractVersion: "1.0",
		JobID:           "live-failover-1",
		IdempotencyKey:  "live-failover-key-1",
		Source:          Source{Type: "zotero", SourceID: "s1", ServerID: "srv"},
		Document: Document{DocumentID: "d1", ZoteroKey: "ZK1", ZoteroVersion: 1,
			MetadataSnapshot: json.RawMessage(`{"title":"Failover Live"}`)},
		Attachment: Attachment{
			AttachmentID: "a1", ZoteroKey: "AK1", ZoteroVersion: 1,
			ContentType: "application/pdf", Filename: "live.pdf", LocalPath: pdf,
			ContentHash: "sha256:" + hex.EncodeToString(sum[:]),
			SizeBytes:   st.Size(),
		},
	}

	acc, err := fc.SubmitProcess(ctx, req)
	if err != nil {
		t.Fatalf("submit via failover: %v", err)
	}
	if acc.Status != "accepted" && acc.Status != "running" {
		t.Fatalf("unexpected accept status %q", acc.Status)
	}

	// Poll to completed (reference compute on one page is fast).
	deadline := time.Now().Add(30 * time.Second)
	for {
		js, err := fc.JobStatus(ctx, req.JobID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if js.Status == "completed" {
			break
		}
		if js.Status == "failed" || time.Now().After(deadline) {
			t.Fatalf("job status %q (err=%v)", js.Status, js.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
	res, err := fc.JobResult(ctx, req.JobID)
	if err != nil || len(res) == 0 {
		t.Fatalf("result: %v (len=%d)", err, len(res))
	}
	if err := fc.Ack(ctx, req.JobID, Ack{Persisted: true}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	if !strings.Contains(logBuf.String(), "ingest failover: primary runner") {
		t.Fatalf("failover not documented: %q", logBuf.String())
	}
	t.Logf("[IT] live failover: real document completed via local runner (%d bytes result); log: %s",
		len(res), strings.TrimSpace(logBuf.String()))
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/processor -> axiom_ng -> repo root
	root := filepath.Dir(filepath.Dir(filepath.Dir(wd)))
	if _, err := os.Stat(filepath.Join(root, "axiom_ng_runner", "__main__.py")); err != nil {
		t.Skipf("axiom_ng_runner not found at %s (run from a full checkout)", root)
	}
	return root
}

func waitHealthy(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := httpGet(base + "/v1/health")
		if err == nil && resp == 200 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner at %s never became healthy: %v", base, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func httpGet(url string) (int, error) {
	cl := &http.Client{Timeout: 2 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
