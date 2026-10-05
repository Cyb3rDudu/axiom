// rollback.go — `axiom data rollback --run <dir>`: the way back, with
// the same gate discipline as the way forward.
//
//		pre-write  nothing new landed on the new store since the cutover:
//		           config revert + restart-old only (the cheap path — no
//		           reverse delta needed; the decision is evidenced, not
//		           assumed: the target is indexed and compared against the
//		           freeze anchor)
//		post-write new/changed Library rows exist on the new store: the
//		           reverse delta (target vs anchor) lands idempotently in
//		           cutover_shadow_* tables on the legacy database first,
//		           THEN config reverts and the old stack restarts — the
//		           reversed rows are catch-up evidence for the legacy
//	          system, never auto-merged into live tables
//
// The binary/name rollback (0.1.x binaries back, renames undone) is
// runbook territory — deployment is not owned by this tool; the run
// manifest records exactly which steps the tool performed so the
// operator's checklist resumes cleanly.
package cutover

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/Cyb3rDudu/axiom/axiom/internal/databundle"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RollbackOptions parameterize a rollback invocation.
type RollbackOptions struct {
	RunDir  string // the cutover run directory
	Confirm bool
	Logf    func(format string, args ...any)
}

func (o RollbackOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Rollback runs (or resumes) the rollback of a cutover run.
func Rollback(ctx context.Context, opts RollbackOptions) (*RollbackRecord, error) {
	r, err := loadRun(opts.RunDir)
	if err != nil {
		return nil, fmt.Errorf("rollback: %w", err)
	}
	if r.man.Kind != RunKindCutover {
		return nil, fmt.Errorf("rollback: %s is a %s run — rollback targets a cutover run", opts.RunDir, r.man.Kind)
	}
	if r.man.Switch == nil {
		return nil, fmt.Errorf("rollback: run %s never switched the config — nothing to roll back (resume the cutover with --run-dir, or discard the run)", r.man.RunID)
	}
	// The plan snapshot inside the run directory is the truth for a
	// rollback (the outer plan file may have moved on).
	plan, err := LoadPlan(filepath.Join(r.dir, r.man.PlanSnapshot))
	if err != nil {
		return nil, fmt.Errorf("rollback: plan snapshot: %w", err)
	}
	baselineMan, err := databundle.LoadManifest(plan.BaselineBundle)
	if err != nil {
		return nil, fmt.Errorf("rollback: baseline bundle: %w", err)
	}
	if r.man.AnchorFile == "" {
		return nil, fmt.Errorf("rollback: run %s carries no freeze anchor — the reverse delta has no base", r.man.RunID)
	}

	// A rollback invocation is ALWAYS a fresh pass over its gates:
	// every gate is safe to re-run (the reverse-delta apply is
	// idempotent per stable key, the config revert converges to the
	// backup, restart re-runs), and the pre/post-write decision is
	// RE-EVIDENCED each time — the target may have drifted further
	// since the last rollback. The audit log keeps every invocation's
	// history; rollback.json always describes the latest pass.
	rec := &RollbackRecord{Status: "running", StartedAt: nowTS()}
	if err := r.saveRollback(rec); err != nil {
		return nil, err
	}
	r.man.Rollback = rec

	if !opts.Confirm {
		opts.logf("rollback: run %s — DRY validation only; re-run with --require-confirmation to execute", r.man.RunID)
		opts.logf("rollback: cutover gates completed: %s", completedList(r))
		return rec, nil
	}

	if err := runRollbackGates(ctx, r, rec, plan, baselineMan, opts); err != nil {
		r.finish(rbCurrentGate(rec), "failed", err.Error())
		r.man.Rollback = rec
		_ = r.save()
		return rec, err
	}
	rec.Status = "completed"
	r.man.Rollback = rec
	if err := r.saveRollback(rec); err != nil {
		return rec, err
	}
	_ = r.save()
	return rec, nil
}

func runRollbackGates(ctx context.Context, r *run, rec *RollbackRecord, plan *Plan, baselineMan *databundle.Manifest, opts RollbackOptions) error {
	rbCompleted := func(gate string) bool {
		for _, g := range rec.Gates {
			if g.Gate == gate && g.Status == "completed" {
				return true
			}
		}
		return false
	}
	rbBegin := func(gate string) {
		for i := range rec.Gates {
			if rec.Gates[i].Gate == gate {
				rec.Gates[i].Status = "started"
				return
			}
		}
		rec.Gates = append(rec.Gates, GateRecord{Gate: gate, Status: "started", StartedAt: nowTS()})
		_ = r.saveRollback(rec)
	}
	rbFinish := func(gate, status, note string, evidence ...string) {
		for i := range rec.Gates {
			if rec.Gates[i].Gate == gate {
				rec.Gates[i].Status = status
				rec.Gates[i].FinishedAt = nowTS()
				rec.Gates[i].Note = note
				rec.Gates[i].Evidence = append(rec.Gates[i].Evidence, evidence...)
				_ = r.saveRollback(rec)
				r.audit(gate, status, note, evidence...)
				return
			}
		}
	}

	// 1 maintenance — stop the new stack's mutators ----------------------
	if !rbCompleted(RBGateMaintenance) {
		rbBegin(RBGateMaintenance)
		for i, cmd := range plan.Rollback.StopCommands {
			if err := runCommand(ctx, cmd, filepath.Join(r.dir, fmt.Sprintf("rb-stop-%d.log", i)), plan.CommandTimeout()); err != nil {
				return fmt.Errorf("rollback maintenance: stop command %d: %w", i, err)
			}
		}
		rbFinish(RBGateMaintenance, "completed", fmt.Sprintf("%d stop command(s) executed", len(plan.Rollback.StopCommands)))
	}

	// 2 reverse-delta — the pre/post-write decision, evidenced -----------
	if !rbCompleted(RBGateReverse) {
		rbBegin(RBGateReverse)
		anchor, err := loadAnchor(filepath.Join(r.dir, r.man.AnchorFile))
		if err != nil {
			return fmt.Errorf("rollback reverse-delta: %w", err)
		}
		var tgt *databundle.SourceIndex
		if plan.Target.DSN != "" {
			tgt, err = databundle.IndexPostgres(ctx, plan.Target.DSN)
		} else {
			tgt, err = databundle.IndexSQLiteFile(ctx, plan.Target.SQLitePath)
		}
		if err != nil {
			return fmt.Errorf("rollback reverse-delta: target read: %w", err)
		}
		diff := diffAgainstAnchor(anchor, tgt)
		if diff == 0 {
			rec.Mode = "pre-write"
			rbFinish(RBGateReverse, "completed",
				"pre-write path: the target is byte-identical to the freeze anchor (zero new/changed rows) — no reverse delta needed; the evidence is this comparison")
			opts.logf("rollback: PRE-WRITE path — target equals the freeze anchor, no reverse delta")
		} else {
			rec.Mode = "post-write"
			revDir := filepath.Join(r.dir, "reverse")
			rep, err := reverseDelta(revDir, anchor, tgt, baselineMan, r.man.RunID, opts.Logf)
			if err != nil {
				return fmt.Errorf("rollback reverse-delta: %w", err)
			}
			repRaw, _ := json.MarshalIndent(rep, "", "  ")
			repPath := filepath.Join(r.dir, "reverse-delta-report.json")
			if err := writeAtomic(repPath, append(repRaw, '\n'), 0o600); err != nil {
				return fmt.Errorf("rollback reverse-delta: report: %w", err)
			}
			rec.ReverseBundle = "reverse"
			rec.ReverseReport = "reverse-delta-report.json"

			// Load the reverse bundle's rows and land them in the
			// shadow tables on the legacy side, idempotently.
			revIdx, _, err := databundle.IndexBundle(ctx, revDir)
			if err != nil {
				return fmt.Errorf("rollback reverse-delta: index: %w", err)
			}
			shadowDSN := plan.Rollback.ShadowDSN
			if shadowDSN == "" {
				shadowDSN = plan.SourceDSN
			}
			pool, err := pgxpool.New(ctx, shadowDSN)
			if err != nil {
				return fmt.Errorf("rollback reverse-delta: shadow open: %w", err)
			}
			rbRunID := r.man.RunID + "-rb"
			applyRes, err := applyReverseDelta(ctx, pool, revIdx, rbRunID)
			pool.Close()
			if err != nil {
				return fmt.Errorf("rollback reverse-delta: shadow apply: %w", err)
			}
			applied, idem := int64(0), int64(0)
			for _, t := range applyRes.Tables {
				applied += t.Applied
				idem += t.Idempotent
			}
			rbFinish(RBGateReverse, "completed",
				fmt.Sprintf("post-write path: %d row(s) reversed into cutover_shadow_* tables (%d applied, %d idempotent from earlier runs) — tombstone keys are REPORTED, never auto-deleted", diff, applied, idem),
				"reverse/", "reverse-delta-report.json")
			opts.logf("rollback: POST-WRITE path — %d row(s) in shadow tables", applied)
		}
	}

	// 3 config-revert -----------------------------------------------------
	if !rbCompleted(RBGateConfigRevert) {
		rbBegin(RBGateConfigRevert)
		backupPath := filepath.Join(r.dir, r.man.ConfigBackup)
		backup, err := readBackup(backupPath)
		if err != nil {
			return fmt.Errorf("rollback config-revert: backup: %w", err)
		}
		if err := revertConfig(backup, r.man.Switch); err != nil {
			return fmt.Errorf("rollback config-revert: %w", err)
		}
		rbFinish(RBGateConfigRevert, "completed", fmt.Sprintf("config restored to the pre-switch state (backup %s)", filepath.Base(backupPath)))
	}

	// 4 restart-old ---------------------------------------------------------
	if !rbCompleted(RBGateRestart) {
		rbBegin(RBGateRestart)
		if err := runCommand(ctx, plan.Rollback.RestartCommand, filepath.Join(r.dir, "rb-restart.log"), plan.CommandTimeout()); err != nil {
			return fmt.Errorf("rollback restart: %w", err)
		}
		if err := runChecks(ctx, plan.Rollback.Checks); err != nil {
			return fmt.Errorf("rollback restart: %w", err)
		}
		rbFinish(RBGateRestart, "completed", fmt.Sprintf("old stack restarted (%d checks green)", len(plan.Rollback.Checks)))
	}
	return nil
}

// diffAgainstAnchor counts rows on the target that are new or changed
// relative to the freeze anchor (the pre/post-write discriminator).
func diffAgainstAnchor(anchor *Anchor, tgt *databundle.SourceIndex) int64 {
	am := anchor.anchorMap()
	var n int64
	for tname, rows := range tgt.Tables {
		digests := am[tname]
		for k, line := range rows.Rows {
			if d, ok := digests[k]; !ok || d != lineDigest(line) {
				n++
			}
		}
	}
	return n
}

func lineDigest(line string) string {
	return fmt.Sprintf("%x", sha256Sum([]byte(line)))
}

func rbCurrentGate(rec *RollbackRecord) string {
	for i := len(rec.Gates) - 1; i >= 0; i-- {
		if rec.Gates[i].Status == "started" {
			return rec.Gates[i].Gate
		}
	}
	return "rollback"
}
