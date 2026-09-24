// alias_it_test.go — F05 #299 binary witnesses for the axiom-ng
// compatibility alias and the canonical axiom binary. These exec the
// BUILT binaries (the composition package's TestBinaryExitsNonZero*
// pattern): the deprecation count sonde (exactly one warning line per
// process), the health deprecations counter (the F02 mechanism made
// visible by the first real in-process caller), the readiness field
// through the real serve path, and the graceful SIGTERM exit (real SIGTERM, and a cleanup that actually kills a never-exited child).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// buildBinary builds the named command into a temp path and returns it.
func buildBinary(t *testing.T, pkg string, name string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestIT_AliasWarnsExactlyOnce — the #299 DoD sonde: an axiom-ng process
// emits the deprecation warning EXACTLY ONCE (the F02 hook's
// once-per-process semantics through the real alias main). The canonical
// axiom binary emits none.
func TestIT_AliasWarnsExactlyOnce(t *testing.T) {
	alias := buildBinary(t, ".", "axiom-ng")
	out := runCapture(t, alias, "--version")
	if n := strings.Count(out, `deprecated name "axiom-ng" used`); n != 1 {
		t.Fatalf("axiom-ng --version must emit exactly one deprecation warning, got %d:\n%s", n, out)
	}

	canonical := buildBinary(t, "../axiom", "axiom")
	out = runCapture(t, canonical, "version")
	if n := strings.Count(out, `deprecated name`); n != 0 {
		t.Fatalf("axiom version must not warn, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "axiom-ng") {
		// the banner line stays the single identity line (agrees with
		// /api/health, #205) — it must print, whatever its prefix
		t.Fatalf("version must print the banner, got:\n%s", out)
	}
}

// runCapture runs bin with args and returns combined output (waiting for
// exit; commands used here all terminate).
func runCapture(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		if _, alive := err.(*exec.ExitError); !alive {
			t.Fatalf("run %s %v: %v\n%s", bin, args, err, out)
		}
	}
	return string(out)
}

// TestIT_AliasHealthCounterAndReadiness — the running alias serves
// /api/health with the deprecations counter visible ({"axiom-ng":1} —
// the N1-consistent runtime shape; the working-tree golden's {} pin
// derives from alias-free processes) and the F05 readiness field through
// the real serve path; SIGTERM exits 0 (graceful stop, no zombie).
func TestIT_AliasHealthCounterAndReadiness(t *testing.T) {
	alias := buildBinary(t, ".", "axiom-ng")
	port := freePort(t)

	cmd := exec.Command(alias) // no-arg = the compat full boot, db-less → api-only
	cmd.Env = append(os.Environ(),
		"AXIOM_DATABASE_URL=",
		"AXIOM_ZOTERO_BASE=http://127.0.0.1:9", // fast refuse: the zotero check reports unhealthy, the boot proceeds
		fmt.Sprintf("AXIOM_API_PORT=%d", port),
		"AXIOM_BIND_ADDR=127.0.0.1",
		"AXIOM_ALLOW_DEBUG_BIND=1", // debug build; port may fall into the guard range
	)
	var buf safeBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Track the real exit: ProcessState is nil until Wait, so a deferred
	// ProcessState guard can never see a RUNNING process — kill on
	// "Wait never happened" instead (review round 3: the inverted guard
	// leaked live test children).
	exited := make(chan struct{})
	defer func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Kill()
		}
	}()

	// Poll for readiness, not just the first 200: the per-role Ready
	// goroutines run after the start loop, so a first-response assert
	// races "starting" (composition_test.go polls for the same reason).
	deadline := time.Now().Add(20 * time.Second)
	var health healthShape
	for time.Now().Before(deadline) {
		health = pollHealthOnce(t, port)
		if health.Readiness["api"] == "ready" && health.Deprecations["axiom-ng"] == 1 {
			break
		}
		if !isProcAlive(cmd.Process) {
			t.Fatalf("alias exited early (log:\n%s)", buf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if health.Deprecations["axiom-ng"] != 1 {
		t.Fatalf("alias health must count axiom-ng:1, got %v (log:\n%s)", health.Deprecations, buf.String())
	}
	if health.Readiness["api"] != "ready" {
		t.Fatalf("alias health must expose the readiness aggregation, got %v", health.Readiness)
	}
	// The v1 edge serves through the running alias too.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port))
	if err != nil {
		t.Fatalf("v1 health: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"canonical_name"`) {
		t.Fatalf("v1 health through the alias: %d %s", resp.StatusCode, raw)
	}

	// Graceful SIGTERM (the documented signal; SIGINT shares the same
	// NotifyContext wiring): exit 0, exactly one warning line overall.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		close(exited)
		if err != nil {
			t.Fatalf("graceful shutdown must exit 0, got %v (log:\n%s)", err, buf.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("alias did not exit after SIGTERM (zombie)")
	}
	if n := strings.Count(buf.String(), `deprecated name "axiom-ng" used`); n != 1 {
		t.Fatalf("whole-process warning count = %d, want exactly 1:\n%s", n, buf.String())
	}
}

// healthShape is the projection these ITs need from /api/health.
type healthShape struct {
	Deprecations map[string]int    `json:"deprecations"`
	Readiness    map[string]string `json:"readiness"`
}

// pollHealthOnce reads /api/health; a transport error returns the zero
// shape (the caller retries).
func pollHealthOnce(t *testing.T, port int) healthShape {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", port))
	if err != nil {
		return healthShape{}
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return healthShape{}
	}
	var h healthShape
	if json.Unmarshal(raw, &h) != nil {
		return healthShape{}
	}
	return h
}

// isProcAlive reports whether the process has not exited yet.
func isProcAlive(p *os.Process) bool {
	return p.Signal(syscall.Signal(0)) == nil
}

// safeBuffer is a mutex-guarded buffer (the process writes from several
// goroutines; the test reads it in failure messages).
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
