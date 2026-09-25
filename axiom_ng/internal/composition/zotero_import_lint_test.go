// zotero_import_lint_test.go — the #301 Zotero seam gate: imports of the
// Zotero adapter package (internal/zoteroprovider, formerly
// internal/zotero — every Zotero HTTP interaction lives there) are
// allowed ONLY inside the adapter itself and the documented exception
// list. The list is the F07 debt ledger: every entry names its abatement
// step (F08–F10); the list must SHRINK, never grow.
//
// Teeth witness: TestZoteroImportLintCatchesPlantedImports plants a probe
// import in a copy of internal/library — the exact #301 DoD sonde (a
// direct Zotero import outside the adapter package goes red).
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

// zoteroImportAllowance: package-dir prefixes (relative to the module
// root, slash-separated) allowed to import the Zotero adapter beyond the
// adapter itself. Every entry is named debt with an abatement owner:
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
//	internal/backfill        legacy CLI python bridge; abatement with the
//	                         backfill retirement
//	internal/baseline        F01 scaffolding probes the frozen surface
//	                         (test-only package)
var zoteroImportAllowance = []string{
	"internal/zoteroprovider/",
	"internal/composition/",
	"internal/sync/",
	"internal/repair/",
	"internal/repo/",
	"internal/server/",
	"internal/fixerinvoker/",
	"internal/backfill/",
	"internal/baseline/",
}

type zoteroImportViolation struct {
	path string
	line int
}

func findZoteroImportViolations(moduleRoot string) []zoteroImportViolation {
	var violations []zoteroImportViolation
	_ = filepath.WalkDir(filepath.Join(moduleRoot, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(moduleRoot, path)
		if rerr != nil {
			return nil
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
	return violations
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
	violations := findZoteroImportViolations("../..")
	if len(violations) == 0 {
		return
	}
	var out []string
	for _, v := range violations {
		out = append(out, fmt.Sprintf("%s:%d imports %s outside the adapter seam (route through the Library ports or document the exception in the F07 ledger)", v.path, v.line, zoteroAdapterImport))
	}
	t.Fatalf("Zotero imports outside the adapter package (F07 #301 seam; exceptions must shrink F08–F10):\n\t%s", strings.Join(out, "\n\t"))
}

// TestZoteroImportLintCatchesPlantedImports — the teeth witness: the SAME
// detector catches a planted adapter import in a probe copy of
// internal/library (the #301 DoD sonde), while the allowed probe
// (internal/sync) stays green.
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
	if v := findZoteroImportViolations(probe); len(v) != 0 {
		t.Fatalf("clean probe must yield zero violations, got %v", v)
	}
	write("internal/library/planted.go", "package library\n\nimport _ \""+zoteroAdapterImport+"\"\n\nfunc b() {}\n")
	v := findZoteroImportViolations(probe)
	if len(v) != 1 || !strings.Contains(v[0].path, "library") {
		t.Fatalf("a direct Zotero import in internal/library must be caught exactly once, got %v", v)
	}
	write("internal/sync/allowed.go", "package sync\n\nimport _ \""+zoteroAdapterImport+"\"\n\nfunc c() {}\n")
	if v := findZoteroImportViolations(probe); len(v) != 1 {
		t.Fatalf("the documented sync exception must stay green, got %v", v)
	}
}
