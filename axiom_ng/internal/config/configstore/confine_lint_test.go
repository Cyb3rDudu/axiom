// confine_lint_test.go — the F13 #307 configstore-confinement gate:
// config.sqlite is read and written ONLY through internal/config (the
// resolution chain — the sanctioned config reader) and internal/cli
// (the operator surface: config set/unset/import-env). No domain layer
// may open the file directly — the composition root included (it
// receives the resolved Config; the chain did the reading, mirroring
// the F12 rule that no domain path opens a database file itself).
//
// Rule: a direct import of internal/config/configstore is banned in
// every production package except the two sanctioned ones. Tests may
// import the store (a test proving the store IS the file's only home
// is not a leak) — the scan covers production files only.
//
// Teeth (TestConfigstoreConfinementCatchesPlantedImports): probe trees
// with a planted import in the composition root and in a Library
// package are refused; the sanctioned shapes stay green.
package configstore

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/Cyb3rDudu/axiom/axiom_ng"

// configstoreImporters: the ONLY production packages entitled to the
// config store. A new consumer joins this list deliberately (and needs
// a reason that is not "a domain layer wants to read config.sqlite").
var configstoreImporters = map[string]bool{
	"internal/config": true, // the resolution chain (LoadResolved)
	"internal/cli":    true, // config set/unset/import-env/validate
}

// scanImports parses every production .go file under root and returns,
// per package, its direct import paths. Dot-directories are skipped
// wholesale: they are hidden/transient state (another package's probe
// trees — composition's blank-import witness creates .blankprobe-N
// under the module root WHILE tests run in parallel — never real
// packages; scanning them races the runner for no signal).
func scanImports(root string) (map[string][]string, error) {
	out := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			switch d.Name() {
			case "testdata", "vendor", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		pkg := filepath.ToSlash(filepath.Dir(rel))
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == modulePath+"/internal/config/configstore" {
				out[pkg] = append(out[pkg], "configstore")
			}
		}
		return nil
	})
	return out, err
}

// confinementViolations returns the packages importing configstore
// outside the sanctioned set.
func confinementViolations(perPkg map[string][]string) []string {
	var out []string
	for pkg := range perPkg {
		if !configstoreImporters[pkg] && pkg != "internal/config/configstore" {
			out = append(out, pkg)
		}
	}
	// deterministic order
	sort.Strings(out)
	return out
}

// TestConfigstoreConfinedToConfigAndCli — the standing gate.
func TestConfigstoreConfinedToConfigAndCli(t *testing.T) {
	perPkg, err := scanImports("../../..") // the module root (this package sits three levels deep)
	if err != nil {
		t.Fatalf("configstore lint scan failed (fail closed): %v", err)
	}
	if v := confinementViolations(perPkg); len(v) > 0 {
		t.Fatalf("configstore imported outside the sanctioned set (internal/config, internal/cli — F13 #307):\n\t%s",
			strings.Join(v, "\n\t"))
	}
}

// TestConfigstoreConfinementCatchesPlantedImports — the teeth witnesses
// on probe copies (the real tree stays untouched).
func TestConfigstoreConfinementCatchesPlantedImports(t *testing.T) {
	probe := func(relPkg string) []string {
		root := t.TempDir()
		dir := filepath.Join(root, filepath.FromSlash(relPkg))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "planted.go"),
			[]byte("package planted\n\nimport _ \""+modulePath+"/internal/config/configstore\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		perPkg, err := scanImports(root)
		if err != nil {
			t.Fatalf("probe scan: %v", err)
		}
		return confinementViolations(perPkg)
	}

	// Sonde 1: the composition root — it must sit on the RESOLVED
	// config, never the file.
	if v := probe("internal/composition"); len(v) == 0 || v[0] != "internal/composition" {
		t.Fatalf("composition importing configstore must be caught, got %v", v)
	}
	// Sonde 2: a Library package (domain layer).
	if v := probe("internal/library/repair"); len(v) == 0 || v[0] != "internal/library/repair" {
		t.Fatalf("a Library package importing configstore must be caught, got %v", v)
	}
	// Legal shape 1: the config chain.
	if v := probe("internal/config"); len(v) != 0 {
		t.Fatalf("internal/config importing configstore is legal, got %v", v)
	}
	// Legal shape 2: the CLI operator surface.
	if v := probe("internal/cli"); len(v) != 0 {
		t.Fatalf("internal/cli importing configstore is legal, got %v", v)
	}
	// Legal shape 3: a HIDDEN directory (the transient .blankprobe-N trees
	// another package's tests create under the module root mid-run) is
	// skipped wholesale — the standing gate must not race parallel tests.
	{
		root := t.TempDir()
		dir := filepath.Join(root, ".blankprobe-race")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "planted.go"),
			[]byte("package main\n\nimport _ \""+modulePath+"/internal/config/configstore\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		perPkg, err := scanImports(root)
		if err != nil {
			t.Fatal(err)
		}
		if v := confinementViolations(perPkg); len(v) != 0 {
			t.Fatalf("hidden dot-directory probe trees must be skipped, got %v", v)
		}
	}
}
