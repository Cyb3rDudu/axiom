// import_lint_test.go — the F03 transport-neutrality gate (#297).
//
// Contract packages may import ONLY the standard library and their
// sibling packages under internal/contracts. That rule bans, by
// construction, everything the seam must never see: internal/db and
// internal/repo (SQL/pgx/repo models), net/http and chi (transport),
// internal/zotero (provider), pgx itself. A violation here means a DB
// or transport model leaked into the compile anchor F06/F09/F11 build
// against — the persistence split (F12/DM) would be undone through the
// back door.
//
// Red-proof: the probe commit on a worktree copy (an internal/db import
// injected into a contract file) turns this test red; see the issue
// comment for the transcript.
package contracts

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// contractsRoot is this package's directory — the root of everything
// the lint covers (all subpackages, production and test files alike:
// test files are examples, and leaky examples breed leaky code).
const contractsImportPrefix = "github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts"

func TestContractPackagesAreTransportNeutral(t *testing.T) {
	var violations []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if allowedImport(p) {
				continue
			}
			violations = append(violations, path+": "+p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("contract packages must stay transport-neutral (stdlib + sibling contract packages only; no internal/db, no internal/repo, no net/http, no chi, no zotero, no pgx):\n\t%s",
			strings.Join(violations, "\n\t"))
	}
}

// allowedImport: sibling contract packages and the standard library
// (identified by a dot-free first path segment).
func allowedImport(p string) bool {
	if p == contractsImportPrefix || strings.HasPrefix(p, contractsImportPrefix+"/") {
		return true
	}
	first := p
	if i := strings.Index(first, "/"); i > 0 {
		first = first[:i]
	}
	return !strings.Contains(first, ".")
}
