// zotero_import_lint_test.go — the #301 Zotero seam gate: imports of the
// Zotero adapter package (internal/zoteroprovider, formerly
// internal/zotero — every Zotero HTTP interaction lives there) are
// allowed ONLY inside the adapter itself and the documented exception
// list. The list is the F07 debt ledger: every entry names its abatement
// step (F08–F10); the list must SHRINK, never grow.
//
// Gate shape (hardened after the first review round): the walk covers the
// WHOLE module (cmd/ included, _test.go files included — a test importing
// the adapter is still a Zotero path), only vendor/ and testdata/ are
// exempt, and walk/parse errors are FATAL — a fail-open detector would
// green-light exactly the drift it exists to catch.
//
// Teeth witness: TestZoteroImportLintCatchesPlantedImports plants probe
// imports in a copy of internal/library (the #301 DoD sonde), in cmd/,
// and as a _test.go file — each must go red.
package composition

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const zoteroAdapterImport = "github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"

// zoteroImportAllowance: paths (relative to the module root,
// slash-separated, directory prefixes) allowed to import the Zotero
// adapter beyond the adapter itself. Every entry is named debt with an
// abatement owner:
//
//	internal/zoteroprovider  the adapter (unrestricted — this IS the seam)
//	internal/composition     the wiring root: constructs the adapter and
//	                         hands it to the Library ports (permanent —
//	                         construction is exactly the root's job)
//	internal/sync            the Zotero mirror read path; F09 (#303) moves
//	                         sync behind Library-owned ports
//	internal/repair          the repair write track; F08 moves repair
//	                         ownership onto the Library single-writer
//	internal/repo            mirror persistence (CanonicalItem types);
//	                         F09's revision-typed intake removes the types
//	internal/server          the Zotero health check + repair write
//	                         gateway surface; leaves with F08/F11
//	internal/fixerinvoker    the fixer apply deps (repair track, F08)
//	internal/baseline        F01 scaffolding probes the frozen surface
//	                         (test-only package)
//	cmd/meta-backfill        the legacy CLI bridge; leaves with the
//	                         backfill retirement
var zoteroImportAllowance = []string{
	"internal/zoteroprovider/",
	"internal/composition/",
	"internal/sync/",
	"internal/repair/",
	"internal/repo/",
	"internal/server/",
	"internal/fixerinvoker/",
	"internal/baseline/",
	"cmd/meta-backfill/",
}

type zoteroImportViolation struct {
	path string
	line int
}

// findZoteroImportViolations scans the whole module under moduleRoot:
// every .go file (tests included), vendor/ and testdata/ exempt. Walk
// and parse errors abort the scan loudly — the detector must fail
// closed, never report a partial tree as clean.
func findZoteroImportViolations(moduleRoot string) ([]zoteroImportViolation, error) {
	var violations []zoteroImportViolation
	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", ".git", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(moduleRoot, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) != zoteroAdapterImport {
				continue
			}
			if !zoteroImportAllowed(rel) {
				violations = append(violations, zoteroImportViolation{path: rel, line: fset.Position(imp.Pos()).Line})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return violations, nil
}

func zoteroImportAllowed(rel string) bool {
	for _, prefix := range zoteroImportAllowance {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// TestZoteroImportsOnlyBehindTheAdapterSeam — the standing gate. A new
// violation is new debt: either route the Zotero access through the
// Library ports (the F07 answer) or extend the ledger above AND the #301
// design comment with its abatement owner.
func TestZoteroImportsOnlyBehindTheAdapterSeam(t *testing.T) {
	violations, err := findZoteroImportViolations("../..")
	if err != nil {
		t.Fatalf("zotero import lint scan failed (fail closed): %v", err)
	}
	if len(violations) == 0 {
		return
	}
	var out []string
	for _, v := range violations {
		out = append(out, fmt.Sprintf("%s:%d imports %s outside the adapter seam (route through the Library ports or document the exception in the F07 ledger)", v.path, v.line, zoteroAdapterImport))
	}
	t.Fatalf("Zotero imports outside the adapter package (F07 #301 seam; exceptions must shrink F08–F10):\n\t%s", strings.Join(out, "\n\t"))
}

// TestZoteroImportLintCatchesPlantedImports — the teeth witnesses: the
// SAME detector catches a planted adapter import in a probe copy of
// internal/library (the #301 DoD sonde), in cmd/ (the first-round blind
// spot), and inside a _test.go file; the allowed probe (internal/sync)
// stays green.
func TestZoteroImportLintCatchesPlantedImports(t *testing.T) {
	probe := t.TempDir()
	write := func(rel, body string) {
		if err := os.MkdirAll(filepath.Join(probe, filepath.FromSlash(filepath.Dir(rel))), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(probe, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/library/service.go", "package library\n\nfunc a() {}\n")
	v, err := findZoteroImportViolations(probe)
	if err != nil || len(v) != 0 {
		t.Fatalf("clean probe must yield zero violations, got %v (%v)", v, err)
	}

	// Sonde 1 (#301 DoD): a direct adapter import in internal/library.
	write("internal/library/planted.go", "package library\n\nimport _ \""+zoteroAdapterImport+"\"\n\nfunc b() {}\n")
	v, err = findZoteroImportViolations(probe)
	if err != nil || len(v) != 1 || !strings.Contains(v[0].path, "library") {
		t.Fatalf("a direct Zotero import in internal/library must be caught exactly once, got %v (%v)", v, err)
	}

	// Sonde 2: cmd/ is scanned (the first-round blind spot).
	write("cmd/plantedtool/main.go", "package main\n\nimport _ \""+zoteroAdapterImport+"\"\n\nfunc main() {}\n")
	v, err = findZoteroImportViolations(probe)
	if err != nil || len(v) != 2 || !hasViolationIn(v, "cmd/") {
		t.Fatalf("an adapter import in cmd/ must be caught, got %v (%v)", v, err)
	}

	// Sonde 3: _test.go files are Zotero paths too.
	write("internal/library/planted_test.go", "package library\n\nimport _ \""+zoteroAdapterImport+"\"\n\nfunc c() {}\n")
	v, err = findZoteroImportViolations(probe)
	if err != nil || len(v) != 3 || !hasViolationIn(v, "_test.go") {
		t.Fatalf("an adapter import in a _test.go must be caught, got %v (%v)", v, err)
	}

	// The documented sync exception stays green.
	write("internal/sync/allowed.go", "package sync\n\nimport _ \""+zoteroAdapterImport+"\"\n\nfunc d() {}\n")
	v, err = findZoteroImportViolations(probe)
	if err != nil || len(v) != 3 {
		t.Fatalf("the documented sync exception must stay green, got %v (%v)", v, err)
	}

	// vendor/ and testdata/ are exempt (generated/vendored trees).
	write("vendor/planted/main.go", "package vendored\n\nimport _ \""+zoteroAdapterImport+"\"\n")
	write("testdata/planted.go", "package testdata\n\nimport _ \""+zoteroAdapterImport+"\"\n")
	v, err = findZoteroImportViolations(probe)
	if err != nil || len(v) != 3 {
		t.Fatalf("vendor/testdata must be exempt, got %v (%v)", v, err)
	}
}

// hasViolationIn reports whether any violation's path contains sub
// (walk-order independence for the probe assertions).
func hasViolationIn(v []zoteroImportViolation, sub string) bool {
	for _, x := range v {
		if strings.Contains(x.path, sub) {
			return true
		}
	}
	return false
}
