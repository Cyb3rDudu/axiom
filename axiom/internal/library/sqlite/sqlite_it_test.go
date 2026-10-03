// sqlite_it_test.go — the SQLite profile's operating rules (F12 #306),
// test-pinned: the pragmas are asserted at startup AND re-read here; the
// file is created atomically with restrictive permissions; reopening is
// idempotent (fresh install vs existing); the single-writer lease holds
// across TWO REAL PROCESSES (re-exec probe, the F07 pattern); and the
// file carries ONLY the library_* namespace (no Store tables — the
// component isolation this issue splits on).
package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/contracterr"
	lib "github.com/Cyb3rDudu/axiom/axiom/internal/library"
)

// TestOperatingPragmasPinned — WAL, foreign_keys, busy_timeout took
// effect on the file (the runtime state, not the DSN string).
func TestOperatingPragmasPinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	r, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	var mode string
	var fk, busy int
	if err := r.read.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := r.read.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if err := r.read.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || fk != 1 || busy < 5000 {
		t.Fatalf("pinned pragmas drifted: journal=%s fk=%d busy=%dms", mode, fk, busy)
	}
	// WAL side files exist while the handles are open (the WAL mode is
	// live, not just a flag).
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("WAL side file missing: %v", err)
	}
}

// TestAtomicCreationAndPermissions — the file appears via the atomic
// exclusive create (no temp residue) with 0600; a directory this code
// creates is 0700.
func TestAtomicCreationAndPermissions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "state")
	path := filepath.Join(dir, "library.sqlite")
	if _, err := Open(context.Background(), path); err != nil {
		t.Fatalf("open (creates dirs + file): %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("library.sqlite must be 0600, got %o", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("created dir must be 0700, got %o", di.Mode().Perm())
	}
}

// TestAdoptExistingEmptyFile — the exclusive-create loser path: a file
// that already exists (the concurrent creator won O_EXCL, or an operator
// touched one) is ADOPTED, not clobbered — Open initializes and migrates
// it like a fresh install.
func TestAdoptExistingEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("adopt existing empty file: %v", err)
	}
	defer r.Close()
	// The adopted file carries the full library namespace (migrated).
	rows, err := r.read.QueryContext(context.Background(), `SELECT name FROM sqlite_master WHERE type='table' AND name='library_imports'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the adopted file was not migrated")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestReopenIdempotent — a second Open over the existing file migrates
// nothing new and keeps the ledger (fresh install vs. existing file).
func TestReopenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	ctx := context.Background()
	r1, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.CreateImport(ctx, lib.ImportRow{
		IdempotencyKey: "reopen", PayloadHash: "p", RecordType: "book",
		RequestJSON: []byte(`{}`), Status: "received",
		StagingSHA256: "sha", StagingSize: 1, MediaType: "application/pdf",
	}); err != nil {
		t.Fatal(err)
	}
	r1.Close()
	r2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	if row, err := r2.GetByIdempotencyKey(ctx, "reopen"); err != nil || row.ImportID == "" {
		t.Fatalf("data must survive the reopen: %+v %v", row, err)
	}
}

// TestFileCarriesOnlyLibraryNamespace — the component isolation: this
// file NEVER carries Store tables (jobs, snapshots, chunks, embeddings,
// KG, outbox) — one file per component, no ATTACH, no cross-queries.
func TestFileCarriesOnlyLibraryNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	r, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rows, err := r.read.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range tables {
		if strings.HasPrefix(name, "sqlite_") { // engine internals (sequence…)
			continue
		}
		if name == "library_schema_migrations" || strings.HasPrefix(name, "library_") {
			continue
		}
		t.Fatalf("library.sqlite carries a non-Library table %q — component isolation broken (tables: %v)", name, tables)
	}
}

// TestTwoProcessWriterLease — the single-writer declaration across TWO
// REAL PROCESSES: the parent holds the lease; the child process (this
// binary, helper mode) tries to acquire the SAME scope and must be
// refused with the typed conflict. The F07 two-process sonde, SQLite
// profile.
func TestTwoProcessWriterLease(t *testing.T) {
	if os.Getenv("AXIOM_SQLITE_LEASE_CHILD") == "1" {
		// helper process — distinct owner identity from the parent, so
		// the probe proves a DIFFERENT writer is refused (not a self-hit
		// on the parent's own lease row). Exit contract: 0 = refused with
		// the typed conflict, 2 = acquired (the guard broke), 3 = open
		// error, 4 = a NON-conflict error (busy timeout, engine trouble —
		// not the lease guard's verdict).
		r, err := Open(context.Background(), os.Getenv("AXIOM_SQLITE_LEASE_PATH"))
		if err != nil {
			fmt.Println("child-open-error:", err)
			os.Exit(3)
		}
		defer r.Close()
		err = r.AcquireWriterLease(context.Background(), "zotero|child|probe", "child-host:2:n", lib.DefaultWriterLeaseTTL)
		if err == nil {
			fmt.Println("child-acquired: lease guard FAILED across processes")
			os.Exit(2)
		}
		if class, ok := contracterr.ClassOf(err); ok && class == contracterr.ClassConflict {
			fmt.Println("child-refused:", err)
			os.Exit(0)
		}
		fmt.Println("child-error:", err)
		os.Exit(4)
	}

	path := filepath.Join(t.TempDir(), "library.sqlite")
	r, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.AcquireWriterLease(context.Background(), "zotero|child|probe", "parent-host:1:n", lib.DefaultWriterLeaseTTL); err != nil {
		t.Fatalf("parent acquire: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestTwoProcessWriterLease", "-test.v")
	cmd.Env = append(os.Environ(), "AXIOM_SQLITE_LEASE_CHILD=1", "AXIOM_SQLITE_LEASE_PATH="+path)
	out, err := cmd.CombinedOutput()
	// The helper exits 0 ONLY on the typed refusal. CombinedOutput's err
	// is the exit status, not the verdict — decode it before failing.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
		t.Fatalf("the child process ACQUIRED the live lease — single-writer guard broken across processes:\n%s", out)
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 4 {
		t.Fatalf("the child probe failed with a NON-conflict error (busy timeout or engine trouble — not a lease verdict):\n%s", out)
	}
	if err != nil {
		t.Fatalf("child probe failed:\n%s (%v)", out, err)
	}
	if !strings.Contains(string(out), "child-refused:") {
		t.Fatalf("child did not report the typed refusal:\n%s", out)
	}
	if !strings.Contains(string(out), "parent-host:1:n") {
		t.Fatalf("refusal must diagnose the live owner:\n%s", out)
	}
}

// TestBusyTimeoutUnderCrossProcessContention — the pinned busy_timeout
// (5s) must PARK a second writer and retry it to success, never error:
// the parent holds an IMMEDIATE write transaction for ~1s while a child
// PROCESS attempts a write; the child must land its write after the
// parent commits (exit 0 + marker). Without busy_timeout the child
// would fail fast with SQLITE_BUSY — the single-writer slot is one
// process's transaction, the timeout is the parking discipline between
// them.
func TestBusyTimeoutUnderCrossProcessContention(t *testing.T) {
	if os.Getenv("AXIOM_SQLITE_BUSY_CHILD") == "1" {
		// helper process: a write that must park on the parent's live
		// IMMEDIATE lock, then succeed. Exit contract: 0 = write landed,
		// 2 = write error (busy timeout exhausted or worse), 3 = open
		// error.
		r, err := Open(context.Background(), os.Getenv("AXIOM_SQLITE_LEASE_PATH"))
		if err != nil {
			fmt.Println("child-open-error:", err)
			os.Exit(3)
		}
		defer r.Close()
		if err := r.CreateImport(context.Background(), lib.ImportRow{
			IdempotencyKey: "busy-child", PayloadHash: "p", RecordType: "book",
			RequestJSON: []byte(`{}`), Status: "received",
			StagingSHA256: "sha", StagingSize: 1, MediaType: "application/pdf",
		}); err != nil {
			fmt.Println("child-write-error:", err)
			os.Exit(2)
		}
		fmt.Println("child-write-ok: busy-child")
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "library.sqlite")
	r, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	// The parent claims the single writer slot (IMMEDIATE via the write
	// handle's _txlock) and HOLDS it across the child's attempt.
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := formatTS(time.Now())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO library_imports (import_id, idempotency_key, payload_hash, record_type, request_json,
			status, staging_sha256, staging_size, media_type, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		mustNewUUID(), "busy-parent", "p", "book", `{}`, "received", "sha", 1, "application/pdf", ts, ts); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestBusyTimeoutUnderCrossProcessContention", "-test.v")
	cmd.Env = append(os.Environ(), "AXIOM_SQLITE_BUSY_CHILD=1", "AXIOM_SQLITE_LEASE_PATH="+path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	time.Sleep(1 * time.Second) // the child is parked on the parent's lock now
	if err := tx.Commit(); err != nil {
		cmd.Process.Kill()
		t.Fatalf("parent commit: %v", err)
	}
	if werr := cmd.Wait(); werr != nil {
		t.Fatalf("the child write did not survive the contention (busy_timeout must park and retry, not error):\n%s (%v)", out.String(), werr)
	}
	if !strings.Contains(out.String(), "child-write-ok: busy-child") {
		t.Fatalf("child did not report the parked-then-succeeded write:\n%s", out.String())
	}
	// The marker is not enough — the child's row must actually exist.
	if row, err := r.GetByIdempotencyKey(ctx, "busy-child"); err != nil || row.ImportID == "" {
		t.Fatalf("the child's write must be durable after the contention: %+v %v", row, err)
	}
}

// TestCrossEngineWriteSerialization — the teeth for the write handle's
// IMMEDIATE transactions (F12 #306): TWO engine instances over the SAME
// file (separate connection pools — the in-process stand-in for two
// processes) concurrently PublishRevision on ONE record. The minting
// transaction READS (existing revision lookup, MAX) and then WRITES —
// with a DEFERRED begin that read-then-write upgrade is the classic
// SQLITE_BUSY_SNAPSHOT (not retried by the busy handler): the mutation
// probe (drop _txlock=immediate from dsn()) turns this test red. The
// suite's goroutine probe cannot catch this (it funnels through ONE
// pool with MaxOpenConns=1, where serialization is structural).
func TestCrossEngineWriteSerialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	ctx := context.Background()
	r1, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	const perEngine = 4
	errs := make(chan error, perEngine*2)
	type result struct {
		id     int64
		minted bool
	}
	results := make(chan result, perEngine*2)
	var wg sync.WaitGroup
	publishN := func(r *Repo, tag string) {
		defer wg.Done()
		for i := 0; i < perEngine; i++ {
			dom := revisionFixtureFor(t, tag, i)
			id, minted, err := r.PublishRevision(ctx, dom)
			if err != nil {
				errs <- err
				continue
			}
			results <- result{id: id, minted: minted}
		}
	}
	wg.Add(2)
	go publishN(r1, "a")
	go publishN(r2, "b")
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatalf("cross-engine publish (a deferred-BEGIN regression surfaces here as SQLITE_BUSY_SNAPSHOT): %v", err)
	}
	// All publishes must survive with strictly monotonic ids 1..2N.
	ids := map[int64]bool{}
	for res := range results {
		if !res.minted {
			t.Fatalf("every distinct content must mint, got surviving id %d unminted", res.id)
		}
		if ids[res.id] {
			t.Fatalf("duplicate revision id %d — cross-engine serialization broken", res.id)
		}
		ids[res.id] = true
	}
	for want := int64(1); want <= perEngine*2; want++ {
		if !ids[want] {
			t.Fatalf("revision id %d missing — minting lost a slot under contention (got %v)", want, ids)
		}
	}
}

// revisionFixtureFor builds a distinct-content revision under one record.
func revisionFixtureFor(t *testing.T, tag string, i int) lib.SourceRevisionDomain {
	return lib.SourceRevisionDomain{
		SourceID:      "11111111-1111-4111-8111-111111111111",
		RecordID:      "XCONN",
		RenditionID:   "ATT-XCONN",
		ContentHash:   fmt.Sprintf("sha-xconn-%s-%d", tag, i),
		MediaType:     "application/pdf",
		ContentTicket: fmt.Sprintf("lst-xconn-%s-%d", tag, i),
		Origin:        "import",
		CreatedAt:     time.Now(),
	}
}
