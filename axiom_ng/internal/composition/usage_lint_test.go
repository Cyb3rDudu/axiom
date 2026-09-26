// usage_lint_test.go — the #298 domain-purity gate: internal packages may
// not read the process environment, spawn processes, or resolve service
// discovery. The env Erlaubnisraum is EXACTLY: internal/config, the
// composition root, and the F01 baseline scaffolding — plus one counted
// legacy env read in search.
//
// The allowlist is the #298 debt ledger: every entry names a Bestandsübeltäter
// with an abatement owner (F06–F10). F04 does not rewrite them; it makes the
// debt VISIBLE and SHRINKING — a new violation anywhere fails this test.
//
// Teeth witnesses: TestUsageLintDetectorCatchesPlantedViolations plants real
// violations in a probe tree and asserts they are caught; the red-proof
// transcript for the REAL tree (one os.Getenv injected into internal/search,
// suite red, reverted) is in the #298 issue comment.
package composition

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envAllowance/execAllowance: how many violations a path may carry. A
// package-dir prefix grants the package-wide allowance; a file entry grants
// a counted one. Everything not listed has an allowance of ZERO.
//
// Allowlist (#298 debt ledger, abatement F06–F10):
//
//	internal/config       env ∞   the sanctioned config reader (by design)
//	internal/composition  env ∞   the sanctioned composition root (#298)
//	internal/baseline     env ∞   F01 scaffolding knobs (test-only package)
//	internal/cli/cli.go   env 1   debug-bind opt-out (#205 §5; F05 #299)
//	internal/cli/modes.go env 2   retention-mode env fallbacks (#290;
//	                              abatement F13 — config store reader)
//	internal/search/search.go env 1  AXIOM_OS_INDEX dev override — F06 moves
//	                              it behind config
//	internal/library/repair exec ∞ the RepairExecutor port's LOCAL binding
//	                              (process-group exec) — F08 (#302) moved it
//	                              to its final home under the Library
//	internal/backfill     exec ∞  legacy CLI python bridge — F06–F10
//	                              abatement
var packageEnvAllowance = map[string]int{
	"internal/config/":      math.MaxInt,
	"internal/composition/": math.MaxInt,
	"internal/baseline/":    math.MaxInt,
}

var packageExecAllowance = map[string]int{
	"internal/library/repair/": math.MaxInt,
	"internal/backfill/":       math.MaxInt,
}

var fileEnvAllowance = map[string]int{
	"internal/search/search.go": 1, // AXIOM_OS_INDEX override; F06 target
	// internal/cli is the operator command surface (F05 #299) — counted
	// per file, not package-wide: the debug-bind opt-out (cli.go) and the
	// retention mode's env fallbacks (modes.go, pre-F05 surface). A NEW
	// env read anywhere else in cli goes red. Abatement: F13 moves the
	// config surface into its store-backed reader.
	"internal/cli/cli.go":   1,
	"internal/cli/modes.go": 2,
}

// discoveryImports: import paths that ARE service discovery (a domain
// package resolving peers at runtime). Vacuously green today — the list
// exists so F11+ cannot add one silently.
var discoveryImports = map[string]bool{}

type usageViolation struct {
	path  string // relative to the module root (axiom_ng/)
	line  int
	what  string // e.g. "os.Getenv"
	owner string // abatement pointer for allowlisted classes, "" otherwise
}

func (v usageViolation) String() string {
	if v.owner != "" {
		return fmt.Sprintf("%s:%d %s (allowlisted, abatement %s)", v.path, v.line, v.what, v.owner)
	}
	return fmt.Sprintf("%s:%d %s", v.path, v.line, v.what)
}

// findUsageViolations scans every non-test .go file under root/internal for
// banned environment/process/discovery usage beyond the allowlist.
func findUsageViolations(moduleRoot string) []usageViolation {
	var violations []usageViolation
	_ = filepath.WalkDir(filepath.Join(moduleRoot, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		envSeen := 0
		execSeen := 0
		// Known blind spot (honest debt-gate limit): the match below is a
		// selector call on an identifier literally named `os`/`exec` — an
		// aliased import (`import o "os"`) or an indirect assignment
		// (`fn := os.Getenv`) escapes it. New code reviews + F06's move of
		// the remaining reads behind config shrink the surface this gate
		// has to be perfect on.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" && pkg.Name != "exec" {
				return true
			}
			switch {
			case pkg.Name == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv" || sel.Sel.Name == "Setenv"):
				envSeen++
				if envSeen > envAllowance(rel) {
					violations = append(violations, usageViolation{path: rel, line: fset.Position(call.Lparen).Line, what: "os." + sel.Sel.Name})
				}
			case pkg.Name == "exec" && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"):
				execSeen++
				if execSeen > execAllowance(rel) {
					violations = append(violations, usageViolation{path: rel, line: fset.Position(call.Lparen).Line, what: "exec." + sel.Sel.Name})
				}
			}
			return true
		})

		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if discoveryImports[p] {
				violations = append(violations, usageViolation{path: rel, line: fset.Position(imp.Pos()).Line, what: "discovery import " + p})
			}
		}
		return nil
	})
	return violations
}

// envAllowance resolves the effective env-read allowance for a file path
// relative to the module root.
func envAllowance(rel string) int {
	if n, ok := fileEnvAllowance[rel]; ok {
		return n
	}
	return prefixAllowance(packageEnvAllowance, rel)
}

func execAllowance(rel string) int {
	return prefixAllowance(packageExecAllowance, rel)
}

func prefixAllowance(table map[string]int, rel string) int {
	for prefix, n := range table {
		if strings.HasPrefix(rel, prefix) {
			return n
		}
	}
	return 0
}

// TestDomainPackagesStayEnvAndExecFree — the standing gate. A violation
// beyond the allowlist is a NEW debt entry: fix it, or (for a conscious
// step) extend the allowlist table above AND the #298 ledger comment with
// its abatement owner.
func TestDomainPackagesStayEnvAndExecFree(t *testing.T) {
	violations := findUsageViolations("../..")
	if len(violations) > 0 {
		t.Fatalf("domain packages carry banned env/exec/discovery usage beyond the #298 allowlist (env Erlaubnisraum: internal/config, internal/composition, internal/baseline + 1 counted search read; exec: library/repair local worker binding + backfill):\n\t%s",
			strings.Join(violationStrings(violations), "\n\t"))
	}
}

func violationStrings(violations []usageViolation) []string {
	out := make([]string, len(violations))
	for i, v := range violations {
		out[i] = v.String()
	}
	return out
}

// TestUsageLintDetectorCatchesPlantedViolations — the teeth witness: the
// SAME detector that guards the real tree catches a planted os.Getenv in a
// probe copy of internal/search (the #298 DoD sonde) and a planted
// exec.Command, while a clean probe copy stays green.
func TestUsageLintDetectorCatchesPlantedViolations(t *testing.T) {
	probe := t.TempDir()
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(probe, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		mkdir(filepath.Dir(rel))
		if err := os.WriteFile(filepath.Join(probe, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Probe tree mimicking the module layout.
	write("internal/search/search.go", "package search\n\nfunc probe() string { return \"\" }\n")
	clean := findUsageViolations(probe)
	if len(clean) != 0 {
		t.Fatalf("clean probe must yield zero violations, got %v", violationStrings(clean))
	}

	// Sonde 1: one os.Getenv in a NEW file of internal/search — the exact
	// #298 DoD probe (a new env read in the search package goes red; the
	// ONE legacy read in search.go itself stays green per the counted
	// allowance — sonde 3 proves that boundary).
	write("internal/search/planted.go", "package search\n\nimport \"os\"\n\nfunc probe() string { return os.Getenv(\"AXIOM_PLANTED\") }\n")
	v := findUsageViolations(probe)
	if len(v) != 1 || !strings.Contains(v[0].what, "Getenv") || !strings.Contains(v[0].path, "search") {
		t.Fatalf("planted os.Getenv in internal/search must be caught exactly once, got %v", violationStrings(v))
	}

	// Sonde 2: an exec.Command in a domain package.
	write("internal/dispatcher/planted.go", "package dispatcher\n\nimport \"os/exec\"\n\nfunc run() { exec.Command(\"ls\") }\n")
	v = findUsageViolations(probe)
	if len(v) != 2 {
		t.Fatalf("planted exec.Command must be a second violation, got %v", violationStrings(v))
	}

	// Sonde 3: the counted search allowance is real — the one LEGACY read
	// stays green, a second one goes red.
	write("internal/search/search.go", "package search\n\nimport \"os\"\n\nvar a = os.Getenv(\"AXIOM_OS_INDEX\")\nvar b = os.Getenv(\"AXIOM_SECOND_READ\")\n")
	v = findUsageViolations(probe)
	if len(v) != 3 || !strings.Contains(v[2].what, "Getenv") {
		t.Fatalf("a second env read in search.go must exceed the counted allowance, got %v", violationStrings(v))
	}
}
