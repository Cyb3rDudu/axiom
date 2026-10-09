// engine_lint_test.go — the F12 #306 engine-leak gate: pgvector/pgx
// types and OpenSearch-client construction live ONLY in repository
// implementations; the contract and application layers stay engine-free.
//
// Two rules, both over PRODUCTION files (test files may exercise any
// engine — a test proving the repository IS the engine's home is not a
// leak):
//
//  1. DIRECT imports of github.com/jackc/pgx/v5 (incl. pgtype/pgxpool)
//     and github.com/pgvector/* are banned everywhere except the
//     repository-implementation set — the Store engines (repo, search,
//     db, store/migrations) and the Library's PostgreSQL engine plus its
//     documented legacy lanes (pglib, mirror, repair). The contracts
//     tree, both components' application packages (store, dispatcher,
//     processor, events, sourceurl, library, sqlite, reposuite,
//     zoteroprovider, sync) and the bindings must stay engine-neutral —
//     the neutrality the SQLite engines and the follow-up SQLite-Store
//     epic build on (non-blockade, made enforceable).
//
//  2. search.New (the ONLY constructor of the OpenSearch client) may be
//     called only from the search package itself, the composition root,
//     and the repo's tool mains under cmd/ (bench and scaffolding
//     binaries legitimately build the retrieval stack) — an OpenSearch
//     client anywhere else (a Library or transport package) is a leak by
//     construction. This rule binds EVERY production package, not just
//     the clean set.
//
// Documented residual limits (accepted): (a) dot-imports
// (`import . "…/internal/search"` + a bare New() call) escape the
// alias-aware ctor scanner — dot-imports are banned style here (unenforced);
// (b) the import ban covers the PostgreSQL/pgvector engine modules only
// — a modernc.org/sqlite import in an application layer is NOT flagged
// (PG asymmetry; revisit with the SQLite-Store follow-up epic).
//
// Teeth (TestEngineLintCatchesPlantedLeaks): probe copies with a planted
// pgtype import in the Store application layer (the exact sin this gate
// exists for — it was real: normalizeSourceID used pgtype.UUID until
// F12), a planted pgx import in the Library application package, a
// planted search.New call in a Library package, and the two legal shapes
// (repo's pgx, composition's search.New) that must stay green.
//
// Out of scope (deliberate, documented): internal/server, cli,
// baseline, composition are transport and scaffolding — they
// WIRE engines and pools by trade; their engine freedom is F14's
// topology work, not this gate.
package store

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Checked trees and packages — PREFIX polarity (F12 review): every
// subpackage under the component trees is IN by construction, so the
// NEXT package addition fails loudly instead of sailing through an
// exact-name list. engineAllowlist names the repository implementations
// (and documented legacy lanes) entitled to the engine dialect.
var (
	// engineCleanTrees: subpackages of these are engine-clean by prefix.
	engineCleanTrees = []string{
		"internal/contracts",
		"internal/store",
		"internal/library",
	}
	// engineCleanPackages: additional exact application packages (not
	// under the trees above).
	engineCleanPackages = []string{
		"internal/dispatcher",
		"internal/processor",
		"internal/events",
		"internal/sourceurl",
		"internal/bindings",
		"internal/zoteroprovider",
		"internal/sync",
	}
	// engineAllowlist: repository implementations + documented legacy
	// lanes — the ONLY packages exempt from the engine-import check. A
	// NEW engine lane joins THIS list deliberately (a comment here beats
	// a silent pass).
	engineAllowlist = []string{
		"internal/library/pglib",    // the Library PostgreSQL repository
		"internal/library/mirror",   // legacy Zotero-mirror lane (shared DB)
		"internal/library/repair",   // legacy repair lane (shared DB)
		"internal/repo",             // the Store repository
		"internal/search",           // retrieval stack (OS client home)
		"internal/db",               // core schema runner
		"internal/store/migrations", // the Store ledger runner
	}
)

// engineBannedImports: the engine-type modules confined to repository
// implementations (prefix match — pgx/v5, pgx/v5/pgtype, pgx/v5/pgxpool
// all match the first entry).
var engineBannedImports = []string{
	"github.com/jackc/pgx",
	"github.com/pgvector",
}

// scanDirectImports parses every .go file under root (testdata skipped)
// and returns, per file, its direct import paths.
func scanDirectImports(root string) (map[string][]string, error) {
	out := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", ".git", "vendor", "dist":
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
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		key := filepath.ToSlash(rel)
		if _, ok := out[key]; !ok {
			out[key] = nil // import-less files still count (ctor checks)
		}
		for _, imp := range f.Imports {
			out[key] = append(out[key], strings.Trim(imp.Path.Value, `"`))
		}
		return nil
	})
	return out, err
}

// engineLeakViolations checks the production files against the two
// rules: banned engine imports inside the clean set, and the search.New
// confinement everywhere; returns sorted violation strings.
func engineLeakViolations(files map[string][]string, osConstructors map[string]string) []string {
	clean := map[string]bool{}
	for _, p := range engineCleanPackages {
		clean[p] = true
	}
	allowed := map[string]bool{}
	for _, p := range engineAllowlist {
		allowed[p] = true
	}
	// PREFIX polarity (F12 review): subpackages of the component trees
	// are checked BY CONSTRUCTION — the next package addition fails
	// loudly instead of sailing through an exact-name list. Engine homes
	// (the allowlist) are exempt from the import check.
	isChecked := func(pkg string) bool {
		if allowed[pkg] {
			return false // engine home — entitled to the dialect
		}
		if clean[pkg] {
			return true
		}
		for _, tree := range engineCleanTrees {
			if pkg == tree || strings.HasPrefix(pkg, tree+"/") {
				return true // the tree root and every subpackage
			}
		}
		return false
	}
	// The OS-client rule binds EVERY package (see rule 2 above): the
	// allowed sites are the search package itself, the composition root,
	// and the repo's tool mains under cmd/.
	osAllowed := func(pkg string) bool {
		return pkg == "internal/search" || pkg == "internal/composition" || strings.HasPrefix(pkg, "cmd/")
	}
	var out []string
	for file, imports := range files {
		pkg := filepath.ToSlash(filepath.Dir(file))
		if isChecked(pkg) {
			for _, imp := range imports {
				for _, banned := range engineBannedImports {
					if strings.HasPrefix(imp, banned) {
						out = append(out, pkg+" imports "+imp+" (engine type outside repository implementations)")
					}
				}
			}
		}
		if !osAllowed(pkg) {
			if ctor, ok := osConstructors[file]; ok {
				out = append(out, pkg+" constructs the OpenSearch client ("+ctor+") outside the sanctioned wiring sites (search, composition, cmd)")
			}
		}
	}
	// sort + dedupe
	seen := map[string]bool{}
	var sorted []string
	for _, v := range out {
		if !seen[v] {
			seen[v] = true
			sorted = append(sorted, v)
		}
	}
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted
}

// scanOSConstructors finds search.New( call sites per file (production
// files only). Alias-aware: a file importing internal/search as `srch`
// is caught via `srch.New(` too — the import's LOCAL name resolves from
// the parsed import block, so the common alias-escape is covered (a
// plain textscan for "search.New(" alone would miss it).
func scanOSConstructors(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", ".git", "vendor", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		local := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == modulePath+"/internal/search" {
				local = "search"
				if imp.Name != nil {
					local = imp.Name.Name
				}
				break
			}
		}
		if local == "" {
			return nil // does not import the search package
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), local+".New(") {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			out[filepath.ToSlash(rel)] = local + ".New"
		}
		return nil
	})
	return out, err
}

// TestEngineTypesConfinedToRepositoryImplementations — the standing
// gate. A violation means an engine leaked into a contract or
// application layer: route it through the neutral repository seams.
func TestEngineTypesConfinedToRepositoryImplementations(t *testing.T) {
	files, err := scanDirectImports("../..")
	if err != nil {
		t.Fatalf("engine lint scan failed (fail closed): %v", err)
	}
	ctors, err := scanOSConstructors("../..")
	if err != nil {
		t.Fatalf("os-client scan failed (fail closed): %v", err)
	}
	if v := engineLeakViolations(files, ctors); len(v) > 0 {
		t.Fatalf("engine types / OS clients leaked outside repository implementations (F12 #306):\n\t%s",
			strings.Join(v, "\n\t"))
	}
}

// TestEngineLintCatchesPlantedLeaks — the teeth witnesses on probe
// copies (the real tree stays untouched).
func TestEngineLintCatchesPlantedLeaks(t *testing.T) {
	probe := func(plant func(root string)) (files map[string][]string, ctors map[string]string) {
		root := t.TempDir()
		for _, p := range []string{
			"internal/store", "internal/library", "internal/library/pglib", "internal/repo",
			"internal/search", "internal/composition",
		} {
			dir := filepath.Join(root, filepath.FromSlash(p))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		plant(root)
		f, err := scanDirectImports(root)
		if err != nil {
			t.Fatalf("probe scan: %v", err)
		}
		c, err := scanOSConstructors(root)
		if err != nil {
			t.Fatalf("probe ctor scan: %v", err)
		}
		return f, c
	}
	write := func(root, rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Clean probe: zero violations.
	files, ctors := probe(func(string) {})
	if v := engineLeakViolations(files, ctors); len(v) != 0 {
		t.Fatalf("clean probe must be green, got %v", v)
	}

	// Sonde 1 (the reformed real sin): pgtype in the Store application
	// layer.
	files, ctors = probe(func(root string) {
		write(root, "internal/store/planted.go", "package store\n\nimport \"github.com/jackc/pgx/v5/pgtype\"\n\nvar _ = pgtype.UUID{}\n")
	})
	if v := engineLeakViolations(files, ctors); len(v) == 0 || !containsStr(v, "internal/store imports github.com/jackc/pgx/v5/pgtype (engine type outside repository implementations)") {
		t.Fatalf("a pgtype import in the Store application layer must be caught, got %v", v)
	}

	// Sonde 2: pgx in the Library application package.
	files, ctors = probe(func(root string) {
		write(root, "internal/library/planted.go", "package library\n\nimport \"github.com/jackc/pgx/v5\"\n\nvar _ = pgx.ErrNoRows\n")
	})
	if v := engineLeakViolations(files, ctors); len(v) == 0 || !containsStr(v, "internal/library imports github.com/jackc/pgx/v5 (engine type outside repository implementations)") {
		t.Fatalf("a pgx import in the Library application layer must be caught, got %v", v)
	}

	// Sonde 3: OpenSearch-client construction in a Library package — a
	// REAL importing file (alias `srch`), so the REAL alias-aware scanner
	// must find the call site (a comment-only plant no longer matches:
	// the scanner requires the import).
	files, ctors = probe(func(root string) {
		write(root, "internal/library/wire.go", "package library\n\nimport srch \""+modulePath+"/internal/search\"\n\nvar _ = srch.New\n// srch.New(\n")
	})
	if _, ok := ctors["internal/library/wire.go"]; !ok {
		t.Fatalf("the alias-aware ctor scanner missed an importing call site: %v", ctors)
	}
	if v := engineLeakViolations(files, ctors); len(v) == 0 || !containsStr(v, "internal/library constructs the OpenSearch client (srch.New) outside the sanctioned wiring sites (search, composition, cmd)") {
		t.Fatalf("OS-client construction in a Library package must be caught, got %v", v)
	}

	// Sonde 3b (polarity): a NEW subpackage under a component tree is
	// checked by prefix — the exact-name-list hole is closed.
	files, ctors = probe(func(root string) {
		write(root, "internal/store/newlane/planted.go", "package newlane\n\nimport \"github.com/jackc/pgx/v5\"\n\nvar _ = pgx.ErrNoRows\n")
	})
	if v := engineLeakViolations(files, ctors); len(v) == 0 || !containsStr(v, "internal/store/newlane imports github.com/jackc/pgx/v5 (engine type outside repository implementations)") {
		t.Fatalf("a new subpackage under a component tree must be checked by prefix, got %v", v)
	}

	// Legal shape 1: the repository implementation's pgx stays green.
	files, ctors = probe(func(root string) {
		write(root, "internal/repo/store.go", "package repo\n\nimport \"github.com/jackc/pgx/v5/pgxpool\"\n\nvar _ = pgxpool.Pool{}\n")
	})
	if v := engineLeakViolations(files, ctors); len(v) != 0 {
		t.Fatalf("repo's pgx is legal, got %v", v)
	}

	// Legal shape 2: the composition root constructs search.New.
	files, ctors = probe(func(root string) {
		write(root, "internal/composition/wire.go", "package composition\n\n// search.New( here\n")
	})
	if v := engineLeakViolations(files, ctors); len(v) != 0 {
		t.Fatalf("composition's search.New is legal, got %v", v)
	}
}
