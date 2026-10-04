// datacmd.go — `axiom data` (DM03 #312 / DM04 #313): the operator
// surface over the backend-neutral bundle format.
//
//	axiom data export  --component library --dsn URL --out DIR
//	axiom data import  --component library --from DIR (--dsn URL | --sqlite PATH) [--merge]
//	axiom data verify  --component library --from DIR (--dsn URL | --sqlite PATH) [--json]
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
		fmt.Fprintf(os.Stderr, "usage: %s data <export|import|verify> --component library [flags]\n", name)
		return exitUsage
	}
	switch args[0] {
	case "export":
		return cmdDataExport(name, args[1:])
	case "import":
		return cmdDataImport(name, args[1:])
	case "verify":
		return cmdDataVerify(name, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "%s data: unknown subcommand %q (export | import | verify)\n", name, args[0])
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
	if err != nil {
		return exitFailure
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
