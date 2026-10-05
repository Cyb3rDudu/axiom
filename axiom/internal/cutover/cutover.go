// cutover.go — the gate runner: `axiom data cutover`. Six irreversible
// minutes become a recorded, resumable state machine. Gate order is
// the issue contract (and the runbook's resume-point map):
//
//	1 maintenance     stop mutators, drain leases (choice documented)
//	2 freeze          final freeze delta bundle + report + witness
//	3 target-schema   migrate the target BEFORE any data (asserted)
//	4 delta-import    apply the delta (+ tombstones), write the anchor
//	5 store-adoption  store migrations on the adopted DB (DM06 drops)
//	6 config-switch   backup → env compatibility → atomic row flip
//	7 restart         boot without mutating workers + health checks
//	8 shadow          the DM08 re-run — the window's last data check
//	9 reenable:<st>   staged worker re-enable
//
// Without --require-confirmation the runner VALIDATES only (plan,
// baseline, connectivity) and touches nothing.
package cutover

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/config/configstore"
	"github.com/Cyb3rDudu/axiom/axiom/internal/databundle"
	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	libpg "github.com/Cyb3rDudu/axiom/axiom/internal/library/pglib"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/sqlite"
	storemig "github.com/Cyb3rDudu/axiom/axiom/internal/store/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options parameterize a cutover invocation.
type Options struct {
	PlanPath string
	RunDir   string // existing run directory → resume
	Confirm  bool   // --require-confirmation
	Logf     func(format string, args ...any)
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Cutover runs (or resumes) the gated cutover. A returned error is a
// failed run: run.json keeps the completed gates; re-invoke with
// RunDir set to resume from exactly there.
func Cutover(ctx context.Context, opts Options) (*RunManifest, error) {
	planRaw, err := os.ReadFile(opts.PlanPath)
	if err != nil {
		return nil, fmt.Errorf("cutover: read plan: %w", err)
	}
	plan, err := LoadPlan(opts.PlanPath)
	if err != nil {
		return nil, err
	}
	baselineMan, err := databundle.LoadManifest(plan.BaselineBundle)
	if err != nil {
		return nil, fmt.Errorf("cutover: baseline bundle: %w", err)
	}

	// Dry validation — no run directory, no mutation, no DDL.
	if !opts.Confirm {
		if err := dryValidate(ctx, plan, baselineMan, opts); err != nil {
			return nil, err
		}
		opts.logf("cutover: DRY validation complete — nothing was touched; re-run with --require-confirmation to execute the window")
		return nil, nil
	}

	var r *run
	if opts.RunDir != "" {
		r, err = loadRun(opts.RunDir)
		if err != nil {
			return nil, err
		}
		if r.man.Kind != RunKindCutover {
			return nil, fmt.Errorf("cutover: %s is a %s run, not a cutover run", opts.RunDir, r.man.Kind)
		}
		if r.man.PlanDigest != PlanDigest(planRaw) {
			return nil, fmt.Errorf("cutover: %s was planned by a DIFFERENT plan (digest mismatch) — resume requires the plan that started the run", opts.RunDir)
		}
		opts.logf("cutover: resuming run %s (completed: %s)", r.man.RunID, completedList(r))
	} else {
		r, err = newRun(opts.PlanPath, planRaw, plan.RunsDir, RunKindCutover)
		if err != nil {
			return nil, err
		}
		opts.logf("cutover: run %s → %s", r.man.RunID, r.dir)
	}
	r.man.Status = "running"
	_ = r.save()

	if err := runGates(ctx, r, plan, baselineMan, opts); err != nil {
		r.finish(currentGateOf(r), "failed", err.Error())
		r.audit("run", "aborted", err.Error())
		return r.man, err
	}
	r.man.Status = "completed"
	r.finish(GateDone, "completed", "cutover complete — workers re-enabled per plan")
	return r.man, r.save()
}

// dryValidate checks everything cheap and read-only: plan structure,
// baseline integrity, connectivity to every DSN, target reachability.
func dryValidate(ctx context.Context, plan *Plan, baselineMan *databundle.Manifest, opts Options) error {
	opts.logf("cutover: plan OK (format %s v%d, component %s)", PlanFormat, PlanFormatVersion, plan.Component)
	opts.logf("cutover: baseline bundle OK (%d tables, cutoff %s)", len(baselineMan.Tables), baselineMan.Source.ExportCutoff)
	pools := map[string]string{
		"source":      plan.SourceDSN,
		"store":       plan.EffectiveStoreDSN(),
		"maintenance": plan.EffectiveMaintenanceDSN(),
	}
	if plan.Target.DSN != "" {
		pools["target"] = plan.Target.DSN
	}
	for name, dsn := range pools {
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return fmt.Errorf("cutover: %s DSN unreachable: %w", name, err)
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			return fmt.Errorf("cutover: %s DSN ping failed: %w", name, err)
		}
		p.Close()
		opts.logf("cutover: %s DSN reachable", name)
	}
	if plan.Target.SQLitePath != "" {
		if _, err := os.Stat(plan.Target.SQLitePath); err != nil {
			opts.logf("cutover: target sqlite %s does not exist yet — the target-schema gate creates and migrates it", plan.Target.SQLitePath)
		} else {
			if _, err := databundle.IndexSQLiteFile(ctx, plan.Target.SQLitePath); err != nil {
				return fmt.Errorf("cutover: target sqlite unreadable: %w", err)
			}
			opts.logf("cutover: target sqlite readable")
		}
	}
	opts.logf("cutover: gates: maintenance → freeze → target-schema → delta-import → store-adoption → config-switch → restart → shadow → reenable")
	if plan.Maintenance.StuckLease != "" {
		opts.logf("cutover: stuck-lease choice on record: %s", plan.Maintenance.StuckLease)
	}
	return nil
}

// runGates executes the sequence, skipping gates this run already
// completed (resume) — every gate is idempotent by contract.
func runGates(ctx context.Context, r *run, plan *Plan, baselineMan *databundle.Manifest, opts Options) error {
	// 1 maintenance -------------------------------------------------------
	if !r.completed(GateMaintenance) {
		r.begin(GateMaintenance)
		for i, cmd := range plan.Maintenance.StopCommands {
			if err := runCommand(ctx, cmd, filepath.Join(r.dir, fmt.Sprintf("maintenance-stop-%d.log", i)), plan.CommandTimeout()); err != nil {
				return fmt.Errorf("gate maintenance: stop command %d: %w", i, err)
			}
			opts.logf("maintenance: stop command %d OK", i)
		}
		mp, err := pgxpool.New(ctx, plan.EffectiveMaintenanceDSN())
		if err != nil {
			return fmt.Errorf("gate maintenance: open: %w", err)
		}
		if err := drainLeases(ctx, mp, drainOptions{
			Choice:       plan.Maintenance.StuckLease,
			WaitTimeout:  time.Duration(plan.Maintenance.WaitTimeoutS) * time.Second,
			PollInterval: time.Duration(plan.Maintenance.PollIntervalS) * time.Second,
			OnDecision:   func(d LeaseDecision) { r.man.LeaseDecisions = append(r.man.LeaseDecisions, d) },
			Logf:         opts.Logf,
		}); err != nil {
			mp.Close()
			return fmt.Errorf("gate maintenance: %w", err)
		}
		w, err := snapshotJobs(ctx, mp)
		mp.Close()
		if err != nil {
			return fmt.Errorf("gate maintenance: witness: %w", err)
		}
		r.man.Witness = w.record()
		r.finish(GateMaintenance, "completed", fmt.Sprintf("mutators stopped, leases drained (choice %s), witness: %d jobs, %d active leases", plan.Maintenance.StuckLease, w.Count, w.ActiveLeases))
	}

	// 2 freeze ------------------------------------------------------------
	if !r.completed(GateFreeze) {
		r.begin(GateFreeze)
		src, err := databundle.IndexPostgres(ctx, plan.SourceDSN)
		if err != nil {
			return fmt.Errorf("gate freeze: source pull: %w", err)
		}
		base, _, err := databundle.IndexBundle(ctx, plan.BaselineBundle)
		if err != nil {
			return fmt.Errorf("gate freeze: baseline index: %w", err)
		}
		freezeDir := filepath.Join(r.dir, "freeze")
		rep, err := writeDeltaBundle(freezeDir, baselineMan, base, src, r.man.RunID, "freeze", opts.Logf)
		if err != nil {
			return fmt.Errorf("gate freeze: %w", err)
		}
		repRaw, _ := json.MarshalIndent(rep, "", "  ")
		repPath := filepath.Join(r.dir, "delta-report.json")
		if err := writeAtomic(repPath, append(repRaw, '\n'), 0o600); err != nil {
			return fmt.Errorf("gate freeze: report: %w", err)
		}
		r.man.FreezeBundleDir = "freeze"
		r.man.DeltaReportFile = "delta-report.json"

		// No silent continuation: the pull must have happened against a
		// quiesced corpus. Any movement between witness and now taints
		// the freeze — abort, do not improvise.
		mp, err := pgxpool.New(ctx, plan.EffectiveMaintenanceDSN())
		if err != nil {
			return fmt.Errorf("gate freeze: witness recheck open: %w", err)
		}
		w2, lerr := snapshotJobs(ctx, mp)
		mp.Close()
		if lerr != nil {
			return fmt.Errorf("gate freeze: witness recheck: %w", lerr)
		}
		prior := r.man.Witness
		if prior == nil {
			return fmt.Errorf("gate freeze: no maintenance witness on record — the freeze cannot prove a quiesced corpus (resume ordering violated)")
		}
		if prior.Count != w2.Count || prior.ActiveLeases != w2.ActiveLeases || prior.maxUpdatedAt() != w2.maxUpdatedAtStr() {
			return fmt.Errorf("gate freeze: the corpus MOVED during the freeze (witness %d jobs/%d leases → %d/%d) — something is still writing; the freeze is tainted and the run aborts", prior.Count, prior.ActiveLeases, w2.Count, w2.ActiveLeases)
		}
		r.finish(GateFreeze, "completed", fmt.Sprintf("freeze bundle + delta report landed (cutoff %s)", rep.Cutoff), "freeze/", "delta-report.json")
	}

	// 3 target-schema — BEFORE any data (the DM08 lesson, asserted) ------
	if !r.completed(GateTargetSchema) {
		r.begin(GateTargetSchema)
		if plan.Target.DSN != "" {
			if plan.Schema.MigrateTarget {
				core, err := db.Open(ctx, plan.Target.DSN)
				if err != nil {
					return fmt.Errorf("gate target-schema: open: %w", err)
				}
				if err := core.Migrate(ctx); err != nil {
					core.Close()
					return fmt.Errorf("gate target-schema: core migrations: %w", err)
				}
				if err := libpg.Migrate(ctx, core.Pool()); err != nil {
					core.Close()
					return fmt.Errorf("gate target-schema: library migrations: %w", err)
				}
				core.Close()
			}
			// Adoption check ALWAYS (migrated here or prepared
			// pre-window): the gate's completed record is the import's
			// precondition — unverified is not prepared.
			pool, err := pgxpool.New(ctx, plan.Target.DSN)
			if err != nil {
				return fmt.Errorf("gate target-schema: open for verification: %w", err)
			}
			rep, err := libpg.VerifyAdoption(ctx, pool)
			pool.Close()
			if err != nil {
				return fmt.Errorf("gate target-schema: adoption check: %w", err)
			}
			if !rep.Adoptable {
				return fmt.Errorf("gate target-schema: %s", rep.Reason)
			}
			how := "verified prepared target (migrations applied pre-window)"
			if plan.Schema.MigrateTarget {
				how = "target migrated in-window (core+library)"
			}
			r.finish(GateTargetSchema, "completed", fmt.Sprintf("%s: %s", how, rep.Reason))
		} else if plan.Target.SQLitePath != "" {
			repo, err := sqlite.Open(ctx, plan.Target.SQLitePath)
			if err != nil {
				return fmt.Errorf("gate target-schema: sqlite open/migrate: %w", err)
			}
			repo.Close()
			r.finish(GateTargetSchema, "completed", "target sqlite migrated (library schema)")
		} else {
			return fmt.Errorf("gate target-schema: no target — plan is inconsistent")
		}
	}

	// 4 delta-import — REQUIRES the schema gate of THIS run ---------------
	if !r.completed(GateDeltaImport) {
		if !r.completed(GateTargetSchema) {
			return fmt.Errorf("gate delta-import: REFUSED — the target-schema gate has not completed in this run; the import is data-only (zero DDL) and the schema must be migrated first (DM08 lesson: asserted, not assumed)")
		}
		r.begin(GateDeltaImport)
		freezeDir := filepath.Join(r.dir, "freeze")
		tgt, err := OpenTarget(ctx, plan.Target)
		if err != nil {
			return fmt.Errorf("gate delta-import: target: %w", err)
		}
		// Occupancy precheck (fresh entry only — a resumed import is
		// mid-state by design and row-idempotent): the target must still
		// sit at the baseline counts, or be empty (first window run
		// without a pre-window import is NOT supported — the baseline
		// bundle is the delta base and must be applied beforehand).
		if !r.startedBefore(GateDeltaImport) {
			if err := precheckBaselineCounts(ctx, tgt, baselineMan); err != nil {
				tgt.Close()
				return fmt.Errorf("gate delta-import: %w", err)
			}
		}
		res, err := databundle.Import(ctx, databundle.ImportOptions{
			From: freezeDir, DSN: plan.Target.DSN, SQLitePath: plan.Target.SQLitePath,
			Merge: true, ReplaceDiverged: true, // window mode: the freeze is the source of truth
		})
		tgt.Close()
		if err != nil {
			return fmt.Errorf("gate delta-import: %w", err)
		}
		inserted, replaced, idem := int64(0), int64(0), int64(0)
		for _, t := range res.Tables {
			inserted += t.Inserted
			replaced += t.Replaced
			idem += t.Idempotent
		}
		// Tombstones: children before parents.
		var base *databundle.SourceIndex
		var tombs map[string][]string
		if repRaw, rerr := os.ReadFile(filepath.Join(r.dir, "delta-report.json")); rerr == nil {
			var dr DeltaReport
			if jerr := json.Unmarshal(repRaw, &dr); jerr == nil {
				tombs = dr.TombstoneKeys
			}
		}
		if len(tombs) > 0 {
			base, _, err = databundle.IndexBundle(ctx, plan.BaselineBundle)
			if err != nil {
				return fmt.Errorf("gate delta-import: baseline index for tombstones: %w", err)
			}
			th, err := OpenTarget(ctx, plan.Target)
			if err != nil {
				return fmt.Errorf("gate delta-import: tombstone target: %w", err)
			}
			applied, aerr := applyTombstones(ctx, th, tombs, base)
			th.Close()
			if aerr != nil {
				return fmt.Errorf("gate delta-import: tombstones: %w", aerr)
			}
			r.man.Tombstones = applied
		}
		// The freeze anchor — the reverse-delta base (rollback's evidence).
		var anchorPath = filepath.Join(r.dir, "anchor.json")
		var idx *databundle.SourceIndex
		if plan.Target.DSN != "" {
			idx, err = databundle.IndexPostgres(ctx, plan.Target.DSN)
		} else {
			idx, err = databundle.IndexSQLiteFile(ctx, plan.Target.SQLitePath)
		}
		if err != nil {
			return fmt.Errorf("gate delta-import: anchor read: %w", err)
		}
		if _, err := writeAnchor(anchorPath, r.man.RunID, idx); err != nil {
			return fmt.Errorf("gate delta-import: anchor: %w", err)
		}
		r.man.AnchorFile = "anchor.json"
		r.finish(GateDeltaImport, "completed", fmt.Sprintf("delta applied: %d inserted, %d replaced, %d idempotent, %d tombstoned rows deleted; anchor written", inserted, replaced, idem, sumTomb(r.man.Tombstones)), "anchor.json")
	}

	// 5 store-adoption — the DM06 FK drops on the real database ----------
	if plan.Schema.ApplyStoreMigrations && !r.completed(GateStoreAdoption) {
		r.begin(GateStoreAdoption)
		pool, err := pgxpool.New(ctx, plan.EffectiveStoreDSN())
		if err != nil {
			return fmt.Errorf("gate store-adoption: open: %w", err)
		}
		if err := storemig.Migrate(ctx, pool); err != nil {
			pool.Close()
			return fmt.Errorf("gate store-adoption: store migrations (incl. the DM06 FK drops): %w", err)
		}
		pool.Close()
		r.finish(GateStoreAdoption, "completed", "store migrations applied on the adopted database (DM06 FK drops in-window)")
	}

	// 6 config-switch -----------------------------------------------------
	if !r.completed(GateConfigSwitch) {
		r.begin(GateConfigSwitch)
		backupPath, backup, err := backupConfig(r.dir, plan.ConfigSwitch.Path)
		if err != nil {
			return fmt.Errorf("gate config-switch: backup: %w", err)
		}
		path := backup.Path
		verdicts, err := switchEnvCompat(plan.ConfigSwitch)
		if err != nil {
			return fmt.Errorf("gate config-switch: %w", err)
		}
		if err := applySwitch(ConfigSwitch{Path: path, Set: plan.ConfigSwitch.Set, Unset: plan.ConfigSwitch.Unset}); err != nil {
			return fmt.Errorf("gate config-switch: %w", err)
		}
		setDigest := map[string]string{}
		for k, v := range plan.ConfigSwitch.Set {
			setDigest[k] = digestValue(v)
		}
		r.man.ConfigBackup = filepath.Base(backupPath)
		r.man.Switch = &SwitchRecord{
			Path: path, Unset: plan.ConfigSwitch.Unset, Set: setDigest,
			BackupFile: filepath.Base(backupPath), EnvCompat: verdicts, At: nowTS(),
		}
		r.finish(GateConfigSwitch, "completed", fmt.Sprintf("config switched atomically (%d set, %d unset; backup %s)", len(setDigest), len(plan.ConfigSwitch.Unset), filepath.Base(backupPath)), filepath.Base(backupPath))
	}

	// 7 restart — without mutating workers --------------------------------
	if !r.completed(GateRestart) {
		r.begin(GateRestart)
		if err := runCommand(ctx, plan.RestartCommand, filepath.Join(r.dir, "restart.log"), plan.CommandTimeout()); err != nil {
			return fmt.Errorf("gate restart: %w", err)
		}
		if err := runChecks(ctx, plan.RestartChecks); err != nil {
			return fmt.Errorf("gate restart: %w", err)
		}
		r.finish(GateRestart, "completed", fmt.Sprintf("restart OK (%d checks green)", len(plan.RestartChecks)))
	}

	// 8 shadow — the window's last data check ------------------------------
	if plan.FinalShadow && !r.completed(GateShadow) {
		r.begin(GateShadow)
		out := filepath.Join(r.dir, "shadow-report.json")
		res, err := databundle.Shadow(ctx, databundle.ShadowOptions{
			SourceDSN: plan.SourceDSN, DSN: plan.Target.DSN, SQLitePath: plan.Target.SQLitePath, Out: out,
		})
		if err != nil {
			return fmt.Errorf("gate shadow: %w", err)
		}
		r.man.ShadowReport = "shadow-report.json"
		if !res.OK {
			return fmt.Errorf("gate shadow: UNEXPECTED DEVIATIONS — the window refuses to re-enable workers over a red shadow (report: %s)", out)
		}
		r.finish(GateShadow, "completed", "shadow re-run green (zero unexpected deviations)", "shadow-report.json")
	}

	// 9 reenable — staged worker re-enable ---------------------------------
	for _, stage := range plan.ReenableStages {
		gate := GatePrefixReenable + stage.Name
		if r.completed(gate) {
			continue
		}
		r.begin(gate)
		if len(stage.ConfigSet) > 0 {
			path := plan.ConfigSwitch.Path
			if path == "" {
				p, err := configDefaultPath()
				if err != nil {
					return fmt.Errorf("gate %s: config path: %w", gate, err)
				}
				path = p
			}
			if err := applySwitch(ConfigSwitch{Path: path, Set: stage.ConfigSet}); err != nil {
				return fmt.Errorf("gate %s: config: %w", gate, err)
			}
		}
		if err := runCommand(ctx, stage.Command, filepath.Join(r.dir, "reenable-"+stage.Name+".log"), plan.CommandTimeout()); err != nil {
			return fmt.Errorf("gate %s: command: %w", gate, err)
		}
		if err := runChecks(ctx, stage.Checks); err != nil {
			return fmt.Errorf("gate %s: checks: %w", gate, err)
		}
		r.finish(gate, "completed", fmt.Sprintf("stage %s re-enabled (%d checks green)", stage.Name, len(stage.Checks)))
	}
	return nil
}

// precheckBaselineCounts asserts the target still sits at the baseline
// row counts (the delta's base state) before the first import entry.
func precheckBaselineCounts(ctx context.Context, tgt *TargetHandle, baselineMan *databundle.Manifest) error {
	for _, tm := range baselineMan.Tables {
		if tgt.SQLitePath != "" && !sqliteCarriedByName(tm.Name) {
			continue
		}
		n, err := tgt.count(ctx, tm.Name)
		if err != nil {
			return fmt.Errorf("count %s: %w", tm.Name, err)
		}
		if n != tm.Count {
			return fmt.Errorf("table %s: target carries %d rows, the baseline bundle pins %d — the delta base is violated (import the baseline first, or re-baseline; the window does not improvise)", tm.Name, n, tm.Count)
		}
	}
	return nil
}

func sqliteCarriedByName(name string) bool {
	for _, spec := range databundle.LibraryTables {
		if spec.Name == name {
			return spec.SQLite
		}
	}
	return false
}

func sumTomb(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

func completedList(r *run) string {
	var out []string
	for _, g := range r.man.Gates {
		if g.Status == "completed" {
			out = append(out, g.Gate)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return joinStrings(out, ", ")
}

func configDefaultPath() (string, error) {
	p, err := configstore.DefaultPath()
	if err != nil {
		return "", err
	}
	return p, nil
}

func currentGateOf(r *run) string {
	for i := len(r.man.Gates) - 1; i >= 0; i-- {
		if r.man.Gates[i].Status == "started" {
			return r.man.Gates[i].Gate
		}
	}
	return "run"
}
