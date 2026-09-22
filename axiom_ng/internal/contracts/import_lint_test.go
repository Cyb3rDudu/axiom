// import_lint_test.go — the F03 transport-neutrality gate (#297).
//
// Contract packages may import ONLY the standard library — net/http
// excluded, the one stdlib tree that IS transport state — and their
// sibling packages under internal/contracts. That rule bans, by
// construction, everything the seam must never see: internal/db and
// internal/repo (SQL/pgx/repo models), net/http and chi (transport),
// internal/zotero (provider), pgx itself. A violation here means a DB
// or transport model leaked into the compile anchor F06/F09/F11 build
// against — the persistence split (F12/DM) would be undone through the
// back door.
//
// Red-proofs: the probe commit on a worktree copy (an internal/db import
// injected into a contract file) turns this test red; the same dance
// with a net/http import after the explicit stdlib denylist was added.
// See the issue comment for the transcripts.
package contracts

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// contractsImportPrefix is the import-path prefix of every contract
// package (this package's tree) — the allowlist anchor. The lint covers
// all subpackages, production and test files alike: test files are
// examples, and leaky examples breed leaky code.
const contractsImportPrefix = "github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts"

// stdlibDenied is the explicit standard-library denylist: paths that
// are dot-free stdlib by shape but carry transport semantics across
// the seam. Covers net/http and its subpackages (httptrace, httputil,
// httptest…).
const stdlibDenied = "net/http"

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
		t.Fatalf("contract packages must stay transport-neutral (stdlib except net/http + sibling contract packages only; no internal/db, no internal/repo, no net/http, no chi, no zotero, no pgx):\n\t%s",
			strings.Join(violations, "\n\t"))
	}
}

// allowedImport: sibling contract packages and the standard library
// (identified by a dot-free first path segment), EXCEPT the denied
// stdlib transport tree.
func allowedImport(p string) bool {
	if p == contractsImportPrefix || strings.HasPrefix(p, contractsImportPrefix+"/") {
		return true
	}
	first := p
	if i := strings.Index(first, "/"); i > 0 {
		first = first[:i]
	}
	if strings.Contains(first, ".") {
		return false // external module
	}
	if p == stdlibDenied || strings.HasPrefix(p, stdlibDenied+"/") {
		return false // stdlib by shape, transport by nature
	}
	return true
}
