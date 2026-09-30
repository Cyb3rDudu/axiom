package cli

// split_repair_it_test.go — F14 #308: the repair kill sonde in the SPLIT
// topology shape. The library-role selection (exactly what `axiom serve
// library` boots) owns the repair orchestrator; the worker child is a
// REAL OS process (the LocalExecutor spawns the configured command) and
// dies mid-case by SIGKILL — the case must stay retryable (attempt
// burned, no zombie in_repair lease) and the recovery run must heal it
// through the same loop, including the custody apply (a fake Zotero
// witnesses the upload). The all-in-one leg of the sonde is
// library/repair's TestWorkerCrashMidRepairLeavesCaseRetryableNoZombieLease
// (the same invoker/executor the all-in-one process runs, DB-gated like
// this one).

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/composition"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/repair"
)

// repairFakeZotero implements the surface the library-role composition
// touches during the sonde: the ServerID probe (health + write client
// identity), the write path the custody apply drives (item version GET,
// DELETE, item create, file authorize), and item reads that answer an
// EMPTY page (the post-heal targeted sync completes without candidates).
type repairFakeZotero struct {
	srv     *httptest.Server
	mu      sync.Mutex
	created int
}

func newRepairFakeZotero(t *testing.T) *repairFakeZotero {
	t.Helper()
	f := &repairFakeZotero{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Zotero-Server-ID", "split-repair-sonde")
		w.Header().Set("Last-Modified-Version", "42")
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items"):
			f.mu.Lock()
			f.created++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"successful":{"0":{"key":"SPLITNEW1"}},"unchanged":{},"failed":{}}`)
		case r.Method == http.MethodPost: // file authorize: staged-identical path
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"exists":1}`)
		default: // GET: ServerID probe + empty item pages
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[]`)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *repairFakeZotero) creations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// TestIT_SplitLibraryRoleRepairKillSonde — the F14 split ride: boot the
// library-role composition with the invoker armed, seed one repair case,
// let the worker child SIGKILL itself mid-case, and require the retryable
// crash semantics plus the COMPLETE recovery (healed, custody applied).
func TestIT_SplitLibraryRoleRepairKillSonde(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping split repair sonde")
	}
	ctx := context.Background()
	// harness guard: never run against a non-test database
	if u, perr := url.Parse(dsn); perr == nil && !strings.Contains(u.Path, "test") {
		t.Fatalf("refusing to run against non-test database %q", u.Path)
	}
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(d.Close)
	pool := d.Pool()
	if _, err := pool.Exec(ctx, `TRUNCATE repair_cases, zotero_write_audit,
		zotero_attachments, zotero_documents, zotero_sources CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	fake := newRepairFakeZotero(t)

	// Fake HOME: the invoker's default WorkRoot derives from it, and the
	// worker script writes its healed artifact under the same root.
	home := t.TempDir()
	t.Setenv("HOME", home)
	workRoot := filepath.Join(home, ".local", "state", "axiom", "runs")

	// The sonde worker: run 1 dies mid-case (SIGKILL, exactly the F08
	// wedge shape — a real child process the OS reaps); run 2 heals and
	// reports the healed verdict. State via a marker next to the script.
	dir := t.TempDir()
	marker := filepath.Join(dir, "second-run")
	worker := filepath.Join(dir, "sonde-worker.sh")
	script := "#!/bin/sh\n" +
		"key=\"$1\"\n" +
		"if [ -e \"" + marker + "\" ]; then\n" +
		"  mkdir -p \"" + workRoot + "/$key\"\n" +
		"  printf '%%PDF-healed' > \"" + workRoot + "/$key/work.pdf\"\n" +
		"  echo '{\"verdict\":\"healed\"}'\n" +
		"  exit 0\n" +
		"fi\n" +
		"touch \"" + marker + "\"\n" +
		"kill -9 $$\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// A real source pdf the custody quarantine reads.
	srcPDF := filepath.Join(dir, "src.pdf")
	if err := os.WriteFile(srcPDF, []byte("%PDF-original"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cfg := config.Load()
	cfg.DatabaseURL = dsn
	cfg.APIPort = port
	cfg.BindAddr = "127.0.0.1"
	cfg.FixerInvokerEnabled = true
	cfg.FixerCommand = worker
	cfg.FixerConcurrency = 1
	cfg.FixerInterval = 20 * time.Millisecond
	cfg.ZoteroBaseURL = fake.srv.URL + "/api"
	cfg.ZoteroWriteKeyFile = filepath.Join(dir, "write-key")
	if err := os.WriteFile(cfg.ZoteroWriteKeyFile, []byte("sonde-write-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ArtifactRoot = filepath.Join(dir, "artifacts")
	cfg.QuarantineRoot = filepath.Join(dir, "quarantine")
	cfg.OpenSearchURL = ""
	cfg.DispatcherEnabled = false

	// Ports deliberately zero: RepairExecutor nil means the LOCAL binding
	// must spawn the real child (the sonde kills a process, not a stub);
	// the runner ports stay unused in this role set.
	root, err := composition.Select(cfg, sondeLogger(), composition.Ports{}, libraryRoles()...)
	if err != nil {
		t.Fatalf("select library roles: %v", err)
	}
	sigCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	if err := root.Start(sigCtx); err != nil {
		root.Stop(context.Background())
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 20*time.Second)
		defer c()
		root.Stop(stopCtx)
	})

	// Seed source/document/attachment + a QUEUED case (the F08 harness
	// shape, via the public repair store).
	var srcID, docID, attID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://split-repair.sonde','users/0','split-repair-sonde') RETURNING id::text`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1,'DOCSONDE1',1,'book','Split Repair Sonde') RETURNING id::text`, srcID).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path)
		VALUES ($1,$2,'ATTSONDE1',1,'DOCSONDE1','imported_file','application/pdf','sonde.pdf',$3)
		RETURNING id::text`, srcID, docID, srcPDF).Scan(&attID); err != nil {
		t.Fatal(err)
	}
	store := repair.NewStore(pool)
	c, _, err := store.CreateRepairCase(ctx, attID, docID, "reparierbar", []byte(`{}`))
	if err != nil || c == nil {
		t.Fatalf("CreateRepairCase: %v %v", err, c)
	}
	if err := store.QueueRepairCase(ctx, c.ID, "reparierbar", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	// The sonde's verdict: the crash burned attempt 1 and stayed retryable
	// (marker proves run 1 really executed), the recovery healed, no case
	// is left holding an in_repair lease, and the custody apply reached
	// the (fake) Zotero write surface.
	deadline := time.Now().Add(60 * time.Second)
	for {
		var status string
		var attempts int
		if err := pool.QueryRow(ctx,
			`SELECT status::text, attempts FROM repair_cases WHERE id=$1`, c.ID).Scan(&status, &attempts); err != nil {
			t.Fatal(err)
		}
		if status == "healed" {
			if attempts != 2 {
				t.Fatalf("healed after %d attempts, want 2 (crash + recovery)", attempts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("case not healed within 60s: status=%s attempts=%d (marker=%v)", status, attempts, fileExists(marker))
		}
		if status == "failed" || status == "blocked_for_dudu" {
			t.Fatalf("crash escalated terminally: status=%s attempts=%d", status, attempts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !fileExists(marker) {
		t.Fatal("the crash run never executed (marker absent) — the sonde did not ride a real child process")
	}
	var inRepair int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM repair_cases WHERE status='in_repair'`).Scan(&inRepair); err != nil {
		t.Fatal(err)
	}
	if inRepair != 0 {
		t.Fatalf("zombie lease: %d case(s) still in_repair after the healed recovery", inRepair)
	}
	if fake.creations() == 0 {
		t.Fatal("custody apply never reached the Zotero write surface (no item creation witnessed)")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func sondeLogger() *log.Logger { return log.New(os.Stderr, "cli-sonde: ", log.LstdFlags) }
