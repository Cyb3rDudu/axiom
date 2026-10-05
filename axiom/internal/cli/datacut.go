// datacut.go — `axiom data cutover` and `axiom data rollback` (DM09
// #318): the window CLI over internal/cutover. The confirmation flag
// is load-bearing: without it both commands validate/dry-run only —
// no run directory, no DDL, no config row, no import, no shadow table
// ever moves without the operator's explicit --require-confirmation.
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Cyb3rDudu/axiom/axiom/internal/cutover"
)

// cmdDataCutover — the gated cutover window.
func cmdDataCutover(name string, args []string) int {
	var planPath, runDir string
	var confirm bool
	fs := flag.NewFlagSet("data cutover", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&planPath, "plan", "", "cutover plan manifest (JSON, 0600 — it carries DSNs and service commands)")
	fs.StringVar(&runDir, "run-dir", "", "existing run directory to RESUME (prints in the run output)")
	fs.BoolVar(&confirm, "require-confirmation", false, "execute the window (without it: validation only, zero mutations)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if planPath == "" {
		fmt.Fprintf(os.Stderr, "%s data cutover: --plan is required\n", name)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s data cutover: unexpected argument %q\n", name, fs.Arg(0))
		return exitUsage
	}
	ctx := context.Background() // the window owns its time; every gate bounds its own steps
	man, err := cutover.Cutover(ctx, cutover.Options{
		PlanPath: planPath, RunDir: runDir, Confirm: confirm,
		Logf: func(format string, args ...any) { fmt.Printf("cutover: "+format+"\n", args...) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s data cutover: %s\n", name, err)
		return exitFailure
	}
	if man != nil {
		if man.Status == "completed" {
			fmt.Printf("cutover: run %s COMPLETED — see the run directory for evidence\n", man.RunID)
		}
	}
	return exitOK
}

// cmdDataRollback — the way back (pre-write cheap path, post-write
// reverse delta into shadow tables).
func cmdDataRollback(name string, args []string) int {
	var runDir string
	var confirm bool
	fs := flag.NewFlagSet("data rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&runDir, "run", "", "the cutover RUN DIRECTORY to roll back")
	fs.BoolVar(&confirm, "require-confirmation", false, "execute the rollback (without it: validation only, zero mutations)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if runDir == "" {
		fmt.Fprintf(os.Stderr, "%s data rollback: --run (the cutover run directory) is required\n", name)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s data rollback: unexpected argument %q\n", name, fs.Arg(0))
		return exitUsage
	}
	rec, err := cutover.Rollback(context.Background(), cutover.RollbackOptions{
		RunDir: runDir, Confirm: confirm,
		Logf: func(format string, args ...any) { fmt.Printf("rollback: "+format+"\n", args...) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s data rollback: %s\n", name, err)
		return exitFailure
	}
	if confirm && rec != nil && rec.Status == "completed" {
		fmt.Printf("rollback: COMPLETED (mode %s) — binary/name rollback steps are runbook territory (see the cutover runbook)\n", rec.Mode)
	}
	return exitOK
}
