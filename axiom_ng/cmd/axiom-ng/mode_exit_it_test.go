package main

// #202 witness: every mutating CLI mode flag must TERMINATE after its work —
// never fall through to the server boot (the -bind-all-aliases incident
// class: a mode that keeps running serves a port while the operator believes
// a one-shot pass finished).
//
// Red probe: removing the trailing `return` from any mode block in main.go
// makes its subtest hang until the per-run deadline (the fallen-through mode
// binds the API port and blocks in ListenAndServe) — the witness fails on
// deadline and on the "listening on" log line. AXIOM_ALLOW_DEBUG_BIND=1 plus
// a fresh port make the fall-through BIND instead of dying on the debug-port
// refusal, so the witness catches the class, not the port guard.
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../scratch_test?sslmode=disable \
//   go test ./cmd/axiom-ng/ -run TestIT_ModeFlags -v
import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
)

// TestIT_ModeFlagsExitWithoutServerBoot execs the built binary once per mode
// flag (dry-run where the mode supports it) and asserts: exit code 0, no
// "listening on" in the output, termination well inside the deadline.
func TestIT_ModeFlagsExitWithoutServerBoot(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping mode-exit integration test")
	}
	// Same hard guard as the repo suites: never point this at a non-test DB —
	// -repoint-alias-edges mutates.
	if u, err := url.Parse(dsn); err != nil || !strings.HasSuffix(u.Path, "_test") {
		t.Fatalf("refusing to run against non-test database %q (must end in _test)", dsn)
	}

	// The mode flags query KG tables; make sure the schema exists (idempotent).
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	d.Close()

	bin := filepath.Join(t.TempDir(), "axiom-ng")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	modes := []string{
		"-cleanup-frontmatter-kg",
		"-consolidate-relations",
		"-normalize-entity-types",
		"-bind-all-aliases",
		"-bind-flexion-aliases",
		"-repoint-alias-edges",
		"-consolidate-entities",
	}
	for _, mode := range modes {
		// Both invocation shapes: the dry-run path and the --apply path each
		// own a `return`; a missing one in EITHER is the incident class.
		// (-repoint-alias-edges has no dry-run — it always applies.)
		invocations := [][]string{{mode}}
		if mode != "-repoint-alias-edges" {
			invocations = append(invocations, []string{mode, "--apply"})
		}
		for _, args := range invocations {
			name := strings.Join(args, " ")
			t.Run(name, func(t *testing.T) {
				// Fresh port: a fall-through binds HERE and blocks instead of
				// failing on a busy/refused port.
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("reserve port: %v", err)
				}
				port := ln.Addr().(*net.TCPAddr).Port
				ln.Close()

				runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				c := exec.CommandContext(runCtx, bin, args...)
				c.Env = append(os.Environ(),
					"AXIOM_DATABASE_URL="+dsn,
					"AXIOM_ALLOW_DEBUG_BIND=1",
					fmt.Sprintf("AXIOM_API_PORT=%d", port),
				)
				out, err := c.CombinedOutput()
				if runCtx.Err() == context.DeadlineExceeded {
					t.Fatalf("%s did not terminate within 60s — it fell through to the server boot (#202 incident class)\n%s", name, out)
				}
				if err != nil {
					t.Fatalf("%s exited with error: %v\n%s", name, err, out)
				}
				if strings.Contains(string(out), "listening on") {
					t.Fatalf("%s reached the server boot (must run once and exit)\n%s", name, out)
				}
			})
		}
	}
}
