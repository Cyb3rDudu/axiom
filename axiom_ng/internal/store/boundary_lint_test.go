// boundary_lint_test.go — the F09 #303 store-boundary gate: the Zotero/
// credential freedom of the Store packages, proven TRANSITIVELY in the
// dependency graph.
//
// Direct-import lints (the F03/F07 gates) catch a leak one hop away; this
// gate walks the whole module import graph and asserts that NO package of
// the Store component can REACH a banned package through any chain —
// store → repo → anything-Zotero is exactly the back door a per-file lint
// cannot see ("bis in den Dependency-Graph bewiesen").
//
// Store package set (the component's code): this package, dispatcher,
// processor, search, repo, events, sourceurl, db. Everything else in
// internal/ is either the Library (library, library/mirror, sync,
// zoteroprovider, repair, fixerinvoker — Zotero-side), the transport
// (server), the composition root, or scaffolding (cli, backfill,
// baseline, deprecate, …) — none of it may appear in a Store package's
// import closure. Allowed: the standard library/external modules and the
// internal/contracts tree (the transport-neutral component contracts).
//
// Teeth witnesses (TestBoundaryLintCatchesPlantedImports): probe copies of
// the module tree with a planted zoteroprovider import in repo (the
// direct sin), a config import in search (the credential carrier), a
// two-hop chain store→sync→zoteroprovider (the transitive sin this gate
// exists for), and a legal contracts import (must stay green) — each
// class must go red exactly where expected. Walk/parse errors are fatal
// (fail closed, never report a partial tree as clean).
package store

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// storePackages is the Store component's package set (module-relative
// paths). A package joins by OWNING Store truth (jobs, snapshots, chunks,
// embeddings, KG, outbox, search, retrieval wiring) — never by being
// useful to the Store.
var storePackages = []string{
	"internal/store",
	"internal/dispatcher",
	"internal/processor",
	"internal/search",
	"internal/repo",
	"internal/events",
	"internal/sourceurl",
	"internal/db",
}

// bannedPackages: no Store package may reach these through ANY import
// chain. zoteroprovider is the Zotero adapter; sync and library (+mirror)
// are the Library side; config is the credential carrier (Zotero base
// URL/key file wiring live there); repair/fixerinvoker are the F08
// Zotero-coupled write tracks; server/composition/cli/backfill/baseline
// are transport, wiring and scaffolding a component never depends on.
var bannedPackages = []string{
	"internal/zoteroprovider",
	"internal/sync",
	"internal/library",
	"internal/library/mirror",
	"internal/config",
	"internal/repair",
	"internal/fixerinvoker",
	"internal/server",
	"internal/composition",
	"internal/cli",
	"internal/backfill",
	"internal/baseline",
}

const modulePath = "github.com/Cyb3rDudu/axiom/axiom_ng"

// importGraph is the parsed module graph: package path → imported package
// paths (test files included — a test importing the adapter is a Zotero
// path, the F07 precedent).
type importGraph map[string]map[string]bool

// buildImportGraph parses every .go file under root (vendor/, testdata/,
// .git, dist skipped) and groups imports by package directory.
func buildImportGraph(root string) (importGraph, error) {
	g := importGraph{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", ".git", "dist", ".worktree":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
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
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		if g[pkg] == nil {
			g[pkg] = map[string]bool{}
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, modulePath+"/") {
				g[pkg][strings.TrimPrefix(p, modulePath+"/")] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// reachable returns every package reachable from start (start included).
func reachable(g importGraph, start string) map[string]bool {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		for imp := range g[p] {
			walk(imp)
		}
	}
	walk(start)
	return seen
}

// storeBoundaryViolations checks every Store package's transitive closure
// against the banned set; returns sorted violation strings.
func storeBoundaryViolations(g importGraph) []string {
	banned := map[string]bool{}
	for _, b := range bannedPackages {
		banned[b] = true
	}
	var out []string
	for _, sp := range storePackages {
		for p := range reachable(g, sp) {
			if bannedPkg(banned, p) {
				out = append(out, fmt.Sprintf("%s reaches %s", sp, p))
			}
		}
	}
	sort.Strings(out)
	return out
}

// bannedPkg: exact match or any subpackage of a banned root — a NEW
// package under internal/library or internal/sync is Library-side by
// construction and must not sneak past an exact-name list (review F2.7).
func bannedPkg(banned map[string]bool, p string) bool {
	if banned[p] {
		return true
	}
	for b := range banned {
		if strings.HasPrefix(p, b+"/") {
			return true
		}
	}
	return false
}

// TestStorePackagesNeverReachZoteroOrCredentials — the standing gate.
// A violation means the Store boundary broke: route the dependency through
// the contracts (a port, a DTO) or through the composition root instead.
func TestStorePackagesNeverReachZoteroOrCredentials(t *testing.T) {
	g, err := buildImportGraph("../..")
	if err != nil {
		t.Fatalf("store boundary lint scan failed (fail closed): %v", err)
	}
	if v := storeBoundaryViolations(g); len(v) > 0 {
		t.Fatalf("Store packages reach banned packages (F09 #303 boundary; route through contracts or the composition root):\n\t%s",
			strings.Join(v, "\n\t"))
	}
}

// TestBoundaryLintCatchesPlantedImports — the teeth witnesses, on probe
// copies of the module graph so the real tree stays untouched.
func TestBoundaryLintCatchesPlantedImports(t *testing.T) {
	probe := func(mutate func(root string)) importGraph {
		root := t.TempDir()
		write := func(rel string) {
			dst := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("package p\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range append(append([]string{}, storePackages...), bannedPackages...) {
			write(p + "/x.go")
		}
		mutate(root)
		g, err := buildImportGraph(root)
		if err != nil {
			t.Fatalf("probe graph: %v", err)
		}
		return g
	}
	plant := func(root, rel, imp string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte("package p\n\nimport _ \""+modulePath+"/"+imp+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Clean probe: zero violations.
	if v := storeBoundaryViolations(probe(func(string) {})); len(v) != 0 {
		t.Fatalf("clean probe must yield zero violations, got %v", v)
	}

	// Sonde 1: the direct sin — repo imports the Zotero adapter.
	v := storeBoundaryViolations(probe(func(root string) { plant(root, "internal/repo/planted.go", "internal/zoteroprovider") }))
	if len(v) == 0 || !containsStr(v, "internal/repo reaches internal/zoteroprovider") {
		t.Fatalf("a direct zoteroprovider import in repo must be caught, got %v", v)
	}

	// Sonde 2: the credential carrier — search imports config.
	v = storeBoundaryViolations(probe(func(root string) { plant(root, "internal/search/planted.go", "internal/config") }))
	if len(v) == 0 || !containsStr(v, "internal/search reaches internal/config") {
		t.Fatalf("a config import in search must be caught, got %v", v)
	}

	// Sonde 3: the TRANSITIVE sin — this gate's reason to exist: store
	// imports sync (not itself banned-by-lint semantics), sync imports the
	// adapter; the closure must flag store (and sync's other reachers).
	v = storeBoundaryViolations(probe(func(root string) {
		plant(root, "internal/sync/adapter.go", "internal/zoteroprovider")
		plant(root, "internal/store/bridge.go", "internal/sync")
	}))
	if !containsStr(v, "internal/store reaches internal/zoteroprovider") {
		t.Fatalf("the two-hop chain store→sync→zoteroprovider must be caught transitively, got %v", v)
	}

	// Sonde 4: contracts stay legal (the seam packages are the sanctioned
	// Library surface for the Store).
	v = storeBoundaryViolations(probe(func(root string) {
		write := func(rel string) {
			p := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("internal/contracts/revision/doc.go")
		plant(root, "internal/store/legal.go", "internal/contracts/revision")
	}))
	if len(v) != 0 {
		t.Fatalf("a contracts import from a Store package is legal, got %v", v)
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
