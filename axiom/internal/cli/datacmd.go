// datacmd.go — `axiom data` (DM03 #312 / DM04 #313): the operator
// surface over the backend-neutral bundle format.
//
//	axiom data export  --component library --dsn URL --out DIR
//	axiom data import  --component library --from DIR (--dsn URL | --sqlite PATH) [--merge]
//	axiom data verify  --component library --from DIR (--dsn URL | --sqlite PATH) [--json]
//	axiom data shadow  --component library --source-dsn URL (--dsn URL | --sqlite PATH) --out REPORT [--json]
//
// Migration commands against mirror copies — the DSN is ALWAYS an
// explicit flag (never ambient runtime config: these tools point at
// arbitrary restored copies, and the import deliberately runs under
// the DM07 DML-only library role against an already-migrated target).
// Output discipline: tables, columns, counts, digests, PK identifiers —
// never row values (document texts), never secret/DSN values.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/databundle"
)

func cmdData(name string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s data <export|import|verify|shadow> --component library [flags]\n", name)
		return exitUsage
	}
	switch args[0] {
	case "export":
		return cmdDataExport(name, args[1:])
	case "import":
		return cmdDataImport(name, args[1:])
	case "verify":
		return cmdDataVerify(name, args[1:])
	case "shadow":
		return cmdDataShadow(name, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "%s data: unknown subcommand %q (export | import | verify | shadow)\n", name, args[0])
		return exitUsage
	}
}

// dataFlags is the shared flag vocabulary of the data family.
type dataFlags struct {
	component string
	dsn       string
	sqlite    string
	from      string
	out       string
	merge     bool
	json      bool
}

func parseDataFlags(name, verb string, args []string) (dataFlags, int) {
	var f dataFlags
	fs := flag.NewFlagSet("data "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&f.component, "component", "", "component to operate on (library)")
	fs.StringVar(&f.dsn, "dsn", "", "PostgreSQL DSN (export source; import/verify target)")
	fs.StringVar(&f.sqlite, "sqlite", "", "library.sqlite path (import/verify target)")
	fs.StringVar(&f.from, "from", "", "bundle directory (import/verify)")
	fs.StringVar(&f.out, "out", "", "bundle output directory (export)")
	fs.BoolVar(&f.merge, "merge", false, "allow import into a non-empty target")
	fs.BoolVar(&f.json, "json", false, "machine-readable output (verify)")
	if err := fs.Parse(args); err != nil {
		return f, exitUsage
	}
	if f.component != "library" {
		fmt.Fprintf(os.Stderr, "%s data %s: --component library is required (this build carries the library component only)\n", name, verb)
		return f, exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s data %s: unexpected argument %q\n", name, verb, fs.Arg(0))
		return f, exitUsage
	}
	return f, exitOK
}

func cmdDataExport(name string, args []string) int {
	f, code := parseDataFlags(name, "export", args)
	if code != exitOK {
		return code
	}
	if f.dsn == "" || f.out == "" {
		fmt.Fprintf(os.Stderr, "%s data export: --dsn and --out are required\n", name)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := databundle.Export(ctx, "library", databundle.ExportOptions{DSN: f.dsn, Out: f.out})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s data export: %s\n", name, err)
		return exitFailure
	}
	man := res.Manifest
	fmt.Printf("bundle: %s (format %s v%d, component %s)\n", f.out, man.Format, man.FormatVersion, man.Component)
	fmt.Printf("source: %s / %s, cutoff %s\n", man.Source.Build, man.Source.Engine, man.Source.ExportCutoff)
	for _, l := range sortedLedgers(man.Source.Migrations) {
		fmt.Printf("migrations: %s: %d applied\n", l, len(man.Source.Migrations[l]))
	}
	for _, t := range man.Tables {
		fmt.Printf("table %-32s %8d rows  %d batch(es)  rows-sha256 %s…\n", t.Name, t.Count, len(t.Batches), t.RowsSHA256[:12])
	}
	for _, w := range man.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	return exitOK
}

func cmdDataImport(name string, args []string) int {
	f, code := parseDataFlags(name, "import", args)
	if code != exitOK {
		return code
	}
	if f.from == "" || (f.dsn == "" && f.sqlite == "") || (f.dsn != "" && f.sqlite != "") {
		fmt.Fprintf(os.Stderr, "%s data import: --from and exactly one of --dsn / --sqlite are required\n", name)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	res, err := databundle.Import(ctx, databundle.ImportOptions{
		From: f.from, DSN: f.dsn, SQLitePath: f.sqlite, Merge: f.merge,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s data import: %s\n", name, err)
		return exitFailure
	}
	fmt.Printf("import: bundle %s → %s\n", f.from, res.Engine)
	for _, t := range res.Tables {
		switch {
		case t.Skipped:
			fmt.Printf("table %-32s SKIPPED (outside the target namespace)\n", t.Table)
		default:
			fmt.Printf("table %-32s %8d inserted  %8d idempotent-skip\n", t.Table, t.Inserted, t.Idempotent)
		}
	}
	for _, w := range res.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	return exitOK
}

func cmdDataVerify(name string, args []string) int {
	f, code := parseDataFlags(name, "verify", args)
	if code != exitOK {
		return code
	}
	if f.from == "" || (f.dsn == "" && f.sqlite == "") || (f.dsn != "" && f.sqlite != "") {
		fmt.Fprintf(os.Stderr, "%s data verify: --from and exactly one of --dsn / --sqlite are required\n", name)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := databundle.Verify(ctx, databundle.VerifyOptions{From: f.from, DSN: f.dsn, SQLitePath: f.sqlite})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s data verify: %s\n", name, err)
		return exitFailure
	}
	if f.json {
		out, jerr := json.MarshalIndent(res, "", "  ")
		if jerr != nil {
			fmt.Fprintf(os.Stderr, "%s data verify: %s\n", name, jerr)
			return exitFailure
		}
		fmt.Println(string(out))
	} else {
		fmt.Printf("verify: bundle %s → %s\n", f.from, res.Engine)
		for _, t := range res.Tables {
			verdict := "OK"
			if !t.CountOK || !t.DigestOK {
				verdict = "MISMATCH"
			}
			fmt.Printf("table %-32s %8d rows  %s  %s\n", t.Table, t.CountGot, digestMark(t.DigestOK), verdict)
			if t.JSONBVerdict != "" {
				fmt.Printf("       %-32s %s\n", "", t.JSONBVerdict)
			}
		}
		for _, fk := range res.FKs {
			fmt.Printf("fk     %-32s %-32s orphans %d\n", fk.Table, fk.Constraint, fk.Orphans)
		}
		for _, p := range res.Pragmas {
			fmt.Printf("pragma: %s\n", p)
		}
		for _, w := range res.Warnings {
			fmt.Printf("warning: %s\n", w)
		}
	}
	if res.OK {
		fmt.Printf("verify: OK\n")
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "%s data verify: FAILED\n", name)
	return exitFailure
}

func digestMark(ok bool) string {
	if ok {
		return "digest OK"
	}
	return "DIGEST MISMATCH"
}

// cmdDataShadow — the DM08 shadow-read (#317): the legacy mirror copy
// against the imported Library copy, full data set, explicit
// normalization allowlist, every other deviation red. Exit 1 on any
// unexpected deviation — the cutover window's last check.
func cmdDataShadow(name string, args []string) int {
	var component, sourceDSN, dsn, sqlite, out string
	var jsonOut bool
	var maxSamples int
	fs := flag.NewFlagSet("data shadow", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&component, "component", "", "component to operate on (library)")
	fs.StringVar(&sourceDSN, "source-dsn", "", "PostgreSQL DSN of the legacy mirror copy (the pull-point source)")
	fs.StringVar(&dsn, "dsn", "", "PostgreSQL DSN of the imported copy (target)")
	fs.StringVar(&sqlite, "sqlite", "", "imported library.sqlite path (target)")
	fs.StringVar(&out, "out", "", "shadow report JSON path")
	fs.IntVar(&maxSamples, "max-samples", 0, "per-surface sample rows in the report (0 = package default)")
	fs.BoolVar(&jsonOut, "json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if component != "library" {
		fmt.Fprintf(os.Stderr, "%s data shadow: --component library is required (this build carries the library component only)\n", name)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s data shadow: unexpected argument %q\n", name, fs.Arg(0))
		return exitUsage
	}
	if sourceDSN == "" || (dsn == "") == (sqlite == "") {
		fmt.Fprintf(os.Stderr, "%s data shadow: --source-dsn and exactly one of --dsn / --sqlite are required\n", name)
		return exitUsage
	}
	if out == "" {
		fmt.Fprintf(os.Stderr, "%s data shadow: --out (report artifact path) is required — the run must leave evidence\n", name)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := databundle.Shadow(ctx, databundle.ShadowOptions{
		SourceDSN: sourceDSN, DSN: dsn, SQLitePath: sqlite,
		Out: out, MaxSamples: maxSamples,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s data shadow: %s\n", name, err)
		return exitFailure
	}
	if jsonOut {
		b, jerr := json.MarshalIndent(res, "", "  ")
		if jerr != nil {
			fmt.Fprintf(os.Stderr, "%s data shadow: %s\n", name, jerr)
			return exitFailure
		}
		fmt.Println(string(b))
	} else {
		fmt.Printf("shadow: source %s @ cutoff %s → target %s (%s)\n",
			res.SourceEngine, res.SourceCutoff, res.TargetEngine, res.TargetMode)
		for _, t := range res.Tables {
			switch t.Status {
			case "skipped":
				fmt.Printf("table %-32s %-8s %s\n", t.Table, t.Status, t.Note)
			default:
				fmt.Printf("table %-32s %8d compared  %8d equal  %8d normalized  %8d unexpected",
					t.Table, t.Compared, t.Equal, t.Normalized, t.Unexpected)
				if t.MissingOnTarget > 0 || t.ExtraOnTarget > 0 {
					fmt.Printf("  (+%d structural: %d missing / %d extra)", t.MissingOnTarget+t.ExtraOnTarget, t.MissingOnTarget, t.ExtraOnTarget)
				}
				fmt.Println()
				for _, s := range t.Samples {
					printRowDiff(t.Table, s)
				}
			}
		}
		for _, a := range res.Allowlist {
			if a.Applied > 0 {
				fmt.Printf("allowlist %-20s %8d absorbed  (%s)\n", a.ID, a.Applied, a.Scope)
			}
		}
		for _, w := range res.Warnings {
			fmt.Printf("warning: %s\n", w)
		}
	}
	// --json output is PURE: the report object on stdout and nothing
	// after it — trailing human lines stay behind the flag (or on
	// stderr), so `… --json | jq .` parses a green run.
	if !jsonOut {
		fmt.Printf("report: %s\n", out)
	}
	if res.OK {
		compared, skipped := 0, 0
		for _, t := range res.Tables {
			if t.Status == "skipped" {
				skipped++
			} else {
				compared++
			}
		}
		if !jsonOut {
			if skipped > 0 {
				fmt.Printf("shadow: OK — %d/%d surfaces compared, %d skipped (target scope), zero unexpected deviations\n",
					compared, compared+skipped, skipped)
			} else {
				fmt.Printf("shadow: OK (zero unexpected deviations)\n")
			}
		}
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "%s data shadow: FAILED — unexpected deviations, see %s\n", name, out)
	return exitFailure
}

// printRowDiff prints one sample deviation — table, key, columns and
// per-side value digests; never row values (no-leaks discipline).
func printRowDiff(table string, d databundle.RowDiff) {
	switch d.Kind {
	case "field_diff":
		for _, f := range d.Fields {
			rule := "UNEXPECTED"
			if f.Rule != "" {
				rule = "normalized: " + f.Rule
			}
			fmt.Printf("       diff %-32s key %s  column %s  source %s  target %s  [%s]\n",
				table, d.Key, f.Column, f.Source, f.Target, rule)
		}
	default:
		note := d.Note
		if note == "" {
			note = d.Kind
		}
		fmt.Printf("       diff %-32s key %s  %s\n", table, d.Key, note)
	}
}

func sortedLedgers(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// stable order: the core ledger first, then component ledgers
	slices.Sort(out)
	if i := slices.Index(out, "schema_migrations"); i > 0 {
		out = append([]string{"schema_migrations"}, append(out[:i:i], out[i+1:]...)...)
	}
	return out
}
