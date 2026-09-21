// live_test.go — live golden probes against the dev environment in
// --release mode (#295 Ziele 2+3). The freeze bits answer on :8111/:8112;
// every probe asserts against committed fixtures, so a 0.2.0 step that
// changes observable behavior goes red until the fixture update lands in
// the same, review-visible PR.
package baseline

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ragBase() string {
	if v := os.Getenv("AXIOM_BASELINE_RAG"); v != "" {
		return v
	}
	return "http://127.0.0.1:8111"
}

func runnerBase() string {
	if v := os.Getenv("AXIOM_BASELINE_RUNNER"); v != "" {
		return v
	}
	return "http://127.0.0.1:8112"
}

// stateDir is the dev environment state root (~/.local/state/axiom-dev).
func stateDir() string {
	if v := os.Getenv("AXIOM_BASELINE_STATE"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "axiom-dev")
}

// httpJSON issues a request and decodes a JSON response. Connection-level
// failures are FATAL (the caller decides whether a refused connection is
// expected — use httpMaybeJSON inside warmup/wait loops).
func httpJSON(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	return httpMaybeJSON(t, method, url, body, out, false)
}

// httpMaybeJSON: same contract, but tolerate=true turns transport errors
// into status 0 (for retry loops against a warming process).
func httpMaybeJSON(t *testing.T, method, url string, body any, out any, tolerate bool) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if tolerate {
			return 0
		}
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: bad json %q: %v", method, url, string(raw[:min(len(raw), 300)]), err)
		}
	}
	return resp.StatusCode
}

// assertFreezeBits is the FIRST live assertion everywhere: the dev RAG
// must serve from the frozen release binary, never a working-tree build.
func assertFreezeBits(t *testing.T) {
	t.Helper()
	var health struct {
		OK         bool           `json:"ok"`
		Build      string         `json:"build"`
		Checks     map[string]any `json:"checks"`
		Contextual string         `json:"contextual"`
	}
	if code := httpJSON(t, "GET", ragBase()+"/api/health", nil, &health); code != 200 {
		t.Fatalf("health status %d", code)
	}
	if health.Build != FreezeRAGBuild {
		t.Fatalf("dev RAG is not serving the freeze bits:\n got: %s\nwant: %s\n(start with scripts/dev/dev-up.sh --release)",
			health.Build, FreezeRAGBuild)
	}
}

// requireReleaseMode fails unless the dev env state dir records release
// mode — golden probes must witness the freeze bits, never working-tree
// builds. The RAG-side guard is assertFreezeBits (build banner); this is
// the env-side guard (mode file), needed where the probe talks to the
// runner or spawns a second RAG instance.
func requireReleaseMode(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(string(mustReadFile(t, filepath.Join(stateDir(), "mode")))) != "release" {
		t.Fatalf("dev env mode file is not 'release' — run scripts/dev/dev-up.sh --release first")
	}
}

// TestLiveGoldenHealth — /api/health snapshot (Ziel 2): every check name
// and value, the build banner, the contextual state. Canonicalized (the
// checks map iterates randomly at the source) so two runs are
// byte-identical.
func TestLiveGoldenHealth(t *testing.T) {
	liveEnabled(t)
	assertFreezeBits(t) // also proves the banner, the hard freeze-bits guard

	// quiescence gate: right after dev-up the boot contextual sync may
	// still be settling — snapshotting a transiently degraded health would
	// false-red the gate. Wait (bounded) for the steady state instead.
	deadline := time.Now().Add(60 * time.Second)
	for {
		var probe struct {
			Contextual string `json:"contextual"`
		}
		if code := httpJSON(t, "GET", ragBase()+"/api/health", nil, &probe); code == 200 && probe.Contextual == "active" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dev RAG health never reached contextual=active within 60s — env still settling or genuinely degraded")
		}
		time.Sleep(2 * time.Second)
	}

	var health map[string]any
	if code := httpJSON(t, "GET", ragBase()+"/api/health", nil, &health); code != 200 {
		t.Fatalf("health status %d", code)
	}
	goldenCompare(t, "health.json", health)
}

// TestLiveGoldenCapabilities — runner /v1/capabilities snapshot (Ziel 2):
// contract versions, processor identity, formats, every feature flag,
// model stack, limits. The runner release env (not the source venv) must
// answer — guaranteed by dev-up --release + the warm check below.
func TestLiveGoldenCapabilities(t *testing.T) {
	liveEnabled(t)
	requireReleaseMode(t) // the release runner env must answer, not the source venv

	var warm struct {
		Status       string `json:"status"`
		ModelsWarmed bool   `json:"models_warmed"`
	}
	if code := httpJSON(t, "GET", runnerBase()+"/v1/health", nil, &warm); code != 200 {
		t.Fatalf("runner health status %d", code)
	}
	if !warm.ModelsWarmed {
		t.Fatal("runner models not warmed — capabilities would snapshot a mid-warmup state")
	}

	var caps map[string]any
	if code := httpJSON(t, "GET", runnerBase()+"/v1/capabilities", nil, &caps); code != 200 {
		t.Fatalf("capabilities status %d", code)
	}
	goldenCompare(t, "capabilities.json", caps)
}
