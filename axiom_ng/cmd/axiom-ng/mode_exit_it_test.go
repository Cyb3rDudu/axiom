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
// #215: each test execs the built binary against a SESSION-UNIQUE throwaway
// *_test database (openModeTestDB), so `go test ./...` package parallelism
// cannot race the repo/sync suites that share the base *_test DB — the cmd-IT
// --apply matrix and heartbeat seeding never truncate or trample another
// package's fixtures mid-run. The throwaway schema is migrated from scratch
// and dropped in cleanup.
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
	"github.com/jackc/pgx/v5/pgxpool"
)

// modeTestDBName is the session-unique throwaway *_test database these ITs run
// against (#215). It mirrors the repo/dispatcher package-private-DB pattern so
// `go test ./...` package parallelism never shares a table with another
// package's suite.
var modeTestDBName = func() string {
	if n := os.Getenv("AXIOM_TEST_MODE_DB_NAME"); n != "" {
		return n
	}
	return fmt.Sprintf("axiom_ng_modemode_%d_test", os.Getpid())
}()

// swapDB returns the DSN with its database name replaced, preserving params.
func swapDB(dsn, dbname string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbname
	return u.String(), nil
}

// openModeTestDB validates the base *_test DSN, creates the session-unique
// throwaway DB (via the maintenance "postgres" DB), migrates it, and returns
// the effective DSN with cleanup that drops it. Each test gets a clean schema;
// column-mutating probes (the modeFail test) cannot poison any shared DB.
func openModeTestDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping mode-exit integration test")
	}
	// Same hard guard as the repo suites: never point this at a non-test DB.
	if u, err := url.Parse(base); err != nil || !strings.HasSuffix(u.Path, "_test") {
		t.Fatalf("refusing to run against non-test database %q (must end in _test)", base)
	}
	maintainDSN, err := swapDB(base, "postgres")
	if err != nil {
		t.Fatalf("maintenance dsn: %v", err)
	}
	modeDSN, err := swapDB(base, modeTestDBName)
	if err != nil {
		t.Fatalf("mode db dsn: %v", err)
	}

	ctx := context.Background()
	mp, err := pgxpool.New(ctx, maintainDSN)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	// Drop a leftover from a crashed prior run so the name is always ours.
	if _, err := mp.Exec(ctx, `DROP DATABASE IF EXISTS `+modeTestDBName+` WITH (FORCE)`); err != nil {
		t.Fatalf("drop leftover mode db: %v", err)
	}
	if _, err := mp.Exec(ctx, `CREATE DATABASE `+modeTestDBName); err != nil {
		t.Fatalf("create mode test db: %v", err)
	}
	mp.Close()

	d, err := db.Open(ctx, modeDSN)
	if err != nil {
		t.Fatalf("open mode db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate mode db: %v", err)
	}
	d.Close()

	t.Cleanup(func() {
		// Reconnect maintenance pool (the test pool closed above) to drop.
		mp2, err := pgxpool.New(context.Background(), maintainDSN)
		if err != nil {
			return
		}
		defer mp2.Close()
		_, _ = mp2.Exec(context.Background(), `DROP DATABASE `+modeTestDBName+` WITH (FORCE)`)
	})
	return modeDSN
}

// canonical form, one entity per ACTIVE snapshot; snapshots are unique per
// attachment — schema 0011 — so the pair spans two attachments) through the
// same minimal column set the repo ITs use, so the exec'd
// `-consolidate-entities --apply` has real work and its heartbeat must
// appear on the binary's stderr — the end-to-end proof that main wires the
// CLI-mode sink (#202; the unit IT proves the loop beats, this proves the
// wiring). Fixed keys + pre-clean keep reruns re-seedable on the shared DB.
func seedHeartbeatPair(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	for _, stmt := range []string{
		// Pre-clean (rerun-safety): fixed keys identify yesterday's pair.
		`DELETE FROM processing_entities WHERE snapshot_id IN
		   (SELECT id FROM processing_snapshots WHERE profile_hash = 'p-hb')`,
		`DELETE FROM processing_snapshots WHERE profile_hash = 'p-hb'`,
		`DELETE FROM zotero_attachments WHERE zotero_key IN ('ATTHB1', 'ATTHB2', 'ATTDOCHB1', 'ATTDOCHB2')`,
		`DELETE FROM zotero_documents WHERE zotero_key IN ('DOCHB1', 'DOCHB2')
		   AND source_id IN (SELECT id FROM zotero_sources WHERE base_url = 'https://hb.test')`,
		`DELETE FROM zotero_sources WHERE base_url = 'https://hb.test'`,
		// Fixture: one source, TWO documents (one attachment each) — the
		// merge pair must live in different documents; two active snapshots
		// under ONE document would violate the 0019 document-canonical
		// invariant (#228).
		`INSERT INTO zotero_sources (base_url, library_id, server_id)
		   VALUES ('https://hb.test', 'lib-hb', 'srv-hb')`,
		`INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		   SELECT s.id, dk, 1, 'book', 'HB' FROM zotero_sources s
		   CROSS JOIN (SELECT unnest(ARRAY['DOCHB1','DOCHB2']) AS dk) x
		   WHERE s.base_url = 'https://hb.test'`,
		`INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
		     parent_zotero_key, link_mode, content_type, filename, content_hash, preferred, deleted)
		   SELECT s.id, d.id, 'ATT' || d.zotero_key, 1, d.zotero_key, 'imported_file',
		          'application/pdf', 'hb.pdf', 'hb-hash', true, false
		   FROM zotero_sources s
		   JOIN zotero_documents d ON d.source_id = s.id AND d.zotero_key IN ('DOCHB1','DOCHB2')
		   WHERE s.base_url = 'https://hb.test'`,
		// One ACTIVE snapshot per attachment AND per document (0011 + 0019).
		`INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
		     processor_version, profile_hash, document_id, profile, active)
		   SELECT a.id, 'hb-hash', 'hbtest', '1', 'p-hb', a.document_id, '{}', true
		   FROM zotero_attachments a WHERE a.zotero_key IN ('ATTDOCHB1','ATTDOCHB2')`,
		// Same canonical form in both snapshots -> one guarded merge group.
		`INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form, type)
		   SELECT s.id, 'hb-ent', 'herzschlag', 'herzschlag', 'LOCATION'
		   FROM processing_snapshots s WHERE s.profile_hash = 'p-hb'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed heartbeat fixture: %v\n%s", err, stmt)
		}
	}
}

// runMode execs the built binary with args and the guarded env; it fails the
// test on fall-through (deadline + "listening on"), on non-zero exit, and on
// the server boot's log line, returning the combined output on success.
func runMode(t *testing.T, bin, dsn string, args ...string) string {
	t.Helper()
	// Fresh port: a fall-through binds HERE and blocks instead of failing on
	// a busy/refused port.
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
		// Attribute the deadline honestly (#202 review): a fall-through is
		// provable by output; a merely slow apply otherwise.
		if strings.Contains(string(out), "listening on") {
			t.Fatalf("%s fell through to the server boot and was killed at the deadline (#202 incident class)\n%s",
				strings.Join(args, " "), out)
		}
		t.Fatalf("%s did not terminate within 60s (slow apply? no server-boot line in output)\n%s",
			strings.Join(args, " "), out)
	}
	if err != nil {
		t.Fatalf("%s exited with error: %v\n%s", strings.Join(args, " "), err, out)
	}
	if strings.Contains(string(out), "listening on") {
		t.Fatalf("%s reached the server boot (must run once and exit)\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

// TestIT_ModeFlagsExitWithoutServerBoot execs the built binary once per mode
// flag in BOTH invocation shapes (dry-run and --apply; -repoint-alias-edges
// has no dry gate) and asserts: exit code 0, no "listening on", termination
// inside the deadline.
func TestIT_ModeFlagsExitWithoutServerBoot(t *testing.T) {
	// #215: dedicated throwaway *_test DB — migrations + the --apply matrix
	// run against a schema no other package shares.
	dsn := openModeTestDB(t)

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
				runMode(t, bin, dsn, args...)
			})
		}
	}
}

// TestIT_ModeFlagsHeartbeatOnExec (#202 wiring witness): with a seeded
// merge-eligible pair, the exec'd `-consolidate-entities --apply` must emit a
// heartbeat line on its stderr — proving main wires the CLI-mode sink (the
// server-path wiring alone would leave CLI modes silent, the exact gap the
// review round found).
func TestIT_ModeFlagsHeartbeatOnExec(t *testing.T) {
	// #215: dedicated throwaway *_test DB; seeding + exec cannot collide with
	// another package's suite under `go test ./...`.
	dsn := openModeTestDB(t)
	seedHeartbeatPair(t, dsn)

	bin := filepath.Join(t.TempDir(), "axiom-ng")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	out := runMode(t, bin, dsn, "-consolidate-entities", "--apply")
	if !strings.Contains(out, "kg heartbeat:") {
		t.Fatalf("expected a heartbeat line on the exec'd mode's output (sink must be wired in the CLI-mode path)\n%s", out)
	}
	// The heartbeat's position marker is the loser entity id prefix — the
	// seeded pair guarantees exactly one loser.
	if !strings.Contains(out, "entities 1/1") {
		t.Fatalf("heartbeat must report the seeded merge (entities 1/1)\n%s", out)
	}
}

// TestIT_ModeFailExitCodeAndConsistencyStatement (#202 exit-1 probe): a
// broken schema (the consolidation target table gone) makes the apply mode
// exit 1 with the documented MODE FAILED line carrying the
// state-consistency statement.
func TestIT_ModeFailExitCodeAndConsistencyStatement(t *testing.T) {
	// #215: dedicated throwaway *_test DB — the column drop/restore runs on a
	// schema this test owns and the DB is dropped in cleanup, so it can never
	// poison a shared suite.
	dsn := openModeTestDB(t)
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	pool := d.Pool()
	// Break the consolidation plan losslessly restorable: entityConsolidationPairs
	// selects coalesce(canonical_form, text), so a missing column fails the
	// apply into modeFail. Dropping the whole table would CASCADE into
	// dependent tables that the migration ledger (already-applied files are
	// skipped) would never recreate — a column drop/add is the narrow probe.
	if _, err := pool.Exec(ctx, `ALTER TABLE processing_entities DROP COLUMN canonical_form`); err != nil {
		t.Fatalf("break schema: %v", err)
	}
	t.Cleanup(func() {
		// Not strictly needed (the throwaway DB is dropped) but keeps the pool
		// state honest while the connection is still open in this process.
		if _, err := pool.Exec(context.Background(),
			`ALTER TABLE processing_entities ADD COLUMN IF NOT EXISTS canonical_form TEXT`); err != nil {
			t.Fatalf("restore schema: %v", err)
		}
	})

	bin := filepath.Join(t.TempDir(), "axiom-ng")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	c := exec.Command(bin, "-consolidate-entities", "--apply")
	c.Env = append(os.Environ(), "AXIOM_DATABASE_URL="+dsn, "AXIOM_ALLOW_DEBUG_BIND=1", "AXIOM_API_PORT=8099")
	out, err := c.CombinedOutput()
	if err == nil {
		t.Fatalf("mode must exit non-zero on a broken schema; got exit 0\n%s", out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("exit code = %v, want 1\n%s", err, out)
	}
	for _, want := range []string{"MODE FAILED (exit 1)", "state consistent (transaction rolled back"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output missing %q\n%s", want, out)
		}
	}
}

// TestIT_HelpDocumentsExitCodes (#202): -help exits 0 and documents the
// mode/exit-code contract. No DB work — pure arg handling.
func TestIT_HelpDocumentsExitCodes(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "axiom-ng")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}
	c := exec.Command(bin, "-help")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("-help exited with error: %v\n%s", err, out)
	}
	for _, want := range []string{"Exit codes:", "never falls through"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("-help output missing %q\n%s", want, out)
		}
	}
}
