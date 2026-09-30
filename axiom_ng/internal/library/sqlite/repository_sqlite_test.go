// repository_sqlite_test.go — the Library repository contract suite
// against the SQLite engine (F12 #306): the same suite, the same
// fixtures as the PostgreSQL leg (pglib). No environment gate — SQLite
// is a local file, the suite always runs (that is the CI matrix's
// always-on leg).
package sqlite

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contractsuite"
	contracts "github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	lib "github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/reposuite"
)

func TestRepositoryContractSuiteSQLite(t *testing.T) {
	reposuite.Run(t, "sqlite", func(t *testing.T) (lib.Repository, func(), bool) {
		path := filepath.Join(t.TempDir(), "library.sqlite")
		r, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("sqlite open (fresh install): %v", err)
		}
		return r, func() { _ = r.Close() }, true
	})
}

// TestServiceRoundtripOverSQLite — the Library SERVICE over the SQLite
// engine end-to-end: the import saga runs to committed (validation →
// staging → saga → revision), proving the engine-neutral Repository
// carries the whole application, not just the row contract (the deep
// saga battery runs over pglib; this is the SQLite engine's service
// proof).
func TestServiceRoundtripOverSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite")
	r, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	prov := lib.NewFakeProvider()
	crossref := lib.NewFakeResolver("crossref", "fake-v1", lib.StandardCrossrefFixtures())
	openLib := lib.NewFakeResolver("open_library", "fake-v1", lib.StandardOpenLibraryFixtures())
	svc := lib.NewService(lib.Config{
		SourceID:  "src-sqlite-test",
		Provider:  "fake",
		LibraryID: "users/0",
	}, r, lib.NewStaging(t.TempDir()), lib.Ports{
		Catalog:     prov,
		Records:     prov,
		Renditions:  prov,
		Collections: prov,
		Resolvers:   []lib.BibliographicResolver{crossref, openLib},
		Documents:   lib.FakeDocumentInspector{},
	})
	ctx := context.Background()
	if ids, err := svc.ResumeInflight(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("resume on fresh file: %v %v", ids, err)
	}
	pdf := []byte("%PDF-1.4\nA SQLite Engine Roundtrip.\n%AXIOM-LANG: en\n\nBody.")
	crossrefOn, openLibOff := true, false
	req := contracts.ImportRequest{
		IdempotencyKey: "sqlite-roundtrip-1",
		RecordType:     "book",
		Target:         contracts.ImportTarget{LibraryID: "users/0"},
		MetadataHints:  contracts.MetadataHints{Title: contractsuite.SeedBibliography.Title},
		Enrichment:     contracts.EnrichmentFlags{Crossref: &crossrefOn, OpenLibrary: &openLibOff},
	}
	op, err := svc.StartImport(ctx, req, bytes.NewReader(pdf))
	if err != nil {
		t.Fatalf("start import: %v", err)
	}
	for i := 0; !terminalStatus(op.Status); i++ {
		if i > 50 {
			t.Fatalf("saga stuck at %s", op.Status)
		}
		got, err := svc.GetImport(ctx, contracts.ImportRef{ImportID: op.ImportID})
		if err != nil {
			t.Fatal(err)
		}
		op = got
	}
	if op.Status != contracts.ImportCommitted {
		t.Fatalf("import must commit over the SQLite engine, got %s (%+v)", op.Status, op)
	}
	// The revision landed (idempotent replay answers the same operation).
	replay, err := svc.StartImport(ctx, req, bytes.NewReader(pdf))
	if err != nil || replay.ImportID != op.ImportID {
		t.Fatalf("idempotent replay: %+v %v", replay, err)
	}
	if replay.Status != contracts.ImportCommitted {
		t.Fatalf("replay status: %s", replay.Status)
	}
}

// terminalStatus: the saga's resting states (a decision or a verdict).
func terminalStatus(s contracts.ImportStatus) bool {
	switch s {
	case contracts.ImportAwaitingConfirm, contracts.ImportCommitted,
		contracts.ImportRetryableFailed, contracts.ImportTerminalFailed:
		return true
	}
	return false
}
