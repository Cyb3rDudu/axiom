// blank_import_test.go — the #298 import-purity gate: importing any
// internal package must be a pure compile-time act. A package whose init
// spawns persistent goroutines, writes the process environment, or touches
// files (cwd/HOME/tmp) at import time fails here.
//
// Mechanism: every package under internal/ is blank-imported by a compiled
// probe binary run in a sandbox (own cwd, HOME and TMPDIR). The probe
// reports its goroutine count and full environment; the sandbox is checked
// for created files. Detection classes (documented tripwires):
//
//   - persistent goroutine spawns  (a goroutine that exits instantly is
//     invisible — the tripwire catches the leak class, not every race)
//   - env mutation                 (os.Setenv at init)
//   - file creation in cwd/HOME/tmp (import-time file touches)
//
// Red-proof: a temporary init() { go func() { select {} }() } planted in an
// internal package turned this test red (transcript in the #298 comment).
//
// The probe binaries build in a dot-prefixed directory under the module
// root — invisible to ./... patterns, so the probe cannot leak into other
// suites even when a crash interrupts cleanup.
package composition

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// probeTemplate is the per-package probe main. It prints its goroutine
// count and sorted environment; everything else is asserted by the sandbox.
const probeTemplate = `package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"

	_ "%s"
)

func main() {
	env := os.Environ()
	sort.Strings(env)
	fmt.Println("GOROUTINES", runtime.NumGoroutine())
	for _, e := range env {
		fmt.Println("ENV", e)
	}
}
`

// TestBlankImportsHaveNoSideEffects — every internal package, isolated.
func TestBlankImportsHaveNoSideEffects(t *testing.T) {
	if testing.Short() {
		t.Skip("blank-import probes compile ~30 packages; skipped in -short")
	}
	pkgs := goListInternal(t)
	if len(pkgs) < 20 {
		t.Fatalf("suspiciously few internal packages listed (%d) — go list failed?", len(pkgs))
	}

	// The environment the probe binaries are judged against: the test's own
	// env (PWD legitimately differs — the probe runs in the sandbox cwd).
	parentEnv := judgeableEnv(environMap())

	sem := make(chan struct{}, 4) // bounded parallel builds
	var wg sync.WaitGroup
	for _, pkg := range pkgs {
		wg.Add(1)
		go func(pkg string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t.Run(pkg, func(t *testing.T) {
				probeBlankImport(t, pkg, parentEnv)
			})
		}(pkg)
	}
	wg.Wait()
}

func probeBlankImport(t *testing.T, pkg string, parentEnv map[string]string) {
	t.Helper()
	moduleRoot := mustModuleRoot(t)

	// Dot-dir under the module root (per-subtest unique: parallel subtests
	// must not share/removal-race one directory): resolvable imports,
	// invisible to ./... patterns.
	probeDir := filepath.Join(moduleRoot, fmt.Sprintf(".blankprobe-%d", probeSeq.Add(1)))
	src := filepath.Join(probeDir, "main.go")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(probeDir) })
	if err := os.WriteFile(src, []byte(fmt.Sprintf(probeTemplate, pkg)), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(probeDir, "probe.bin")
	build := exec.Command("go", "build", "-o", bin, "./"+filepath.Base(probeDir))
	build.Dir = moduleRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build probe for %s: %v\n%s", pkg, err, out)
	}

	// Sandbox: own cwd + HOME + TMPDIR; the go build cache stays outside.
	sandbox := t.TempDir()
	for _, sub := range []string{"home", "tmp", "cwd"} {
		if err := os.MkdirAll(filepath.Join(sandbox, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gotmp, err := os.MkdirTemp("", "blankprobe-gotmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(gotmp)

	cmd := exec.Command(bin)
	cmd.Dir = filepath.Join(sandbox, "cwd")
	cmd.Env = append(os.Environ(),
		"HOME="+filepath.Join(sandbox, "home"),
		"TMPDIR="+filepath.Join(sandbox, "tmp"),
		"GOTMPDIR="+gotmp,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run probe for %s: %v\n%s", pkg, err, out)
	}

	// Parse GOROUTINES + ENV lines.
	goroutines := -1
	gotEnv := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "GOROUTINES "):
			if _, err := fmt.Sscanf(line, "GOROUTINES %d", &goroutines); err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
		case strings.HasPrefix(line, "ENV "):
			kv := strings.TrimPrefix(line, "ENV ")
			if i := strings.Index(kv, "="); i > 0 {
				gotEnv[kv[:i]] = kv[i+1:]
			}
		}
	}
	if goroutines < 0 {
		t.Fatalf("probe for %s printed no GOROUTINES line:\n%s", pkg, out)
	}
	// A trivial binary runs exactly ONE goroutine (main); the runtime's
	// own service goroutines do not register in NumGoroutine. ANY init-time
	// spawn is therefore a violation — verified against every internal
	// package (the planted-goroutine red-proof transcript is in #298).
	if goroutines > 1 {
		t.Fatalf("%s spawns goroutines at import time: %d goroutines after init (a package import must be a pure compile-time act)", pkg, goroutines)
	}

	// Environment drift beyond PWD (the sandbox cwd).
	got := judgeableEnv(gotEnv)
	drift := envDrift(parentEnv, got)
	if len(drift) > 0 {
		t.Fatalf("%s mutates the process environment at import time: %v", pkg, drift)
	}

	// File touches in the sandbox.
	var touched []string
	for _, sub := range []string{"home", "tmp", "cwd"} {
		entries, err := os.ReadDir(filepath.Join(sandbox, sub))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			touched = append(touched, sub+"/"+e.Name())
		}
	}
	if len(touched) > 0 {
		t.Fatalf("%s creates files at import time (sandbox %s): %v", pkg, sandbox, touched)
	}
}

// probeSeq names a unique probe directory per subtest.
var probeSeq atomic.Int64

// goListInternal returns every library package under internal/ with at
// least one non-test file (test-only packages like internal/contracts
// cannot be blank-imported by a binary — their init class is unreachable
// outside `go test` and has nothing to probe).
func goListInternal(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}} {{len .GoFiles}}", "./internal/...")
	cmd.Dir = mustModuleRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] != "0" {
			pkgs = append(pkgs, parts[0])
		}
	}
	return pkgs
}

// mustModuleRoot resolves the axiom_ng module root (two levels up from
// this package).
func mustModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func environMap() map[string]string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.Index(kv, "="); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// judgeableEnv drops keys that legitimately differ between the parent test
// process and the sandboxed probe.
func judgeableEnv(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		switch k {
		case "PWD", "TMPDIR", "GOTMPDIR", "HOME", "_":
			continue
		default:
			out[k] = v
		}
	}
	return out
}

func envDrift(want, got map[string]string) []string {
	var drift []string
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if w, ok := want[k]; !ok {
			drift = append(drift, "+"+k+"="+got[k])
		} else if w != got[k] {
			drift = append(drift, k+": "+w+" -> "+got[k])
		}
	}
	for k, w := range want {
		if _, ok := got[k]; !ok {
			drift = append(drift, "-"+k+"="+w)
		}
	}
	return drift
}
