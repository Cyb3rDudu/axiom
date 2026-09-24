// library_it_test.go — the F06 DoD battery against real Postgres
// (AXIOM_TEST_DATABASE_URL, session-unique scratch DB, library migrations
// only). Covers: the F03 contract suite over the real service, the
// kill/resume table, the dedup scenarios with provider count asserts, the
// ladder fixtures, and the provenance sonde.
package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contractsuite"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// scratch DB harness (cli-doctor convention: own throwaway DB, refuses
// non-_test base DSNs, dropped in cleanup)

func testStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping library IT")
	}
	base := dbOf(dsn)
	if !strings.HasSuffix(base, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", base)
	}
	dbName := strings.TrimSuffix(base, "_test") + fmt.Sprintf("_lib%d_test", os.Getpid())
	// DDL and database identifiers cannot take bind parameters — the
	// scratch name is ALLOWLISTED ([A-Za-z0-9_] only, nothing else may
	// appear) before any interpolation below.
	for _, r := range dbName {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			t.Fatalf("scratch db name %q is not a safe identifier", dbName)
		}
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, dbName)); err != nil {
		t.Fatalf("kill connections: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName)); err != nil {
		t.Fatalf("drop old scratch: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	admin.Close()

	pool, err := pgxpool.New(ctx, withDB(dsn, dbName))
	if err != nil {
		t.Fatalf("open scratch pool: %v", err)
	}
	st := NewStore(pool)
	if err := st.Migrate(ctx); err != nil {
		pool.Close()
		t.Fatalf("library migrate: %v", err)
	}
	return st, func() {
		ctx2 := context.Background()
		if _, err := pool.Exec(ctx2, fmt.Sprintf(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, dbName)); err == nil {
			// best-effort: closing the pool below frees our own connection
		}
		pool.Close()
		if a, err := pgxpool.New(ctx2, dsn); err == nil {
			_, _ = a.Exec(ctx2, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
			a.Close()
		}
	}
}

func dbOf(dsn string) string {
	name := dsn
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, "?"); i >= 0 {
		name = name[:i]
	}
	return name
}

func withDB(dsn, newDB string) string {
	i := strings.LastIndex(dsn, "/")
	head, tail := dsn[:i+1], dsn[i+1:]
	if j := strings.Index(tail, "?"); j >= 0 {
		return head + newDB + "?" + tail[j+1:]
	}
	return head + newDB
}

// ---------------------------------------------------------------------------
// fixture builders

func newFakeService(t *testing.T, st *Store) (*Service, *FakeProvider, *FakeResolver, *FakeResolver) {
	t.Helper()
	prov := NewFakeProvider()
	return newServiceOver(t, st, prov, t.TempDir())
}

// newServiceOver builds the service over GIVEN provider + staging root —
// the "process restart" shape: provider and staging are EXTERNAL systems
// that survive the crash; only the service instance is fresh.
func newServiceOver(t *testing.T, st *Store, prov *FakeProvider, stagingRoot string) (*Service, *FakeProvider, *FakeResolver, *FakeResolver) {
	t.Helper()
	crossref := NewFakeResolver("crossref", "fake-v1", StandardCrossrefFixtures())
	openLib := NewFakeResolver("open_library", "fake-v1", StandardOpenLibraryFixtures())
	svc := NewService(Config{
		SourceID:  "src-library-test",
		Provider:  "fake",
		LibraryID: "users/0",
	}, st, NewStaging(stagingRoot), Ports{
		Catalog:     prov,
		Records:     prov,
		Renditions:  prov,
		Collections: prov,
		Resolvers:   []BibliographicResolver{crossref, openLib},
		Documents:   FakeDocumentInspector{},
	})
	return svc, prov, crossref, openLib
}

func seedReq(key string) library.ImportRequest {
	crossref, openLib := true, false
	return library.ImportRequest{
		IdempotencyKey: key,
		RecordType:     "book",
		Target:         library.ImportTarget{LibraryID: "users/0"},
		MetadataHints:  library.MetadataHints{Title: contractsuite.SeedBibliography.Title},
		Enrichment:     library.EnrichmentFlags{Crossref: &crossref, OpenLibrary: &openLib},
	}
}

// ladderPDF builds deterministic document bytes for the ladder fixtures:
// the first sentence is the document title; %AXIOM-LANG declares language.
func ladderPDF(docTitle string) []byte {
	return []byte("%PDF-1.4\n" + docTitle + ".\n%AXIOM-LANG: en\n\nBody of the fixture.")
}

// ---------------------------------------------------------------------------
// 1. The F03 contract suite runs green against the real service.

func TestContractSuiteAgainstService(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)
	contractsuite.LibrarySuite(t, svc)
}

// ---------------------------------------------------------------------------
// 2. Ladder fixtures (DoD).

func TestLadderUnambiguousDOIBeatsFuzzy(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, crossref, _ := newFakeService(t, st)

	content := ladderPDF("Document Title From Content")
	req := seedReq("ladder-doi")
	req.MetadataHints.DOI = StandardLadderFixtures.UniqueDOI
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportCommitted {
		t.Fatalf("status = %s (failure %+v, decisions %+v)", op.Status, op.Failure, op.Decisions)
	}
	// The DOI rung resolved directly: the fuzzy pass never ran for crossref.
	for _, c := range crossref.Calls() {
		if c.DOI == "" && c.Title != "" {
			t.Fatalf("fuzzy search ran although the identifier hit was direct: %+v", c)
		}
	}
	// Provenance: title applied from document (verified), crossref's
	// differing title documented as rejected.
	prov := provenanceOf(t, st, op.ImportID)
	if !provHas(prov, "title", "document", true) {
		t.Fatalf("document title not applied: %+v", prov)
	}
	if !provHas(prov, "doi", "identifier", true) {
		t.Fatalf("DOI not applied from the identifier rung: %+v", prov)
	}
	if !provHas(prov, "title", "identifier", false) {
		t.Fatalf("weaker title contribution not documented as rejected: %+v", prov)
	}
}

func TestLadderAmbiguousCrossrefAwaitsConfirmation(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)

	req := seedReq("ladder-amb")
	req.MetadataHints.Title = StandardLadderFixtures.AmbiguousTitle
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(ladderPDF("Network Effects intro text")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportAwaitingConfirm {
		t.Fatalf("status = %s, want awaiting_confirmation", op.Status)
	}
	if len(op.Decisions) != 1 || len(op.Decisions[0].Candidates) != 2 {
		t.Fatalf("want ONE decision with BOTH candidates, got %+v", op.Decisions)
	}
	// Confirm the FIRST candidate → the chosen one commits.
	confirmed, err := svc.ConfirmImport(context.Background(), op.ImportID, op.Decisions[0].DecisionID, op.Decisions[0].Candidates[0].CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != library.ImportCommitted {
		t.Fatalf("confirm did not commit the CHOSEN candidate: %s %+v", confirmed.Status, confirmed.Failure)
	}
	if confirmed.Result == nil || confirmed.Result.Revision.Bibliography.Title != "Network Effects in Platforms" {
		t.Fatalf("committed bibliography is not the chosen candidate: %+v", confirmed.Result)
	}
}

func TestLadderNoHitInventsNothing(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)

	docTitle := "Totally Unknown Work intro"
	op, err := svc.StartImport(context.Background(), seedReq("ladder-none"), bytes.NewReader(ladderPDF(docTitle)))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportCommitted {
		t.Fatalf("status = %s (%+v)", op.Status, op.Failure)
	}
	// Empty fields stay empty; the title carries DOCUMENT provenance only.
	prov := provenanceOf(t, st, op.ImportID)
	if !provHas(prov, "title", "document", true) {
		t.Fatalf("document title missing: %+v", prov)
	}
	for _, p := range prov {
		if p.Field == "publisher" && p.Applied {
			t.Fatalf("publisher invented from nowhere: %+v", p)
		}
	}
}

func TestLadderTypeConflictAwaitsConfirmation(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)

	req := seedReq("ladder-type")
	req.RecordType = "report"
	req.MetadataHints.Title = StandardLadderFixtures.TypeConflictTitle
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(ladderPDF("Typed Work intro")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportAwaitingConfirm {
		t.Fatalf("type conflict auto-took the candidate: %s", op.Status)
	}
}

// Ambiguous IDENTIFIER lookup (two DOI hits): decision via the
// identifier rung — the fuzzy pass never runs (DOI vor unscharfer
// Suche, and ambiguity is decided at the rung that found it).
func TestLadderAmbiguousDOIAwaitsConfirmationViaIdentifierRung(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, crossref, _ := newFakeService(t, st)

	req := seedReq("ladder-doi-amb")
	req.MetadataHints.DOI = StandardLadderFixtures.AmbiguousDOI
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(ladderPDF("Ambiguous DOI document text")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportAwaitingConfirm {
		t.Fatalf("ambiguous identifier hit auto-took a candidate: %s", op.Status)
	}
	if len(op.Decisions) != 1 || len(op.Decisions[0].Candidates) != 2 {
		t.Fatalf("want the identifier-rung decision with BOTH candidates, got %+v", op.Decisions)
	}
	for _, c := range crossref.Calls() {
		if c.DOI == "" {
			t.Fatalf("fuzzy search ran although the identifier lookup was ambiguous: %+v", c)
		}
	}
	// Confirming the SECOND candidate commits ITS bibliography, with
	// user provenance for the chosen fields (rung on ResolverVersion).
	confirmed, err := svc.ConfirmImport(context.Background(), op.ImportID, op.Decisions[0].DecisionID, op.Decisions[0].Candidates[1].CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != library.ImportCommitted {
		t.Fatalf("confirm did not commit the chosen candidate: %s %+v", confirmed.Status, confirmed.Failure)
	}
	if confirmed.Result.Revision.Bibliography.Title != "Ambiguous DOI Work B" {
		t.Fatalf("committed bibliography is not the chosen candidate: %+v", confirmed.Result)
	}
	userProv := false
	for _, p := range provenanceOf(t, st, confirmed.ImportID) {
		if p.Field == "title" && p.Source == "user" && p.ResolverVersion == "identifier" && p.Applied {
			userProv = true
		}
	}
	if !userProv {
		t.Fatalf("confirmed fields must carry source=user with the rung on ResolverVersion")
	}
}

// Provenance sonde (DoD): a weaker hit after a verified document field
// leaves the field unchanged, and the attempt is documented.
func TestProvenanceSondeWeakerHitNeverOverwrites(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)

	// Document says "Document Title From Content"; DOI hit proposes a
	// DIFFERENT title — rejected, documented.
	op, err := svc.StartImport(context.Background(), withDOI(seedReq("prov-sonde"), StandardLadderFixtures.UniqueDOI), bytes.NewReader(ladderPDF("Document Title From Content")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportCommitted {
		t.Fatalf("status = %s (%+v)", op.Status, op.Failure)
	}
	if op.Result.Revision.Bibliography.Title != "Document Title From Content" {
		t.Fatalf("verified document title was overwritten: %q", op.Result.Revision.Bibliography.Title)
	}
	prov := provenanceOf(t, st, op.ImportID)
	if !provHas(prov, "title", "identifier", false) {
		t.Fatalf("the rejected overwrite attempt is not documented: %+v", prov)
	}
}

func withDOI(r library.ImportRequest, doi string) library.ImportRequest {
	r.MetadataHints.DOI = doi
	return r
}

// ---------------------------------------------------------------------------
// 3. Duplicate detection scenarios with count asserts (DoD).

func TestDedupSameRecordSamePDFMembershipOnly(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, prov, _, _ := newFakeService(t, st)

	content := ladderPDF("Dedup Fixture Title")
	target := library.ImportTarget{LibraryID: "users/0", CollectionPath: []string{"Semester 3"}, CreateMissing: true}
	first := seedReq("dedup-1")
	first.Target = target
	op1, err := svc.StartImport(context.Background(), first, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if op1.Status != library.ImportCommitted {
		t.Fatalf("first import: %s (%+v)", op1.Status, op1.Failure)
	}

	// Same content, same metadata (different key), SECOND collection.
	second := seedReq("dedup-2")
	second.Target = library.ImportTarget{LibraryID: "users/0", CollectionPath: []string{"Semester 4"}, CreateMissing: true}
	op2, err := svc.StartImport(context.Background(), second, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if op2.Status != library.ImportCommitted {
		t.Fatalf("second import: %s (%+v)", op2.Status, op2.Failure)
	}
	// Same record, NO second rendition, membership added.
	if op2.Result.RecordID != op1.Result.RecordID {
		t.Fatalf("membership-only scenario created a second record: %s vs %s", op2.Result.RecordID, op1.Result.RecordID)
	}
	if op2.Result.RenditionID != op1.Result.RenditionID {
		t.Fatalf("membership-only scenario re-uploaded the rendition: %s vs %s", op2.Result.RenditionID, op1.Result.RenditionID)
	}
	records, renditions, memberships, _ := prov.Snapshot()
	if records != 1 || renditions != 1 {
		t.Fatalf("provider counts after same-record+same-pdf: records=%d renditions=%d, want 1/1", records, renditions)
	}
	if memberships != 2 {
		t.Fatalf("memberships=%d, want 2 (both collections)", memberships)
	}
}

func TestDedupSameRecordNewRendition(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, prov, _, _ := newFakeService(t, st)

	req1 := seedReq("dedup-r1")
	op1, err := svc.StartImport(context.Background(), req1, bytes.NewReader(ladderPDF("Rendition Parent Title")))
	if err != nil {
		t.Fatal(err)
	}
	if op1.Status != library.ImportCommitted {
		t.Fatalf("first: %s (%+v)", op1.Status, op1.Failure)
	}

	// Same metadata (title matches matrix via document rung), DIFFERENT
	// bytes → rendition added at the parent.
	second := seedReq("dedup-r2")
	second.MetadataHints = library.MetadataHints{Title: "Rendition Parent Title"}
	op2, err := svc.StartImport(context.Background(), second, bytes.NewReader(
		[]byte("%PDF-1.4\nRendition Parent Title.\n%AXIOM-LANG: en\n\nSecond edition: different body bytes.")))
	if err != nil {
		t.Fatal(err)
	}
	if op2.Status != library.ImportCommitted {
		t.Fatalf("second: %s (%+v)", op2.Status, op2.Failure)
	}
	if op2.Result.RecordID != op1.Result.RecordID {
		t.Fatalf("rendition-add scenario created a second record: %s vs %s", op2.Result.RecordID, op1.Result.RecordID)
	}
	if op2.Result.RenditionID == op1.Result.RenditionID {
		t.Fatal("second rendition was not added at the parent")
	}
	records, renditions, _, _ := prov.Snapshot()
	if records != 1 || renditions != 2 {
		t.Fatalf("provider counts: records=%d renditions=%d, want 1/2", records, renditions)
	}
}

func TestDedupAmbiguousNeverAutoMerges(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, prov, _, _ := newFakeService(t, st)

	// Seed two DISTINCT records with the same DOI (EnsureRecord is
	// mutex-safe itself — an outer lock would deadlock).
	y := 2019
	for i, t2 := range []string{"Ambiguous Twin A", "Ambiguous Twin B"} {
		_, _ = prov.EnsureRecord(context.Background(), RecordDraft{
			ExternalKey: fmt.Sprintf("seed-amb-%d", i),
			RecordType:  "book", Title: t2,
			Authors: []Creator{{LastName: "Twin", CreatorType: "author"}},
			Year:    &y,
			DOI:     "10.5555/twin-doi",
		})
	}

	op, err := svc.StartImport(context.Background(), withDOI(seedReq("dedup-amb"), "10.5555/twin-doi"), bytes.NewReader(ladderPDF("Twin incoming title")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportAwaitingConfirm {
		t.Fatalf("ambiguous match auto-merged: status %s", op.Status)
	}
	if len(op.Decisions) != 1 || len(op.Decisions[0].Candidates) != 2 {
		t.Fatalf("want the duplicate decision with BOTH records, got %+v", op.Decisions)
	}
	records, _, _, _ := prov.Snapshot()
	if records != 2 {
		t.Fatalf("records=%d, want exactly the 2 seeded (no third created)", records)
	}
	// Confirm the first twin → links there, adds rendition.
	confirmed, err := svc.ConfirmImport(context.Background(), op.ImportID, op.Decisions[0].DecisionID, op.Decisions[0].Candidates[0].CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != library.ImportCommitted || confirmed.Result.RecordID != "FAKEREC1" {
		t.Fatalf("confirm did not link the chosen record: %+v", confirmed)
	}
	r2, att2, _, _ := prov.Snapshot()
	if r2 != 2 || att2 != 1 {
		t.Fatalf("after confirm: records=%d renditions=%d, want 2/1", r2, att2)
	}
}

// Pagination sonde (DoD): the dedup scan walks EVERY catalog page — a
// DOI match on the LAST page must link there (no 6th record), and the
// provider must have served multiple pages during the import's scans.
func TestDedupScanFollowsEveryPage(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, prov, _, _ := newFakeService(t, st)

	// 5 seeded records (page size 2 → 3 pages); the DOI target is the
	// LAST one — a first-page-only scan would miss it and create a 6th.
	y := 2019
	for i := 0; i < 4; i++ {
		_, _ = prov.EnsureRecord(context.Background(), RecordDraft{
			ExternalKey: fmt.Sprintf("page-filler-%d", i),
			RecordType:  "book", Title: fmt.Sprintf("Filler Work %d", i),
			Authors: []Creator{{LastName: "Filler", CreatorType: "author"}},
			Year:    &y,
		})
	}
	_, _ = prov.EnsureRecord(context.Background(), RecordDraft{
		ExternalKey: "page-target", RecordType: "book", Title: "Paged Target",
		Authors: []Creator{{LastName: "Pager", CreatorType: "author"}}, Year: &y,
		DOI: "10.5555/paged-doi",
	})

	pagesBefore := prov.PagesServed
	op, err := svc.StartImport(context.Background(), withDOI(seedReq("dedup-pages"), "10.5555/paged-doi"), bytes.NewReader(ladderPDF("Paged incoming title")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportCommitted {
		t.Fatalf("status = %s (%+v)", op.Status, op.Failure)
	}
	if op.Result.RecordID != "FAKEREC5" {
		t.Fatalf("last-page match did not link the target record: %s, want FAKEREC5", op.Result.RecordID)
	}
	records, renditions, _, _ := prov.Snapshot()
	if records != 5 || renditions != 1 {
		t.Fatalf("provider counts: records=%d renditions=%d, want 5/1", records, renditions)
	}
	if served := prov.PagesServed - pagesBefore; served < 3 {
		t.Fatalf("the scans served %d page(s) — pagination broken (5 records at page size 2)", served)
	}
}

// ---------------------------------------------------------------------------
// 4. Kill/Resume between EVERY step (DoD table): crash after state entry
// and crash after provider write, for every saga state — resume produces
// EXACTLY ONE committed artifact set; replay mints no second provider row.

func TestKillResumeBetweenEveryStep(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()

	type killPoint struct {
		name     string
		state    string // afterState halt
		provider string // afterProviderWrite halt
	}
	points := []killPoint{
		{"received", "received", ""},
		{"inspecting", "inspecting", "inspecting"},
		{"resolving_metadata", "resolving_metadata", "resolving_metadata"},
		{"awaiting", "awaiting_confirmation", ""},
		{"ensuring_collections", "ensuring_collections", "ensuring_collections"},
		{"creating_record", "creating_record", "creating_record"},
		{"uploading_rendition", "uploading_rendition", "uploading_rendition"},
		{"verifying", "verifying", "verifying"},
	}

	stagingRoot := t.TempDir()
	run := func(t *testing.T, state, provider string, wantAwaiting bool) {
		svc, prov, _, _ := newServiceOver(t, st, NewFakeProvider(), stagingRoot)
		if state != "" {
			svc.halt.afterState = map[string]int{state: 1}
		}
		if provider != "" {
			svc.halt.afterProviderWrite = map[string]int{provider: 1}
		}
		req := seedReq("kill-" + state + "-" + provider)
		if wantAwaiting {
			// The ambiguous fixture drives the saga INTO
			// awaiting_confirmation — the kill point under test.
			req.MetadataHints.Title = StandardLadderFixtures.AmbiguousTitle
		}
		op, err := svc.StartImport(context.Background(), req, bytes.NewReader(contractsuite.SeedContent))
		// Self-check: the armed halt MUST have tripped — any other
		// outcome (nil error, real failure) means the crash mechanism
		// itself broke, and this table would pass as a no-op.
		if !errors.Is(err, ErrHaltSimulated) {
			t.Fatalf("kill at %s/%s: StartImport err=%v, want the simulated-crash sentinel", state, provider, err)
		}
		if op.ImportID == "" {
			// The crash fired before the operation DTO was built (every
			// halt seam returns no op) — the durable row is the identity.
			r0, rerr := st.GetByIdempotencyKey(context.Background(), req.IdempotencyKey)
			if rerr != nil {
				t.Fatalf("kill at %s/%s: import row missing after the crash: %v", state, provider, rerr)
			}
			op.ImportID = r0.ImportID
		}

		// "Process restart": a FRESH service over the same store, provider
		// and staging (external systems survive the crash); boot-time
		// crash recovery resumes in-flight imports.
		fresh, _, _, _ := newServiceOver(t, st, prov, stagingRoot)
		if _, err := fresh.ResumeInflight(context.Background()); err != nil {
			t.Fatalf("resume inflight after kill at %s/%s: %v", state, provider, err)
		}
		row, err := fresh.store.GetImport(context.Background(), op.ImportID)
		if err != nil {
			t.Fatalf("import row lost after kill: %v", err)
		}
		final := row2op(t, fresh, row)
		if final.Status == library.ImportAwaitingConfirm {
			// The crash landed on a caller decision: confirm resumes it.
			final, err = fresh.ConfirmImport(context.Background(), row.ImportID,
				final.Decisions[0].DecisionID, final.Decisions[0].Candidates[0].CandidateID)
			if err != nil {
				t.Fatalf("confirm after kill at %s/%s: %v", state, provider, err)
			}
		}
		if final.Status != library.ImportCommitted {
			t.Fatalf("kill at %s/%s → resume status %s (%+v)", state, provider, final.Status, final.Failure)
		}
		if final.Result.Revision.ContentHash != revision.HashContent(contractsuite.SeedContent) {
			t.Fatalf("committed revision hash drifted: %+v", final.Result)
		}
		// Exactly-once sonde (DB side): the committed row's revision_id
		// must resolve to a real library_source_revisions row — a
		// stamp-then-short-circuit seam would dangle here.
		crow, cerr := fresh.store.GetImport(context.Background(), op.ImportID)
		if cerr != nil {
			t.Fatalf("kill at %s/%s: final row load: %v", state, provider, cerr)
		}
		var revOK bool
		if serr := st.pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM library_source_revisions
			   WHERE source_id = $1 AND record_id = $2 AND rendition_id = $3 AND revision_id = $4)`,
			fresh.cfg.SourceID, crow.RecordID, crow.RenditionID, crow.RevisionID).Scan(&revOK); serr != nil {
			t.Fatalf("kill at %s/%s: revision sonde: %v", state, provider, serr)
		}
		if !revOK {
			t.Fatalf("kill at %s/%s: revision_id %d does not resolve to a revision row (dangling stamp)", state, provider, crow.RevisionID)
		}
		records, renditions, memberships, _ := prov.Snapshot()
		if records != 1 || renditions != 1 || memberships != 0 {
			t.Fatalf("kill at %s/%s → provider counts %d/%d/%d, want 1/1/0", state, provider, records, renditions, memberships)
		}

		// Replay with the same idempotency key → same import, no second row.
		replay, err := fresh.StartImport(context.Background(), req, bytes.NewReader(contractsuite.SeedContent))
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if replay.ImportID != final.ImportID {
			t.Fatalf("replay minted a second import: %s vs %s", replay.ImportID, final.ImportID)
		}
		records2, renditions2, _, _ := prov.Snapshot()
		if records2 != 1 || renditions2 != 1 {
			t.Fatalf("replay doubled provider rows: %d/%d", records2, renditions2)
		}
	}

	for _, p := range points {
		p := p
		wantAwaiting := p.name == "awaiting"
		t.Run("after_state/"+p.name, func(t *testing.T) { run(t, p.state, "", wantAwaiting) })
		if p.provider != "" {
			t.Run("after_provider_write/"+p.name, func(t *testing.T) { run(t, "", p.provider, false) })
		}
	}
}

// row2op loads the operation DTO for a row (test helper).
func row2op(t *testing.T, svc *Service, row ImportRow) library.ImportOperation {
	t.Helper()
	op, err := svc.GetImport(context.Background(), library.ImportRef{ImportID: row.ImportID})
	if err != nil {
		t.Fatalf("get import: %v", err)
	}
	return op
}

// ---------------------------------------------------------------------------
// 5. Idempotency + contract precedence probes (service level; the suite
// covers them too — these pin the DB-backed behavior explicitly).

func TestIdempotencyReplayAndPayloadMismatch(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)
	ctx := context.Background()

	req := seedReq("idem-1")
	first, err := svc.StartImport(ctx, req, bytes.NewReader(contractsuite.SeedContent))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.StartImport(ctx, req, bytes.NewReader(contractsuite.SeedContent))
	if err != nil {
		t.Fatal(err)
	}
	if first.ImportID != second.ImportID || first.Status != second.Status {
		t.Fatalf("replay diverged: %s/%s", first.ImportID, second.ImportID)
	}
	// Pin the payload identity formula: sha256(canonical request JSON ||
	// content) — the frozen DigestWith shape.
	stored, err := st.GetByIdempotencyKey(ctx, req.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PayloadHash != revision.HashContent(append(meta, contractsuite.SeedContent...)) {
		t.Fatalf("payload hash %s drifted from sha256(canonical request || content)", stored.PayloadHash)
	}
	diverge := seedReq("idem-1")
	diverge.MetadataHints.Title = "A Different Title"
	_, err = svc.StartImport(ctx, diverge, bytes.NewReader(contractsuite.SeedContent))
	var mm *contracterr.IdempotencyMismatch
	if !errors.As(err, &mm) || mm.Key != "idem-1" {
		t.Fatalf("payload divergence must be an IdempotencyMismatch, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers

func provenanceOf(t *testing.T, st *Store, importID string) []ProvenanceRow {
	t.Helper()
	rows, err := st.ListProvenance(context.Background(), importID)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	return rows
}

func provHas(rows []ProvenanceRow, field, source string, applied bool) bool {
	for _, r := range rows {
		if r.Field == field && r.Source == source && r.Applied == applied {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 6. Confirm/retry semantics (review rounds).

// TestConfirmOverlaysInsteadOfReplacing — Major-1 regression: a
// document-verified field the chosen candidate does NOT carry survives
// the confirm (its document/applied provenance row stays truthful).
func TestConfirmOverlaysInsteadOfReplacing(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)

	// Document: title differs from the candidate's, language=en (the
	// crossref fixture carries title/authors/year/publisher — no
	// language). Confirming the candidate must REPLACE title (deliberate
	// choice) but KEEP language.
	content := []byte("%PDF-1.4\nNetwork Effects intro.\n%AXIOM-LANG: en\n\nBody.")
	req := seedReq("ovl-1")
	req.MetadataHints.Title = StandardLadderFixtures.AmbiguousTitle
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportAwaitingConfirm {
		t.Fatalf("status = %s, want awaiting_confirmation", op.Status)
	}
	confirmed, err := svc.ConfirmImport(context.Background(), op.ImportID,
		op.Decisions[0].DecisionID, op.Decisions[0].Candidates[0].CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != library.ImportCommitted {
		t.Fatalf("confirm: %s (%+v)", confirmed.Status, confirmed.Failure)
	}
	bib := confirmed.Result.Revision.Bibliography
	if bib.Title != "Network Effects in Platforms" {
		t.Fatalf("chosen title did not win: %q", bib.Title)
	}
	if bib.Language != "en" {
		t.Fatalf("document-verified language lost on confirm: %q", bib.Language)
	}
}

// TestConfirmRerouteWritesUserProvenance — Major-2 regression: the
// bibliography→duplicate reroute path commits the chosen fields WITH
// applied user provenance rows (the effective value must never lack its
// applied audit row).
func TestConfirmRerouteWritesUserProvenance(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, prov, _, _ := newFakeService(t, st)

	// Two same-DOI twins make the confirmed bibliography dedup-ambiguous.
	y := 2021
	for i, t2 := range []string{"Reroute Twin A", "Reroute Twin B"} {
		_, _ = prov.EnsureRecord(context.Background(), RecordDraft{
			ExternalKey: fmt.Sprintf("seed-rr-%d", i),
			RecordType:  "journalArticle", Title: t2,
			Authors: []Creator{{LastName: "Twin", CreatorType: "author"}},
			Year:    &y,
			DOI:     "10.5555/reroute-doi",
		})
	}

	req := seedReq("rr-1")
	req.RecordType = "journalArticle"
	req.MetadataHints = library.MetadataHints{Title: StandardLadderFixtures.AmbiguousTitle, DOI: "10.5555/reroute-doi"}
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(ladderPDF("Network Effects intro")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportAwaitingConfirm {
		t.Fatalf("bibliography decision missing: %s", op.Status)
	}
	// Confirm the bibliography → the same-DOI twins make dedup ambiguous
	// → the import REROUTES to dec-duplicate (not a conflict dead-end).
	rerouted, err := svc.ConfirmImport(context.Background(), op.ImportID,
		op.Decisions[0].DecisionID, op.Decisions[0].Candidates[0].CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if rerouted.Status != library.ImportAwaitingConfirm || len(rerouted.Decisions) != 1 {
		t.Fatalf("reroute: status %s decisions %+v", rerouted.Status, rerouted.Decisions)
	}
	// The chosen fields already carry their applied user provenance —
	// BEFORE the duplicate decision resolves.
	prov2 := provenanceOf(t, st, op.ImportID)
	if !provHas(prov2, "title", "user", true) {
		t.Fatalf("reroute left the chosen fields without applied user provenance: %+v", prov2)
	}
	// Resolve the duplicate → commits; the applied user row persists.
	final, err := svc.ConfirmImport(context.Background(), op.ImportID,
		rerouted.Decisions[0].DecisionID, rerouted.Decisions[0].Candidates[0].CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != library.ImportCommitted {
		t.Fatalf("duplicate confirm: %s (%+v)", final.Status, final.Failure)
	}
	prov3 := provenanceOf(t, st, op.ImportID)
	if !provHas(prov3, "title", "user", true) || !provHas(prov3, "title", "document", true) {
		t.Fatalf("committed fields lack applied provenance: %+v", prov3)
	}
	if final.Result.Revision.Bibliography.Title != "Network Effects in Platforms" {
		t.Fatalf("committed title = %q, want the chosen candidate's", final.Result.Revision.Bibliography.Title)
	}
}

// TestConfirmCrashWindowHeals — the confirm crash window (resolve step
// reached done, but the decision_resolved event / state transition
// never happened) must HEAL on the next confirm instead of dead-ending
// on the missing decision detail: recover forward, commit.
func TestConfirmCrashWindowHeals(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)

	req := seedReq("confirm-crash-window")
	req.MetadataHints.Title = StandardLadderFixtures.AmbiguousTitle
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(ladderPDF("Network Effects intro text")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportAwaitingConfirm {
		t.Fatalf("setup: status = %s", op.Status)
	}

	// Craft the orphan directly: resolve detail persisted done with NO
	// pending decision (and the post-scan plan a real confirm had
	// already folded), status still awaiting, no decision_resolved event.
	row, err := st.GetImport(context.Background(), op.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	det, err := svc.loadResolveDetail(context.Background(), op.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	det.Pending = nil
	det.Plan = PlacementPlan{AddRendition: true}
	if err := svc.persistResolveDetail(context.Background(), row, det, "done"); err != nil {
		t.Fatal(err)
	}

	// The decision/candidate ids are deliberately arbitrary — the heal
	// short-circuits before decision validation.
	final, err := svc.ConfirmImport(context.Background(), op.ImportID, "dec-bibliography", "whatever")
	if err != nil {
		t.Fatalf("confirm must heal the crash window, got %v", err)
	}
	if final.Status != library.ImportCommitted {
		t.Fatalf("healed confirm: status %s (%+v)", final.Status, final.Failure)
	}
}

// failingRenditions wraps a RenditionWriter and fails the first N
// EnsureRendition calls with an Unavailable-class error (retry injection).
type failingRenditions struct {
	inner RenditionWriter
	mu    sync.Mutex
	fail  int
}

func (f *failingRenditions) EnsureRendition(ctx context.Context, d RenditionDraft) (string, error) {
	f.mu.Lock()
	if f.fail > 0 {
		f.fail--
		f.mu.Unlock()
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable, "probe: rendition writer unavailable")
	}
	f.mu.Unlock()
	return f.inner.EnsureRendition(ctx, d)
}

func (f *failingRenditions) EnsureMembership(ctx context.Context, rec, col string) error {
	return f.inner.EnsureMembership(ctx, rec, col)
}

// siblingConflictCollections rejects one configured path segment with the
// provider-conflict sentinel (the same-named-sibling state the Zotero
// provider can really produce; the fake's name-keyed tree cannot).
type siblingConflictCollections struct {
	segment string
}

func (w siblingConflictCollections) ResolvePath(_ context.Context, segments []string, _ bool) (string, error) {
	for _, seg := range segments {
		if seg == w.segment {
			return "", ErrProviderConflict
		}
	}
	return "FAKECOLCONFLICT", nil
}

// TestSiblingConflictIsTerminalConflict — the DoD nail "Gleichnamige
// Geschwister = Konflikt (Fehler, kein Raten)": Conflict class, terminal,
// no record created, no pick.
func TestSiblingConflictIsTerminalConflict(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, prov, _, _ := newFakeService(t, st)
	svc.ports.Collections = siblingConflictCollections{segment: "Zwillinge"}

	req := seedReq("sibl-1")
	req.Target = library.ImportTarget{LibraryID: "users/0", CollectionPath: []string{"Eins", "Zwillinge"}, CreateMissing: true}
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(contractsuite.SeedContent))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportTerminalFailed {
		t.Fatalf("status = %s, want terminal_failed", op.Status)
	}
	if op.Failure == nil || op.Failure.Code != "CONFLICT" {
		t.Fatalf("failure = %+v, want CONFLICT", op.Failure)
	}
	if recs, _, _, _ := prov.Snapshot(); recs != 0 {
		t.Fatalf("sibling conflict created %d record(s) — no pick, no create", recs)
	}
}

// TestRetryResumesAndCommits — the retry SUCCESS path: an injected
// Unavailable rendition write fails the saga retryably; RetryImport
// continues from the failed state and commits. The route's poll
// semantics: a STILL-failing retry returns the failed operation, not an
// error.
func TestRetryResumesAndCommits(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, prov, _, _ := newFakeService(t, st)
	svc.ports.Renditions = &failingRenditions{inner: prov, fail: 1}

	op, err := svc.StartImport(context.Background(), seedReq("retry-ok"), bytes.NewReader(contractsuite.SeedContent))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportRetryableFailed {
		t.Fatalf("injected failure: status = %s (%+v)", op.Status, op.Failure)
	}

	// Retry drives the saga to committed (the second write succeeds).
	final, err := svc.RetryImport(context.Background(), op.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != library.ImportCommitted {
		t.Fatalf("retry: status = %s (%+v)", final.Status, final.Failure)
	}
	if recs, atts, _, _ := prov.Snapshot(); recs != 1 || atts != 1 {
		t.Fatalf("retry provider counts %d/%d, want 1/1", recs, atts)
	}
}

// TestConcurrentRetryNoSpuriousInternal — the event-seq race witness:
// parallel retries on one retryable-failed import never surface a
// non-conflict error (the seq allocation is advisory-locked; the loser
// of the guarded transition gets a Conflict). Exactly one winner drives.
func TestConcurrentRetryNoSpuriousInternal(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)
	// Permanent failure: every retry stays retryable_failed (pollable).
	svc.ports.Renditions = &failingRenditions{inner: NewFakeProvider(), fail: 1 << 30}

	op, err := svc.StartImport(context.Background(), seedReq("retry-race"), bytes.NewReader(contractsuite.SeedContent))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportRetryableFailed {
		t.Fatalf("setup: status = %s", op.Status)
	}

	const rounds = 15
	var conflicts, oks, others int32
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		for g := 0; g < 2; g++ {
			go func() {
				defer wg.Done()
				op, err := svc.RetryImport(context.Background(), op.ImportID)
				switch {
				case err == nil && op.Status == library.ImportRetryableFailed:
					atomic.AddInt32(&oks, 1) // winner of the transition, saga failed again → pollable op
				case err != nil && contracterrClassIs(err, contracterr.ClassConflict):
					atomic.AddInt32(&conflicts, 1) // guard loser
				default:
					atomic.AddInt32(&others, 1) // spurious — must stay 0
				}
			}()
		}
	}
	wg.Wait()
	if others != 0 {
		t.Fatalf("spurious non-conflict errors under concurrent retry: %d (oks=%d conflicts=%d)", others, oks, conflicts)
	}
	if oks == 0 || conflicts == 0 {
		t.Fatalf("expected both outcome classes (oks=%d conflicts=%d)", oks, conflicts)
	}
	// Exactly one retry event per won transition — the guard loser
	// writes no event.
	events, err := svc.store.ListEvents(context.Background(), op.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	retryEvents := 0
	for _, e := range events {
		if e.Kind == "retry" {
			retryEvents++
		}
	}
	if retryEvents != int(oks) {
		t.Fatalf("retry events = %d, want %d (one per won transition)", retryEvents, oks)
	}
}

// TestResumeVsRetryRaceNoSpuriousInternal — boot-resume vs user-retry:
// racing ResumeInflight against two RetryImports never surfaces a
// spurious error from the double-drive (the seq allocation is
// advisory-locked; the guarded transition serializes the retries).
// The resume may honestly report the injected Unavailable fault; a
// Conflict comes from the retries' guard; anything else is spurious.
func TestResumeVsRetryRaceNoSpuriousInternal(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)
	// Permanent failure: every retry stays retryable_failed (pollable).
	svc.ports.Renditions = &failingRenditions{inner: NewFakeProvider(), fail: 1 << 30}

	op, err := svc.StartImport(context.Background(), seedReq("resume-retry-race"), bytes.NewReader(contractsuite.SeedContent))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportRetryableFailed {
		t.Fatalf("setup: status = %s", op.Status)
	}

	const rounds = 15
	var conflicts, oks, others int32
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(3)
		go func() { // boot-resume racer
			defer wg.Done()
			if _, rerr := svc.ResumeInflight(context.Background()); rerr != nil &&
				!contracterrClassIs(rerr, contracterr.ClassUnavailable) {
				atomic.AddInt32(&others, 1) // resuming the injected fault is the honest outcome; anything else is spurious
			}
		}()
		for g := 0; g < 2; g++ {
			go func() {
				defer wg.Done()
				rop, rerr := svc.RetryImport(context.Background(), op.ImportID)
				switch {
				case rerr == nil && rop.Status == library.ImportRetryableFailed:
					atomic.AddInt32(&oks, 1) // winner of the transition, saga failed again → pollable op
				case rerr != nil && contracterrClassIs(rerr, contracterr.ClassConflict):
					atomic.AddInt32(&conflicts, 1) // guard loser
				default:
					atomic.AddInt32(&others, 1) // spurious — must stay 0
				}
			}()
		}
	}
	wg.Wait()
	if others != 0 {
		t.Fatalf("spurious outcomes under resume/retry race: %d (oks=%d conflicts=%d)", others, oks, conflicts)
	}
	if oks == 0 || conflicts == 0 {
		t.Fatalf("expected both outcome classes (oks=%d conflicts=%d)", oks, conflicts)
	}
}

func contracterrClassIs(err error, want contracterr.Class) bool {
	class, ok := contracterr.ClassOf(err)
	return ok && class == want
}

// ClaimIdentifiers sonde: the unique rule is loud across records.
func TestClaimIdentifiersUniqueRule(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	if err := st.ClaimIdentifiers(ctx, "rec-a", "10.5555/claim-doi", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimIdentifiers(ctx, "rec-a", "https://doi.org/10.5555/claim-doi", ""); err != nil {
		t.Fatal(err) // same record, normalized-identical → silent re-claim
	}
	err := st.ClaimIdentifiers(ctx, "rec-b", "10.5555/CLAIM-DOI", "")
	if err == nil || !strings.Contains(err.Error(), "belongs to record rec-a") {
		t.Fatalf("second record claiming the same DOI must conflict loudly, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Migration + staging DoD probes.

func TestMigrationsFreshAndIdempotent(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	// Re-running over the applied set is a no-op (own ledger).
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var n int
	if err := st.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM library_schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("library ledger empty")
	}
}

func TestNoBlobColumnsInLibraryTables(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	rows, err := st.pool.Query(context.Background(), `
		SELECT table_name, column_name, data_type FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name LIKE 'library_%' AND data_type = 'bytea'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var tb, col, dt string
		_ = rows.Scan(&tb, &col, &dt)
		t.Fatalf("BLOB column in library tables: %s.%s (%s)", tb, col, dt)
	}
}

func TestStagingHashedAndRetention(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	root := t.TempDir()
	stg := NewStaging(root)
	content := []byte("%PDF-1.4 staged fixture")
	sha, err := stg.StoreImport(content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "library_staging", sha)); err != nil {
		t.Fatalf("staged file missing under its hash: %v", err)
	}
	// Descriptor row, not a BLOB.
	if err := st.CreateImport(context.Background(), ImportRow{
		IdempotencyKey: "staging-1", PayloadHash: "x", RecordType: "book",
		RequestJSON: []byte("{}"), Status: library.ImportReceived,
		StagingSHA256: sha, StagingSize: int64(len(content)), MediaType: "application/pdf",
	}); err != nil {
		t.Fatal(err)
	}
	// Retention: referenced staging survives, foreign stale files go.
	stale := filepath.Join(root, "library_staging", strings.Repeat("a", 64))
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(stale, old, old)
	removed, err := st.CleanupStaging(context.Background(), stg, time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v, want 1", removed, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale staging file survived retention")
	}
	if _, err := os.Stat(filepath.Join(root, "library_staging", sha)); err != nil {
		t.Fatalf("referenced staging file was deleted: %v", err)
	}
}

// TestStagingSizeLimitBoundary — the streamed limit is exact: exactly N
// bytes stage and commit at limit N; N+1 fails InvalidArgument with NO
// temp trace; a rejected (invalid-magic) intake leaves no temp either.
func TestStagingSizeLimitBoundary(t *testing.T) {
	stg := NewStaging(t.TempDir())
	const n = 100
	tmp, sha, size, _, err := stg.Stage(bytes.NewReader(make([]byte, n)), n)
	if err != nil {
		t.Fatalf("stage at exactly the limit: %v", err)
	}
	if size != n {
		t.Fatalf("size = %d, want %d", size, n)
	}
	if err := stg.Commit(tmp, sha); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := os.Stat(stg.Path(sha)); err != nil {
		t.Fatalf("committed staging file missing: %v", err)
	}

	// One byte over the limit: InvalidArgument, and the temp is gone.
	_, _, _, _, err = stg.Stage(bytes.NewReader(make([]byte, n+1)), n)
	if !contracterrClassIs(err, contracterr.ClassInvalidArgument) {
		t.Fatalf("over-limit stage: %v, want invalid_argument", err)
	}
	temps, _ := filepath.Glob(filepath.Join(stg.root, ".stage-*"))
	if len(temps) != 0 {
		t.Fatalf("overflow left staging temps behind: %v", temps)
	}

	// A rejected intake (magic bytes) leaves no temp in the service's
	// staging root either — the deferred Discard ran.
	st, cleanup := testStore(t)
	defer cleanup()
	svc, _, _, _ := newFakeService(t, st)
	_, err = svc.StartImport(context.Background(), seedReq("magic-reject-no-temp"), bytes.NewReader([]byte("<html>not a document</html>")))
	if !contracterrClassIs(err, contracterr.ClassInvalidArgument) {
		t.Fatalf("html intake: %v, want invalid_argument", err)
	}
	temps, _ = filepath.Glob(filepath.Join(svc.staging.root, ".stage-*"))
	if len(temps) != 0 {
		t.Fatalf("rejected intake left staging temps behind: %v", temps)
	}
}

func TestNamingConventionFromImport(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()
	svc, prov, _, _ := newFakeService(t, st)
	req := seedReq("naming-1")
	req.MetadataHints.DOI = StandardLadderFixtures.UniqueDOI
	op, err := svc.StartImport(context.Background(), req, bytes.NewReader(ladderPDF("Document Title From Content")))
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != library.ImportCommitted {
		t.Fatalf("status %s (%+v)", op.Status, op.Failure)
	}
	// Filename from VERIFIED document metadata, not the crossref hit.
	prov.mu.Lock()
	var filename string
	for _, r := range prov.records {
		if r.providerID == op.Result.RecordID {
			for _, a := range r.renditions {
				filename = a.filename
			}
		}
	}
	prov.mu.Unlock()
	want := SchemaFilename([]Creator{{LastName: "Example", CreatorType: "author"}}, 2019, "Document Title From Content")
	if filename != want {
		t.Fatalf("filename %q, want the schema name from verified metadata %q", filename, want)
	}
}
