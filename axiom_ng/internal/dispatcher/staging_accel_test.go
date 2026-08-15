package dispatcher

// L8 staging-acceleration tests: bounded-parallel artifact fetches and
// per-call-type client timeouts.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

// artifactServer serves refs with a per-request delay; corrupt one ref's bytes
// when asked (digest gate must still fire under parallelism).
type artifactServer struct {
	srv   *httptest.Server
	delay time.Duration
}

func newArtifactServer(t *testing.T, refs map[string][]byte, delay time.Duration, corrupt string) *artifactServer {
	t.Helper()
	a := &artifactServer{delay: delay}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := strings.TrimPrefix(r.URL.Path, "/v1/jobs/j1/artifacts/")
		b, ok := refs[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if ref == corrupt {
			b = []byte("xxx") // SAME size as "ddd" — the DIGEST gate must fire
		}
		time.Sleep(delay)
		w.Write(b)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func newStagingDispatcher(t *testing.T, baseURL string) *Dispatcher {
	d := New(nil, mustClient(t, baseURL), Config{
		ArtifactRoot: t.TempDir(),
	}, testLogger())
	return d
}

func resultWithArtifacts(refs map[string][]byte) []byte {
	type art struct {
		Ref       string `json:"ref"`
		Kind      string `json:"kind"`
		MediaType string `json:"media_type"`
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"size_bytes"`
		Retention string `json:"retention"`
	}
	out := struct {
		Artifacts []art `json:"artifacts"`
	}{}
	// Deterministic declaration order: sorted refs.
	names := make([]string, 0, len(refs))
	for r := range refs {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		sum := sha256.Sum256(refs[r])
		out.Artifacts = append(out.Artifacts, art{
			Ref: r, Kind: "image", MediaType: "image/png",
			SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(refs[r])),
			Retention: "durable",
		})
	}
	b, _ := json.Marshal(out)
	return b
}

func TestStageArtifactsParallelIsFasterThanSerial(t *testing.T) {
	refs := map[string][]byte{}
	for i := 0; i < 12; i++ {
		refs[fmt.Sprintf("image-%04d", i)] = []byte{byte(i)}
	}
	srv := newArtifactServer(t, refs, 250*time.Millisecond, "")
	d := newStagingDispatcher(t, srv.srv.URL)

	start := time.Now()
	recs, err := d.stageArtifacts(context.Background(), "j1", resultWithArtifacts(refs))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("stageArtifacts: %v", err)
	}
	if len(recs) != 12 {
		t.Fatalf("records = %d, want 12", len(recs))
	}
	// Serial floor: 12 × 250ms = 3s. Parallel(6) ≈ 2 waves ≈ 0.5–0.8s.
	// Assert well under half the serial floor.
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("staging took %v — not parallel (serial floor 3s)", elapsed)
	}
	// Deterministic order preserved (sorted refs).
	for i, r := range recs {
		want := fmt.Sprintf("image-%04d", i)
		if r.Ref != want {
			t.Fatalf("rec[%d].Ref = %q, want %q (declaration order must survive)", i, r.Ref, want)
		}
	}
}

func TestStageArtifactsCorruptDigestStillTerminal(t *testing.T) {
	refs := map[string][]byte{
		"a": []byte("aaa"), "b": []byte("bbb"), "c": []byte("ccc"),
		"d": []byte("ddd"), "e": []byte("eee"), "f": []byte("fff"),
		"g": []byte("ggg"),
	}
	srv := newArtifactServer(t, refs, 50*time.Millisecond, "d")
	d := newStagingDispatcher(t, srv.srv.URL)

	_, err := d.stageArtifacts(context.Background(), "j1", resultWithArtifacts(refs))
	if err == nil {
		t.Fatal("corrupted artifact must fail the whole job")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("err = %v, want the DIGEST branch (same-size wrong bytes)", err)
	}
	// No partial commit: no FINAL file for the corrupt ref (staging cleaned),
	// and the job dir only holds successfully renamed files.
	root := d.cfg.ArtifactRoot
	filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if strings.HasSuffix(p, ".staging") {
			t.Errorf("staging leftover: %s", p)
		}
		return nil
	})
}

func TestStatusPollTimesOutAtPerCallBudget(t *testing.T) {
	// The poll budget (10s) must fire long before the old shared 300s window.
	// The handler parks on a done channel so cleanup does not wait out the
	// full fake hang (httptest Close blocks on in-flight handlers).
	done := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(60 * time.Second):
		case <-done:
		}
	}))
	t.Cleanup(func() { close(done); hang.Close() })
	client, err := processor.New(processor.Options{BaseURL: hang.URL})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, jerr := client.JobStatus(context.Background(), "j1")
	elapsed := time.Since(start)
	if jerr == nil {
		t.Fatal("hanging status endpoint must error")
	}
	if elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Fatalf("JobStatus errored after %v, want ~10s per-call budget (not the shared 300s)", elapsed)
	}
}

func TestJobResultTimesOutAtConfiguredBudget(t *testing.T) {
	// W7: the resultBudget wiring must actually be pinned to
	// Options.ResultTimeout, not a constant. Handler parks on done so
	// cleanup does not wait out the fake hang.
	done := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(60 * time.Second):
		case <-done:
		}
	}))
	t.Cleanup(func() { close(done); hang.Close() })
	client, err := processor.New(processor.Options{BaseURL: hang.URL, ResultTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, jerr := client.JobResult(context.Background(), "j1")
	elapsed := time.Since(start)
	if jerr == nil {
		t.Fatal("hanging result endpoint must error")
	}
	if elapsed < 1500*time.Millisecond || elapsed > 3500*time.Millisecond {
		t.Fatalf("JobResult errored after %v, want ~2s configured budget", elapsed)
	}
}
