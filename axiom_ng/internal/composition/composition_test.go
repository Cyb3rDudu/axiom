// composition_test.go — CI-runnable lifecycle witnesses (no DB needed).
// DB-backed registry tests live in composition_it_test.go.
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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
)

func testLogger() *log.Logger { return log.New(os.Stderr, "composition-test: ", log.LstdFlags) }

func apiOnlyCfg(port int) config.Config {
	cfg := config.Load()
	cfg.DatabaseURL = ""
	cfg.APIPort = port
	cfg.BindAddr = "127.0.0.1"
	// Neutralize worker opt-ins the surrounding shell may export (the dev
	// env sets the fixer env): the db-less unit tests exercise api-only.
	cfg.DispatcherEnabled = false
	cfg.FixerInvokerEnabled = false
	return cfg
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

// TestSelectRejectsMissingDependency — the loud selective-start contract
// (#298 DoD): selecting a role WITHOUT its dependencies fails with a
// diagnosis naming the role and its missing port, before anything started.
// Leaf roles (events/search/repair/dispatcher) are droppable by design —
// the companion case proves that.
func TestSelectRejectsMissingDependency(t *testing.T) {
	cases := []struct{ role, missing Role }{
		{RoleDispatcher, RoleStore},
		{RoleSync, RoleStore},
		{RoleEvents, RoleStore},
		{RoleSearch, RoleStore},
		{RoleIngest, RoleStore},
		{RoleRepair, RoleStore},
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"-without-"+string(tc.missing), func(t *testing.T) {
			cfg := apiOnlyCfg(0)
			cfg.DatabaseURL = "postgres://invalid" // store WOULD be startable
			_, err := Select(cfg, testLogger(), Ports{}, tc.role)
			if err == nil {
				t.Fatalf("selecting %q alone must fail (needs %q)", tc.role, tc.missing)
			}
			if !strings.Contains(err.Error(), string(tc.role)) || !strings.Contains(err.Error(), string(tc.missing)) {
				t.Fatalf("diagnosis must name role %q and missing port %q, got: %v", tc.role, tc.missing, err)
			}
		})
	}
	// Droppable leaves: api+store alone is a valid (if minimal) selection.
	cfg := apiOnlyCfg(0)
	cfg.DatabaseURL = "postgres://invalid"
	if _, err := Select(cfg, testLogger(), Ports{}, RoleAPI, RoleStore); err != nil {
		t.Fatalf("api+store must be selectable (deps satisfied), got: %v", err)
	}
}

// TestSelectRejectsStoreWithoutDSN — selecting store with no database URL
// is a configuration error, not a degrade.
func TestSelectRejectsStoreWithoutDSN(t *testing.T) {
	_, err := Select(apiOnlyCfg(0), testLogger(), Ports{}, RoleStore)
	if err == nil || !strings.Contains(err.Error(), "AXIOM_DATABASE_URL") {
		t.Fatalf("store without DSN must fail with the DSN diagnosis, got: %v", err)
	}
}

// TestSelectRejectsHalfWiredWorkers — the deliberate F04 delta: an enabled
// worker (dispatcher/fixer env) without its storage port refuses to boot
// loudly instead of silently never claiming. Pre-F04 this degraded quietly.
func TestSelectRejectsHalfWiredWorkers(t *testing.T) {
	for _, tc := range []struct {
		set  func(*config.Config)
		env  string
		role Role
	}{
		{func(c *config.Config) { c.DispatcherEnabled = true }, "AXIOM_DISPATCHER_ENABLED", RoleDispatcher},
		{func(c *config.Config) { c.FixerInvokerEnabled = true }, "AXIOM_FIXER_INVOKER_ENABLED", RoleRepair},
	} {
		t.Run(tc.env, func(t *testing.T) {
			cfg := apiOnlyCfg(0)
			tc.set(&cfg)
			// Even an explicit api-only selection must refuse: the operator
			// asked for a worker the roles then dropped.
			_, err := Select(cfg, testLogger(), Ports{}, RoleAPI)
			if err == nil || !strings.Contains(err.Error(), tc.env) || !strings.Contains(err.Error(), "half-wired") {
				t.Fatalf("half-wiring must fail naming %s, got: %v", tc.env, err)
			}
			// And the config-derived Full path likewise.
			_, err = Full(cfg, testLogger(), Ports{})
			if err == nil || !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("Full with %s set and no DSN must fail, got: %v", tc.env, err)
			}
		})
	}
}

func TestSelectRejectsUnknownRole(t *testing.T) {
	_, err := Select(apiOnlyCfg(0), testLogger(), Ports{}, Role("bogus"))
	if err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("unknown role must fail, got: %v", err)
	}
}

// TestAPIOnlyLifecycleAndQuietStop — the db-less composition boots exactly
// the api role, serves the unchanged health shape, stops on demand, and
// leaves the process internally quiet (the goleak-equivalent witness).
func TestAPIOnlyLifecycleAndQuietStop(t *testing.T) {
	port := freePort(t)
	cfg := apiOnlyCfg(port)
	root, err := Full(cfg, testLogger(), Ports{})
	if err != nil {
		t.Fatal(err)
	}
	if got := root.Roles(); len(got) != 1 || got[0] != string(RoleAPI) {
		t.Fatalf("db-less config must select api-only, got %v", got)
	}

	base := goroutineBaseline(t)
	sigCtx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := root.Start(sigCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := root.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", port))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var health struct {
		OK            bool              `json:"ok"`
		CanonicalName string            `json:"canonical_name"`
		ServiceClass  string            `json:"service_class"`
		Checks        map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("health json: %v\n%s", err, body)
	}
	// Baseline protection: the frozen identity fields and the health shape
	// are byte-stable; only the zotero check exists db-less.
	if health.CanonicalName != "axiom" || health.ServiceClass != "api+library+store" {
		t.Fatalf("health identity fields changed: %s", body)
	}
	if _, hasPG := health.Checks["postgres"]; hasPG {
		t.Fatalf("db-less health must not report postgres, got: %s", body)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root.Stop(stopCtx)

	// The listener is gone.
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond); err == nil {
		t.Fatal("listener still open after Stop")
	}
	assertGoroutinesSettled(t, base, 5*time.Second)
}

// TestStartTwiceRejected — lifecycle discipline.
func TestStartTwiceRejected(t *testing.T) {
	root, err := Full(apiOnlyCfg(freePort(t)), testLogger(), Ports{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := root.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer root.Stop(context.Background())
	if err := root.Start(ctx); err == nil {
		t.Fatal("second Start must fail")
	}
}

// TestStartAbortsLoudlyOnBindConflict — a failing LAST component (http bind)
// surfaces the error (exit-≠-0 class for the caller) and the abort path
// runs; the in-process zombie-freedom of a partial start is witnessed by
// TestIT_StoppedStartLeavesNoGoroutines (DB-backed, mid-sequence failure).
func TestStartAbortsLoudlyOnBindConflict(t *testing.T) {
	port := freePort(t)
	// Hold the port with a dummy listener.
	hold, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()

	root, err := Full(apiOnlyCfg(port), testLogger(), Ports{})
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
		t.Fatal("start on a bound port must fail")
	}
	if !strings.Contains(startErr.Error(), "http server") {
		t.Fatalf("bind failure must carry the http server diagnosis, got: %v", startErr)
	}
	assertGoroutinesSettled(t, base, 5*time.Second)
}

// TestBinaryExitsNonZeroOnHalfWiring — the entrypoint-level probe of the
// selective-start DoD: exit ≠ 0, a diagnosis line naming the env var, and
// the process actually terminated (no zombie).
func TestBinaryExitsNonZeroOnHalfWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("binary build skipped in -short mode")
	}
	bin := filepath.Join(t.TempDir(), "axiom-ng")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/axiom-ng").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"AXIOM_DISPATCHER_ENABLED=1",
		"AXIOM_DATABASE_URL=",
		fmt.Sprintf("AXIOM_API_PORT=%d", freePort(t)),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("half-wired boot must exit non-zero, output:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit code, got: %v", err)
	}
	if err := assertProcessExited(cmd.ProcessState, "axiom-ng"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "half-wired") || !strings.Contains(string(out), "AXIOM_DISPATCHER_ENABLED") {
		t.Fatalf("diagnosis must name the env var and the half-wiring refusal, got:\n%s", out)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// TestRolesFromConfigKeepsRepairWithoutInvoker — baseline-protection
// witness for the repair surface: the repair ROLE is selected whenever the
// store is (the write-key gate and the invoker opt-in live INSIDE the
// component). Pre-F04, /api/repair/* was served whenever DB + write key
// existed, with AXIOM_FIXER_INVOKER_ENABLED=0; gating the role on the env
// would silently drop that surface. Select opens nothing — a fake DSN is
// enough.
func TestRolesFromConfigKeepsRepairWithoutInvoker(t *testing.T) {
	cfg := apiOnlyCfg(0)
	cfg.DatabaseURL = "postgres://roles-probe.invalid/roles_test"
	cfg.FixerInvokerEnabled = false
	cfg.DispatcherEnabled = false
	root, err := Full(cfg, testLogger(), Ports{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"store", "events", "sync", "repair", "search", "ingest", "api"}
	got := root.Roles()
	if len(got) != len(want) {
		t.Fatalf("roles with invoker off must still include repair, got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roles with invoker off must still include repair, got %v want %v", got, want)
		}
	}
}

// TestSelectWorkerEnvPrecedenceNote — explicit role selection wins over
// worker opt-in envs, but never silently (#298): Select logs the drop,
// Full (which derives its roles from the config) does not. No Start —
// construction is enough for the log assertions; Select never opens the DB.
func TestSelectWorkerEnvPrecedenceNote(t *testing.T) {
	cfg := apiOnlyCfg(0)
	cfg.DatabaseURL = "postgres://invalid" // store startable, never opened
	cfg.DispatcherEnabled = true

	// Explicit Select keeps the store but drops the dispatcher role.
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	if _, err := Select(cfg, logger, Ports{}, RoleAPI, RoleStore); err != nil {
		t.Fatalf("select api+store must succeed, got: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "AXIOM_DISPATCHER_ENABLED") {
		t.Fatalf("Select must log the dropped env-enabled worker, got log: %q", out)
	}

	// Full derives the dispatcher role from the same env — no note.
	buf.Reset()
	if _, err := Full(cfg, log.New(&buf, "", 0), Ports{}); err != nil {
		t.Fatalf("Full must succeed, got: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "AXIOM_DISPATCHER_ENABLED=1 but") {
		t.Fatalf("Full derives roles from config, must not log the precedence note, got log: %q", out)
	}
}

// TestReadinessPublicInHealth — F05 #299: the aggregated internal readiness
// is publicly visible in /api/health ("ready" once the component's internal
// signal fired) and flips to "stopping" when the shutdown begins (witnessed
// on the snapshot directly — the listener is gone by then).
func TestReadinessPublicInHealth(t *testing.T) {
	port := freePort(t)
	root, err := Full(apiOnlyCfg(port), testLogger(), Ports{})
	if err != nil {
		t.Fatal(err)
	}
	sigCtx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := root.Start(sigCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer root.Stop(context.Background())

	// The api component's Ready is synchronous — the flag flips via the
	// post-Start goroutine; poll briefly for it.
	deadline := time.Now().Add(3 * time.Second)
	var readiness map[string]string
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", port))
		if err == nil {
			var health struct {
				Readiness map[string]string `json:"readiness"`
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if json.Unmarshal(body, &health) == nil {
				readiness = health.Readiness
				if readiness["api"] == "ready" {
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if readiness["api"] != "ready" {
		t.Fatalf("health readiness must show api ready, got %v", readiness)
	}
	if len(readiness) != 1 {
		t.Fatalf("api-only composition must report exactly one readiness role, got %v", readiness)
	}
	// The v1 mirror serves the same aggregated field (parity by construction).
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port))
	if err != nil {
		t.Fatalf("v1 health: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var v1 struct {
		Readiness map[string]string `json:"readiness"`
	}
	if err := json.Unmarshal(body, &v1); err != nil || v1.Readiness["api"] != "ready" {
		t.Fatalf("v1 health readiness diverged: %s", body)
	}

	root.Stop(context.Background())
	if got := root.readinessSnapshot()["api"]; got != "stopping" {
		t.Fatalf("post-stop snapshot must read stopping, got %q", got)
	}
}

// TestStopParallelSubBudgets — the F04 deferral landed in F05 (#299): the
// stop joins (api drain, dispatcher, fixer, sync) run IN PARALLEL with
// per-join budgets. A deliberately slow drain must NOT block the other
// joins: under the pre-F05 serialized stop the fast join could only run
// AFTER the slow drain (reverse component order puts the dispatcher
// first); in parallel it completes while the slow one is still draining.
// The store join still lands LAST (the pool closes after every other
// join settled or expired its budget).
func TestStopParallelSubBudgets(t *testing.T) {
	root, err := Full(apiOnlyCfg(freePort(t)), testLogger(), Ports{})
	if err != nil {
		t.Fatal(err)
	}
	const slowDrain = 400 * time.Millisecond
	var fastAt, storeAt time.Duration
	var mu sync.Mutex
	t0 := time.Now()

	// Component order matters for the serialization probe: reverse
	// iteration must hit the slow dispatcher drain BEFORE the fast join,
	// so only parallelism (not list order) can let the fast join through.
	root.components = []Component{
		funcComponent{name: "store", role: RoleStore, stop: func(ctx context.Context) error {
			mu.Lock()
			storeAt = time.Since(t0)
			mu.Unlock()
			return nil
		}},
		funcComponent{name: "fast-join", role: RoleRepair, stop: func(ctx context.Context) error {
			mu.Lock()
			fastAt = time.Since(t0)
			mu.Unlock()
			return nil
		}},
		funcComponent{name: "slow-drain", role: RoleDispatcher, stop: func(ctx context.Context) error {
			select {
			case <-time.After(slowDrain): // a drain that uses its whole budget
			case <-ctx.Done():
			}
			return nil
		}},
		funcComponent{name: "api", role: RoleAPI, stop: func(ctx context.Context) error { return nil }},
	}
	root.roles = map[Role]bool{RoleAPI: true, RoleStore: true, RoleDispatcher: true, RoleRepair: true}
	root.started.Store(true)
	root.cancel = func() {} // Start never ran; Stop's cancel is a no-op here

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root.Stop(stopCtx)
	total := time.Since(t0)

	mu.Lock()
	fast, store := fastAt, storeAt
	mu.Unlock()
	if fast >= slowDrain-100*time.Millisecond {
		t.Fatalf("fast join blocked by the slow drain: finished at %v (slow drain %v) — joins are not parallel", fast, slowDrain)
	}
	if store < slowDrain-100*time.Millisecond {
		t.Fatalf("store join must land after the slow drain (pool closes last), finished at %v", store)
	}
	if total < slowDrain-100*time.Millisecond {
		t.Fatalf("total stop %v cannot be below the slowest join %v", total, slowDrain)
	}
	if total > 5*time.Second {
		t.Fatalf("total stop %v far above the slowest join — a join overran its own budget", total)
	}
}
