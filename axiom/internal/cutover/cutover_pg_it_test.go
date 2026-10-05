// cutover_pg_it_test.go — the DM09 evidence chain (#318), gated on
// AXIOM_TEST_DATABASE_URL (scratch-DB convention, same as the
// databundle suite): a full cutover against a seeded legacy copy into
// a prepared target, the confirmation-gate sonde (zero mutations
// without the flag), the stuck-lease sondes (wait timeout / cancel
// convergence / abort), the mid-cutover failure with an exact resume
// (no gate re-runs), the DM06 FK-drop witness on the adopted DB, the
// tombstone apply, the post-write rollback with an idempotent
// re-apply, and the pre-write discriminator.
package cutover

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/databundle"
	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	libpg "github.com/Cyb3rDudu/axiom/axiom/internal/library/pglib"
	"github.com/jackc/pgx/v5/pgxpool"
)

// itScratch creates a fresh scratch database (core+library migrated),
// returns its DSN + cleanup.
func itScratch(t *testing.T, tag string) (string, func()) {
	t.Helper()
	admin := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set — PG-gated cutover tests skip")
	}
	base := dsnDatabaseOf(admin)
	if !strings.HasSuffix(base, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", base)
	}
	name := fmt.Sprintf("%s_cutover_%s_%d_test", strings.TrimSuffix(base, "_test"), tag, os.Getpid())
	ctx := context.Background()
	ap, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	if _, err := ap.Exec(ctx, fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, name)); err != nil {
		t.Fatalf("kill connections: %v", err)
	}
	if _, err := ap.Exec(ctx, `DROP DATABASE IF EXISTS `+name); err != nil {
		t.Fatalf("drop old scratch: %v", err)
	}
	if _, err := ap.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	dsn := dsnWithDatabaseOf(admin, name)
	cleanup := func() {
		if _, err := ap.Exec(ctx, fmt.Sprintf(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, name)); err == nil {
			_, _ = ap.Exec(ctx, `DROP DATABASE IF EXISTS `+name)
		}
		ap.Close()
	}
	core, err := db.Open(ctx, dsn)
	if err != nil {
		cleanup()
		t.Fatalf("open scratch: %v", err)
	}
	if err := core.Migrate(ctx); err != nil {
		core.Close()
		cleanup()
		t.Fatalf("core migrate: %v", err)
	}
	if err := libpg.Migrate(ctx, core.Pool()); err != nil {
		core.Close()
		cleanup()
		t.Fatalf("library migrate: %v", err)
	}
	core.Close()
	return dsn, cleanup
}

func dsnDatabaseOf(dsn string) string {
	name := dsn
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '?'); i >= 0 {
		name = name[:i]
	}
	return name
}

func dsnWithDatabaseOf(dsn, newDB string) string {
	i := strings.LastIndexByte(dsn, '/')
	base := dsn[:i+1]
	rest := dsn[i+1:]
	if j := strings.IndexAny(rest, "?;"); j >= 0 {
		return base + newDB + rest[j:]
	}
	return base + newDB
}

// seedLegacy plants the corpus: two sources (A kept+changed later, B
// tombstoned later) and one claimed job (the lease sonde material).
func seedLegacy(t *testing.T, dsn string) (srcA, srcB, jobID string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var a, b string
	if err := pool.QueryRow(ctx, `INSERT INTO zotero_sources (base_url, library_id) VALUES ('https://zotero.example', 'users/0') RETURNING id::text`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO zotero_sources (base_url, library_id) VALUES ('https://zotero2.example', 'users/0') RETURNING id::text`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	var j string
	if err := pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, content_hash, status, attempt, max_attempts)
		VALUES ($1, 'deadbeef', 'pending', 0, 3) RETURNING id::text`, a).Scan(&j); err != nil {
		t.Fatal(err)
	}
	return a, b, j
}

// claimJob plants an ACTIVE lease (the stuck-lease sonde).
func claimJob(t *testing.T, dsn, jobID string, leaseFor time.Duration) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tag, err := pool.Exec(ctx, `
		UPDATE ingest_jobs SET status='claimed', claimed_by='stuck-worker', lease_token=gen_random_uuid(),
		       lease_until=now() + make_interval(secs => $1), last_heartbeat_at=now()
		 WHERE id=$2`, int(leaseFor.Seconds()), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("claim affected %d rows", tag.RowsAffected())
	}
}

func jobStatus(t *testing.T, dsn, jobID string) string {
	t.Helper()
	var status string
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.QueryRow(ctx, `SELECT status::text FROM ingest_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func countRows(t *testing.T, dsn, table string) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// fkCount counts the cross-component FKs (the DM06 drop witness).
func fkCount(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_catalog.pg_constraint
		 WHERE contype='f' AND conrelid IN ('ingest_jobs'::regclass, 'processing_snapshots'::regclass)
		   AND confrelid IN ('zotero_sources'::regclass, 'zotero_documents'::regclass, 'zotero_attachments'::regclass)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// writeITPlan lands a plan file; mut lets each sonde bend it.
func writeITPlan(t *testing.T, dir, sourceDSN, targetDSN, baseline string, mut func(*Plan)) string {
	t.Helper()
	p := Plan{
		Format: PlanFormat, FormatVersion: PlanFormatVersion, Component: "library",
		SourceDSN: sourceDSN, StoreDSN: sourceDSN, Target: Target{DSN: targetDSN},
		BaselineBundle: baseline, RunsDir: filepath.Join(dir, "runs"),
		FinalShadow:    true,
		Maintenance:    Maintenance{StuckLease: LeaseWait, WaitTimeoutS: 4, PollIntervalS: 1},
		Schema:         SchemaPlan{ApplyStoreMigrations: true},
		ConfigSwitch:   ConfigSwitch{Path: filepath.Join(dir, "config.sqlite"), Set: map[string]string{"AXIOM_DISPATCHER_ENABLED": "0"}},
		RestartCommand: "true",
		Rollback:       RollbackPlan{RestartCommand: "true"},
	}
	if mut != nil {
		mut(&p)
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runDirOf(t *testing.T, runsDir string) string {
	t.Helper()
	entries, err := os.ReadDir(runsDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no run directory under %s (err=%v)", runsDir, err)
	}
	return filepath.Join(runsDir, entries[0].Name())
}

var quietLog = func(string, ...any) {}

// TestCutoverFullRehearsal — the end-to-end window against seeded
// scratch databases: dry-run sonde, stuck lease (cancel), freeze delta
// (added+changed+tombstoned), asserted schema-before-data, DM06 FK
// drops on the adopted DB, atomic config switch, shadow re-run green,
// staged re-enable; then the post-write rollback rehearsal with an
// idempotent re-apply.
func TestCutoverFullRehearsal(t *testing.T) {
	ctx := context.Background()
	legacyDSN, cleanupLegacy := itScratch(t, "legacy")
	defer cleanupLegacy()
	targetDSN, cleanupTarget := itScratch(t, "target")
	defer cleanupTarget()
	dir := t.TempDir()

	srcA, srcB, jobID := seedLegacy(t, legacyDSN)

	// The pre-window state: baseline bundle from the legacy copy,
	// imported into the target (the rehearsed DM04 loop).
	baseline := filepath.Join(dir, "baseline")
	if _, err := databundle.Export(ctx, "library", databundle.ExportOptions{DSN: legacyDSN, Out: baseline}); err != nil {
		t.Fatal(err)
	}
	if _, err := databundle.Import(ctx, databundle.ImportOptions{From: baseline, DSN: targetDSN}); err != nil {
		t.Fatal(err)
	}

	// The window opens: the legacy corpus evolves (add + change + drop)
	// and a lease is stuck.
	pool, err := pgxpool.New(ctx, legacyDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE zotero_sources SET last_sync_at = now() WHERE id=$1`, srcA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM zotero_sources WHERE id=$1`, srcB); err != nil {
		t.Fatal(err)
	}
	var srcC string
	if err := pool.QueryRow(ctx, `INSERT INTO zotero_sources (base_url, library_id) VALUES ('https://zotero3.example', 'users/0') RETURNING id::text`).Scan(&srcC); err != nil {
		t.Fatal(err)
	}
	// A short-lived stuck lease: cancel converges it once it expires
	// (the gate itself applies the expired-cancel terminalization —
	// no dispatcher is alive in the window to do it).
	claimJob(t, legacyDSN, jobID, 2*time.Second)
	pool.Close()

	planPath := writeITPlan(t, dir, legacyDSN, targetDSN, baseline, func(p *Plan) {
		p.Maintenance.StuckLease = LeaseCancel // converge the stuck lease, then proceed
		p.Maintenance.WaitTimeoutS = 10
		p.ReenableStages = []Stage{{Name: "dispatcher", ConfigSet: map[string]string{"AXIOM_DISPATCHER_ENABLED": "1"}, Command: "true"}}
	})

	// --- Sonde 1: the confirmation gate has teeth ----------------------
	fksBefore := fkCount(t, legacyDSN)
	legacySourcesBefore := countRows(t, legacyDSN, "zotero_sources")
	_, err = Cutover(ctx, Options{PlanPath: planPath, Confirm: false, Logf: quietLog})
	if err != nil {
		t.Fatalf("dry validation failed: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "runs")); len(entries) != 0 {
		t.Fatal("dry run created a run directory — validation must touch nothing")
	}
	if fkCount(t, legacyDSN) != fksBefore {
		t.Fatal("dry run applied migrations — validation must touch nothing")
	}
	if countRows(t, legacyDSN, "zotero_sources") != legacySourcesBefore {
		t.Fatal("dry run mutated the legacy DB — validation must touch nothing")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.sqlite")); !os.IsNotExist(err) {
		t.Fatal("dry run created config.sqlite — validation must touch nothing")
	}
	if countRows(t, targetDSN, "zotero_sources") != 2 {
		t.Fatalf("dry run touched the target (%d rows)", countRows(t, targetDSN, "zotero_sources"))
	}

	// --- The window -----------------------------------------------------
	man, err := Cutover(ctx, Options{PlanPath: planPath, Confirm: true, Logf: quietLog})
	if err != nil {
		t.Fatalf("cutover failed: %v", err)
	}
	if man.Status != "completed" {
		t.Fatalf("run status %q", man.Status)
	}
	rd := filepath.Join(dir, "runs", man.RunID)

	// The stuck lease converged to cancelled, decision on record.
	if st := jobStatus(t, legacyDSN, jobID); st != "cancelled" {
		t.Fatalf("stuck lease job status %q, want cancelled", st)
	}
	var runMan RunManifest
	raw, err := os.ReadFile(filepath.Join(rd, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &runMan); err != nil {
		t.Fatal(err)
	}
	foundDecision := false
	for _, d := range runMan.LeaseDecisions {
		if d.JobID == jobID && d.Choice == LeaseCancel && d.Outcome == "cancelled" {
			foundDecision = true
		}
	}
	if !foundDecision {
		t.Fatalf("lease decision not on record: %+v", runMan.LeaseDecisions)
	}

	// The delta: +1 added, 1 changed, 1 tombstoned, applied to the target.
	var drep DeltaReport
	raw, err = os.ReadFile(filepath.Join(rd, "delta-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &drep); err != nil {
		t.Fatal(err)
	}
	var srcDelta *TableDelta
	for i := range drep.Tables {
		if drep.Tables[i].Table == "zotero_sources" {
			srcDelta = &drep.Tables[i]
		}
	}
	if srcDelta == nil || srcDelta.Added != 1 || srcDelta.Changed != 1 || srcDelta.Tombstoned != 1 {
		t.Fatalf("delta counts wrong: %+v", srcDelta)
	}
	if n := countRows(t, targetDSN, "zotero_sources"); n != 2 {
		t.Fatalf("target carries %d sources, want 2 (2 kept + 1 added - 1 tombstoned)", n)
	}
	// The tombstone really removed srcB from the target.
	var cnt int
	tpool, _ := pgxpool.New(ctx, targetDSN)
	defer tpool.Close()
	if err := tpool.QueryRow(ctx, `SELECT count(*) FROM zotero_sources WHERE id=$1`, srcB).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Fatal("tombstoned source still on the target")
	}
	if err := tpool.QueryRow(ctx, `SELECT count(*) FROM zotero_sources WHERE id=$1`, srcC).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatal("added source missing on the target")
	}

	// The DM06 FK drops landed on the adopted DB (in-window).
	if n := fkCount(t, legacyDSN); n != 0 {
		t.Fatalf("%d cross-component FKs remain after store adoption", n)
	}

	// The config switch landed + the backup exists.
	if _, err := os.Stat(filepath.Join(dir, "config.sqlite")); err != nil {
		t.Fatal("config.sqlite missing after the switch")
	}
	if _, err := os.Stat(filepath.Join(rd, "config-backup.json")); err != nil {
		t.Fatal("config backup missing")
	}

	// The shadow re-run (the window's last data check) is green and on disk.
	if runMan.ShadowReport == "" {
		t.Fatal("shadow report not recorded")
	}

	// --- Pre-write rollback: nothing new since the switch ----------------
	recPre, err := Rollback(ctx, RollbackOptions{RunDir: rd, Confirm: true, Logf: quietLog})
	if err != nil {
		t.Fatalf("pre-write rollback failed: %v", err)
	}
	if recPre.Mode != "pre-write" {
		t.Fatalf("drift-free rollback mode %q, want pre-write", recPre.Mode)
	}
	// No shadow tables exist yet on the legacy side.
	lpool0, err := pgxpool.New(ctx, legacyDSN)
	if err != nil {
		t.Fatal(err)
	}
	var shadowExists bool
	if err := lpool0.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='cutover_shadow_zotero_sources')`).Scan(&shadowExists); err != nil {
		t.Fatal(err)
	}
	lpool0.Close()
	if shadowExists {
		t.Fatal("pre-write rollback created shadow tables — nothing was written, nothing to reverse")
	}

	// --- Post-write rollback rehearsal ----------------------------------
	// New Library writes on the new store: one changed row + one new row.
	if _, err := tpool.Exec(ctx, `UPDATE zotero_sources SET last_sync_at = now() WHERE id=$1`, srcA); err != nil {
		t.Fatal(err)
	}
	var srcD string
	if err := tpool.QueryRow(ctx, `INSERT INTO zotero_sources (base_url, library_id) VALUES ('https://zotero4.example', 'users/0') RETURNING id::text`).Scan(&srcD); err != nil {
		t.Fatal(err)
	}

	rec, err := Rollback(ctx, RollbackOptions{RunDir: rd, Confirm: true, Logf: quietLog})
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if rec.Mode != "post-write" {
		t.Fatalf("rollback mode %q, want post-write (the target drifted since the pre-write pass)", rec.Mode)
	}
	// The reversed rows live in the shadow tables on the legacy side.
	lpool, err := pgxpool.New(ctx, legacyDSN)
	if err != nil {
		t.Fatal(err)
	}
	var appliedRows int64
	if err := lpool.QueryRow(ctx, `SELECT count(*) FROM cutover_shadow_zotero_sources`).Scan(&appliedRows); err != nil {
		t.Fatalf("shadow table missing: %v", err)
	}
	if appliedRows != 2 {
		t.Fatalf("shadow carries %d rows, want 2 (changed + new)", appliedRows)
	}
	// The reversed new row references the NEW store's key space — the
	// shadow table carries it verbatim (LIKE without FKs).
	var d int
	if err := lpool.QueryRow(ctx, `SELECT count(*) FROM cutover_shadow_zotero_sources WHERE id=$1`, srcD).Scan(&d); err != nil {
		t.Fatal(err)
	}
	if d != 1 {
		t.Fatal("post-write new row missing from the shadow table")
	}

	// Idempotency: a third invocation (nothing drifted since the
	// second) re-lands nothing new — same shadow rows, no duplicates.
	rec2, err := Rollback(ctx, RollbackOptions{RunDir: rd, Confirm: true, Logf: quietLog})
	if err != nil {
		t.Fatalf("rollback re-run failed: %v", err)
	}
	if rec2.Mode != "post-write" {
		t.Fatalf("re-run mode %q", rec2.Mode)
	}
	if !strings.Contains(rec2.reverseNote(t, rd), "idempotent") {
		t.Fatalf("re-run gate note does not evidence idempotent skips: %q", rec2.reverseNote(t, rd))
	}
	var appliedRows2 int64
	if err := lpool.QueryRow(ctx, `SELECT count(*) FROM cutover_shadow_zotero_sources`).Scan(&appliedRows2); err != nil {
		t.Fatal(err)
	}
	if appliedRows2 != appliedRows {
		t.Fatalf("re-run changed the shadow row count: %d → %d (duplicates!)", appliedRows, appliedRows2)
	}
	lpool.Close()

	tpool.Close()
}

// reverseNote reads a rollback record's reverse-delta gate note (the
// idempotency witness).
func (rec *RollbackRecord) reverseNote(t *testing.T, rd string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rd, "rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r2 RollbackRecord
	if err := json.Unmarshal(raw, &r2); err != nil {
		t.Fatal(err)
	}
	for _, g := range r2.Gates {
		if g.Gate == RBGateReverse {
			return g.Note
		}
	}
	return ""
}

// TestStuckLeaseAbortAndTimeout — the other two documented choices.
func TestStuckLeaseAbortAndTimeout(t *testing.T) {
	ctx := context.Background()
	legacyDSN, cleanupLegacy := itScratch(t, "abrtlegacy")
	defer cleanupLegacy()
	targetDSN, cleanupTarget := itScratch(t, "abrttarget")
	defer cleanupTarget()
	dir := t.TempDir()

	_, _, jobID := seedLegacy(t, legacyDSN)
	baseline := filepath.Join(dir, "baseline")
	if _, err := databundle.Export(ctx, "library", databundle.ExportOptions{DSN: legacyDSN, Out: baseline}); err != nil {
		t.Fatal(err)
	}
	if _, err := databundle.Import(ctx, databundle.ImportOptions{From: baseline, DSN: targetDSN}); err != nil {
		t.Fatal(err)
	}
	claimJob(t, legacyDSN, jobID, 60*time.Second)

	// abort: the run refuses, the lease stays untouched.
	planAbort := writeITPlan(t, dir, legacyDSN, targetDSN, baseline, func(p *Plan) {
		p.Maintenance.StuckLease = LeaseAbort
	})
	_, err := Cutover(ctx, Options{PlanPath: planAbort, Confirm: true, Logf: quietLog})
	if err == nil || !strings.Contains(err.Error(), jobID) {
		t.Fatalf("abort choice must fail naming the lease, got %v", err)
	}
	if st := jobStatus(t, legacyDSN, jobID); st != "claimed" {
		t.Fatalf("abort must leave the job alone, status %q", st)
	}

	// wait + timeout: the drain budget expires, the run refuses.
	dir2 := t.TempDir()
	planWait := writeITPlan(t, dir2, legacyDSN, targetDSN, baseline, func(p *Plan) {
		p.Maintenance.StuckLease = LeaseWait
		p.Maintenance.WaitTimeoutS = 1
		p.Maintenance.PollIntervalS = 1
	})
	_, err = Cutover(ctx, Options{PlanPath: planWait, Confirm: true, Logf: quietLog})
	if err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("wait timeout must fail the run, got %v", err)
	}
	if st := jobStatus(t, legacyDSN, jobID); st != "claimed" {
		t.Fatalf("wait timeout must leave the job claimed, status %q", st)
	}
}

// TestMidCutoverFailureResume — a restart check fails mid-window; the
// resume continues from EXACTLY there (no gate re-runs: the stop
// command marker fires once, the freeze cutoff is not recomputed).
func TestMidCutoverFailureResume(t *testing.T) {
	ctx := context.Background()
	legacyDSN, cleanupLegacy := itScratch(t, "reslegacy")
	defer cleanupLegacy()
	targetDSN, cleanupTarget := itScratch(t, "restarget")
	defer cleanupTarget()
	dir := t.TempDir()

	_, _, _ = seedLegacy(t, legacyDSN)
	baseline := filepath.Join(dir, "baseline")
	if _, err := databundle.Export(ctx, "library", databundle.ExportOptions{DSN: legacyDSN, Out: baseline}); err != nil {
		t.Fatal(err)
	}
	if _, err := databundle.Import(ctx, databundle.ImportOptions{From: baseline, DSN: targetDSN}); err != nil {
		t.Fatal(err)
	}

	// A health port that is CLOSED first, opened before the resume.
	port := freePort(t)
	marker := filepath.Join(dir, "stops.txt")
	planPath := writeITPlan(t, dir, legacyDSN, targetDSN, baseline, func(p *Plan) {
		p.Maintenance.StopCommands = []string{fmt.Sprintf("echo stop >> %s", marker)}
		p.RestartChecks = []Check{{Kind: "http", URL: fmt.Sprintf("http://127.0.0.1:%d/api/health", port), RetryS: 1}}
	})

	man, err := Cutover(ctx, Options{PlanPath: planPath, Confirm: true, Logf: quietLog})
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("the closed health port must fail the restart gate, got %v", err)
	}
	if man == nil || man.Status != "running" {
		t.Fatalf("failed run must stay resumable (man=%v)", man != nil)
	}
	rd := filepath.Join(dir, "runs", man.RunID)

	// The completed gates are on record — everything up to the switch.
	var rm RunManifest
	raw, _ := os.ReadFile(filepath.Join(rd, "run.json"))
	if err := json.Unmarshal(raw, &rm); err != nil {
		t.Fatal(err)
	}
	for _, gate := range []string{GateMaintenance, GateFreeze, GateTargetSchema, GateDeltaImport, GateStoreAdoption, GateConfigSwitch} {
		if !gateCompleted(&rm, gate) {
			t.Fatalf("gate %s not completed before the restart failure", gate)
		}
	}
	// The freeze evidence predates the failure: capture the cutoff.
	freezeCutoff := rm.cutoffOf(t, rd)
	stopCount := fileLines(t, marker)
	if stopCount != 1 {
		t.Fatalf("stop marker fired %d times, want 1", stopCount)
	}

	// Open the health port, resume the SAME run.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	}))

	man2, err := Cutover(ctx, Options{PlanPath: planPath, RunDir: rd, Confirm: true, Logf: quietLog})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if man2.Status != "completed" {
		t.Fatalf("resumed run status %q", man2.Status)
	}
	// No re-runs: the stop command fired exactly once in total.
	if n := fileLines(t, marker); n != 1 {
		t.Fatalf("resume re-ran the stop gate (marker lines %d)", n)
	}
	// The freeze bundle was NOT recomputed (same cutoff).
	raw2, _ := os.ReadFile(filepath.Join(rd, "run.json"))
	var rm2 RunManifest
	if err := json.Unmarshal(raw2, &rm2); err != nil {
		t.Fatal(err)
	}
	if c := rm2.cutoffOf(t, rd); c != freezeCutoff {
		t.Fatalf("freeze cutoff moved on resume: %q → %q", freezeCutoff, c)
	}
}

func gateCompleted(rm *RunManifest, gate string) bool {
	for _, g := range rm.Gates {
		if g.Gate == gate && g.Status == "completed" {
			return true
		}
	}
	return false
}

func (rm *RunManifest) cutoffOf(t *testing.T, rd string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rd, "delta-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d DeltaReport
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d.Cutoff
}

func fileLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(strings.TrimSpace(string(b)), "\n") + 1
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

// TestDrainConvergesPreExistingExpiredCancel — the deterministic
// regression for the empty-exit convergence: the job is seeded DIRECTLY
// into the post-expiry state (claimed, cancel_requested_at set,
// lease_until in the past — an operator cancellation with the
// dispatcher already gone), so the drain's FIRST poll sees zero active
// leases and the convergence at the empty exit is the only thing that
// can terminalize the row. No timers, no polling luck: if the empty-exit
// convergence is removed, the row stays claimed and this test is red.
func TestDrainConvergesPreExistingExpiredCancel(t *testing.T) {
	ctx := context.Background()
	legacyDSN, cleanupLegacy := itScratch(t, "cvrg")
	defer cleanupLegacy()
	_, _, jobID := seedLegacy(t, legacyDSN)

	pool, err := pgxpool.New(ctx, legacyDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// The seeded state: cancel requested a minute ago, lease expired a
	// minute ago, still claimed (nobody ran terminalizeStale).
	tag, err := pool.Exec(ctx, `
		UPDATE ingest_jobs SET
			status='claimed', claimed_by='departed-worker', lease_token=gen_random_uuid(),
			lease_until = now() - interval '60 seconds',
			last_heartbeat_at = now() - interval '70 seconds',
			cancel_requested_at = now() - interval '60 seconds'
		WHERE id=$1`, jobID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("seed expired cancel: rows=%d err=%v", tag.RowsAffected(), err)
	}

	// Drain under the cancel choice: the first poll is already
	// lease-empty; convergence happens at the empty exit.
	if err := drainLeases(ctx, pool, drainOptions{
		Choice:       LeaseCancel,
		WaitTimeout:  5 * time.Second,
		PollInterval: 1 * time.Second,
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}

	var status string
	var cancelReq bool
	var claimedBy *string
	if err := pool.QueryRow(ctx,
		`SELECT status::text, (cancel_requested_at IS NOT NULL), claimed_by FROM ingest_jobs WHERE id=$1`, jobID,
	).Scan(&status, &cancelReq, &claimedBy); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || !cancelReq || claimedBy != nil {
		t.Fatalf("row not terminalized: status=%q cancel_requested=%v claimed_by=%v (want cancelled/true/nil)", status, cancelReq, claimedBy)
	}

	// wait and abort must NOT converge: the choices that did not opt
	// into cancellation leave the row to the owning semantics.
	_, _, job2 := seedSecondJob(t, legacyDSN)
	if _, err := pool.Exec(ctx, `
		UPDATE ingest_jobs SET
			status='claimed', claimed_by='w2', lease_token=gen_random_uuid(),
			lease_until = now() - interval '60 seconds',
			cancel_requested_at = now() - interval '60 seconds'
		WHERE id=$1`, job2); err != nil {
		t.Fatal(err)
	}
	if err := drainLeases(ctx, pool, drainOptions{Choice: LeaseWait, WaitTimeout: time.Second, PollInterval: 10 * time.Millisecond}); err != nil {
		t.Fatalf("wait drain over expired leases: %v", err)
	}
	var status2 string
	if err := pool.QueryRow(ctx, `SELECT status::text FROM ingest_jobs WHERE id=$1`, job2).Scan(&status2); err != nil {
		t.Fatal(err)
	}
	if status2 != "claimed" {
		t.Fatalf("wait choice must not converge foreign cancels, got %q", status2)
	}
}

// seedSecondJob plants one more pending job (the wait/abort control).
func seedSecondJob(t *testing.T, dsn string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var src, j string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM zotero_sources ORDER BY id LIMIT 1`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, content_hash, status, attempt, max_attempts)
		VALUES ($1, 'dm09-second', 'pending', 0, 3) RETURNING id::text`, src).Scan(&j); err != nil {
		t.Fatal(err)
	}
	return src, "", j
}
