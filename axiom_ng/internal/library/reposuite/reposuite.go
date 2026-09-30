// Package reposuite is the Library repository contract suite (F12,
// #306): ONE suite, run against EVERY engine implementation — the
// PostgreSQL repository (pglib) and the SQLite repository (sqlite) in
// the CI engine matrix. Identical fixtures, identical assertions, no
// engine-specific skips: parity is a test result, not a claim.
//
// The runner GUARDS against silent thinning: once an engine subtest is
// entered (the engine declared itself runnable by opening successfully),
// NO test inside may skip — a Skip is recorded and fails the run
// (TestSkipGuardProbes proves the guard's teeth). Engine AVAILABILITY
// (no PostgreSQL DSN in the environment) is decided BEFORE the subtest
// starts; that is the only sanctioned skip point, and CI's matrix legs
// select engines explicitly so a selected leg cannot quietly skip.
package reposuite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	contracts "github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
)

// Fixture is one import fixture (identical for every engine — the DM03
// type-mapping rows ride along: uuid text, µs UTC time, canonical JSON,
// enum vocabulary).
type importFixture struct {
	key     string
	hash    string
	rtype   string
	request string // canonical JSON
	sha     string
	size    int64
	media   string
}

func seedImport(n int) importFixture {
	return importFixture{
		key:     fmt.Sprintf("idem-%d", n),
		hash:    fmt.Sprintf("payload-%d", n),
		rtype:   "book",
		request: fmt.Sprintf(`{"idempotency_key":"idem-%d","record_type":"book"}`, n),
		sha:     fmt.Sprintf("deadbeef%056d", n),
		size:    int64(1000 + n),
		media:   "application/pdf",
	}
}

func (f importFixture) row(status contracts.ImportStatus) library.ImportRow {
	return library.ImportRow{
		IdempotencyKey: f.key,
		PayloadHash:    f.hash,
		RecordType:     f.rtype,
		RequestJSON:    []byte(f.request),
		Status:         status,
		StagingSHA256:  f.sha,
		StagingSize:    f.size,
		MediaType:      f.media,
	}
}

// skipRecord is the anti-thinning ledger: every Skip inside a running
// engine is a violation (the suite must not silently thin out).
type skipRecord struct {
	mu    sync.Mutex
	skips []string
}

func (r *skipRecord) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skips = append(r.skips, name)
}

func (r *skipRecord) violations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.skips...)
}

// guardedT wraps *testing.T to intercept Skip/Skipf — the engine matrix's
// red line: a running engine's suite body has NO sanctioned skip.
type guardedT struct {
	*testing.T
	rec *skipRecord
}

// guard wraps one subtest's t (the runner wraps every level).
func guard(t *testing.T, rec *skipRecord) *guardedT { return &guardedT{T: t, rec: rec} }

func (g *guardedT) Skip(args ...any) {
	g.rec.add(g.Name())
	g.T.Skip(args...)
}

func (g *guardedT) Skipf(format string, args ...any) {
	g.rec.add(g.Name())
	g.T.Skipf(format, args...)
}

// SkipNow is guarded too — the third spelling of a skip (F12 review).
func (g *guardedT) SkipNow() {
	g.rec.add(g.Name())
	g.T.SkipNow()
}

// Open is one engine's factory: it must open a FRESH, MIGRATED
// repository (fresh-install path) and return it with its cleanup. An
// unavailable engine (no DSN) returns ok=false BEFORE any subtest runs —
// the availability skip, the only sanctioned one.
type Open func(t *testing.T) (repo library.Repository, cleanup func(), ok bool)

// Run executes the whole repository contract suite against one engine
// factory under the given engine label.
func Run(t *testing.T, engine string, open Open) {
	rec := &skipRecord{}
	t.Run("engine="+engine, func(t *testing.T) {
		repo, cleanup, ok := open(t)
		if !ok {
			t.Skipf("engine %s not available in this environment", engine)
			return
		}
		defer cleanup()
		// The engine declared itself runnable: from here on, skips are
		// violations — collected and failed AFTER the body so one skip
		// cannot hide the rest of the suite's verdicts.
		defer func() {
			if v := rec.violations(); len(v) > 0 {
				t.Fatalf("engine %s thinned the suite with engine-specific skips (forbidden): %v", engine, v)
			}
		}()
		g := &guardedT{T: t, rec: rec}
		g.Run("imports", func(t *testing.T) { suiteImports(guard(t, rec), repo) })
		g.Run("events", func(t *testing.T) { suiteEvents(guard(t, rec), repo) })
		g.Run("steps", func(t *testing.T) { suiteSteps(guard(t, rec), repo) })
		g.Run("identifiers", func(t *testing.T) { suiteIdentifiers(guard(t, rec), repo) })
		g.Run("provenance", func(t *testing.T) { suiteProvenance(guard(t, rec), repo) })
		g.Run("revisions", func(t *testing.T) { suiteRevisions(guard(t, rec), repo) })
		g.Run("anchors-audit", func(t *testing.T) { suiteAnchorsAudit(guard(t, rec), repo) })
		g.Run("writer-lease", func(t *testing.T) { suiteWriterLease(guard(t, rec), repo) })
		g.Run("resume-staging-mirror", func(t *testing.T) { suiteResumeStagingMirror(guard(t, rec), repo) })
	})
}

// ---------------------------------------------------------------------------
// 1. Imports / saga rows

func suiteImports(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	f := seedImport(1)
	if err := repo.CreateImport(ctx, f.row(contracts.ImportReceived)); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.GetByIdempotencyKey(ctx, f.key)
	if err != nil {
		t.Fatalf("get by key: %v", err)
	}
	if got.ImportID == "" {
		t.Fatal("engine must mint an import id (db default or app-side)")
	}
	if got.Status != contracts.ImportReceived || got.PayloadHash != f.hash ||
		got.StagingSize != f.size || got.MediaType != f.media ||
		got.RecordType != f.rtype || got.StagingSHA256 != f.sha {
		t.Fatalf("row roundtrip drift: %+v", got)
	}
	// JSON roundtrip is SEMANTIC (JSONB normalizes key order on the
	// PostgreSQL side; SQLite stores the exact bytes — both are the same
	// JSON document, which is the DM03 mapping rule both engines share).
	var wantReq, gotReq map[string]any
	if err := json.Unmarshal([]byte(f.request), &wantReq); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got.RequestJSON, &gotReq); err != nil {
		t.Fatalf("request_json is not valid JSON after roundtrip: %v", err)
	}
	if fmt.Sprint(wantReq) != fmt.Sprint(gotReq) {
		t.Fatalf("request_json semantic drift: want %v got %v", wantReq, gotReq)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps must be real, never zero: %+v", got)
	}
	// µs-UTC form (DM03): nanosecond residue would prove a different
	// clock discipline than the frozen wire shape.
	if got.CreatedAt.Nanosecond()%1000 != 0 {
		t.Fatalf("created_at not microsecond-aligned: %v", got.CreatedAt)
	}
	if _, err := repo.GetByIdempotencyKey(ctx, "no-such-key"); !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("absent key must surface ErrRowAbsent, got %v", err)
	}
	byID, err := repo.GetImport(ctx, got.ImportID)
	if err != nil || byID.IdempotencyKey != f.key {
		t.Fatalf("get by id: %v %+v", err, byID)
	}
	if _, err := repo.GetImport(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("absent id must surface ErrRowAbsent, got %v", err)
	}

	// duplicate key: the neutral ErrDuplicateKey (the caller replays).
	f2 := f
	f2.hash = "different-payload"
	if err := repo.CreateImport(ctx, f2.row(contracts.ImportReceived)); !errors.Is(err, library.ErrDuplicateKey) {
		t.Fatalf("duplicate idempotency key must surface ErrDuplicateKey, got %v", err)
	}

	// guarded status CAS: advance, then a stale-expect update loses.
	if err := repo.UpdateImportStatus(ctx, got.ImportID, contracts.ImportResolvingMetadata, "", nil,
		"", "", "", "", "", 0); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := repo.UpdateImportStatus(ctx, got.ImportID, contracts.ImportCommitted,
		contracts.ImportReceived, nil, "", "", "", "", "", 0); !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("stale expect must lose the CAS (ErrRowAbsent), got %v", err)
	}
	// failure columns: set on failure, cleared on leaving the failed state.
	fail := &contracts.ImportFailure{Code: "provider_down", Message: "boom"}
	if err := repo.UpdateImportStatus(ctx, got.ImportID, contracts.ImportRetryableFailed, "", fail,
		"", "", "", "", "", 0); err != nil {
		t.Fatalf("fail: %v", err)
	}
	after, _ := repo.GetImport(ctx, got.ImportID)
	if after.FailureCode != "provider_down" || after.FailureMessage != "boom" {
		t.Fatalf("failure columns must persist: %+v", after)
	}
	if err := repo.UpdateImportStatus(ctx, got.ImportID, contracts.ImportResolvingMetadata, "", nil,
		"", "", "", "", "", 0); err != nil {
		t.Fatalf("retry: %v", err)
	}
	after, _ = repo.GetImport(ctx, got.ImportID)
	if after.FailureCode != "" || after.FailureMessage != "" {
		t.Fatalf("failure columns must clear on leaving a failed status: %+v", after)
	}
	// outcome columns fill COALESCE-style (never clobber set values with empty).
	if err := repo.UpdateImportStatus(ctx, got.ImportID, contracts.ImportCommitted, "", nil,
		"rec-p1", "rend-p1", "coll-p1", "REC1", "REND1", 7); err != nil {
		t.Fatalf("commit: %v", err)
	}
	after, _ = repo.GetImport(ctx, got.ImportID)
	if after.RecordProviderID != "rec-p1" || after.RenditionProviderID != "rend-p1" ||
		after.CollectionProviderID != "coll-p1" || after.RecordID != "REC1" ||
		after.RenditionID != "REND1" || after.RevisionID != 7 {
		t.Fatalf("outcome columns: %+v", after)
	}
	// an empty-string update never clobbers the committed outcome.
	if err := repo.UpdateImportStatus(ctx, got.ImportID, contracts.ImportCommitted, "", nil,
		"", "", "", "", "", 0); err != nil {
		t.Fatalf("re-stamp: %v", err)
	}
	after, _ = repo.GetImport(ctx, got.ImportID)
	if after.RecordID != "REC1" || after.RevisionID != 7 {
		t.Fatalf("empty update clobbered the outcome: %+v", after)
	}
}

// ---------------------------------------------------------------------------
// 2. Events

func suiteEvents(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	f := seedImport(2)
	if err := repo.CreateImport(ctx, f.row(contracts.ImportReceived)); err != nil {
		t.Fatal(err)
	}
	row, _ := repo.GetByIdempotencyKey(ctx, f.key)
	for i, kind := range []string{"state_entered", "decision_offered", "decision_resolved"} {
		if err := repo.AppendEvent(ctx, row.ImportID, kind, map[string]any{"i": i}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	evs, err := repo.ListEvents(ctx, row.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) || e.Kind != []string{"state_entered", "decision_offered", "decision_resolved"}[i] {
			t.Fatalf("event %d drift: %+v", i, e)
		}
		if len(e.Detail) == 0 {
			t.Fatalf("event %d must persist its detail JSON", i)
		}
	}
	// concurrent double-drive: no PK collision, no spurious error — the
	// engine-internal serialization (advisory lock / IMMEDIATE tx) holds.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- repo.AppendEvent(ctx, row.ImportID, "retry", map[string]any{"probe": true})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	evs, _ = repo.ListEvents(ctx, row.ImportID)
	if len(evs) != 11 {
		t.Fatalf("want 11 events after concurrent appends, got %d", len(evs))
	}
	seen := map[int64]bool{}
	for _, e := range evs {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d — serialization broken", e.Seq)
		}
		seen[e.Seq] = true
	}
}

// ---------------------------------------------------------------------------
// 3. Steps

func suiteSteps(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	f := seedImport(3)
	if err := repo.CreateImport(ctx, f.row(contracts.ImportReceived)); err != nil {
		t.Fatal(err)
	}
	row, _ := repo.GetByIdempotencyKey(ctx, f.key)
	if _, err := repo.GetStep(ctx, row.ImportID, "inspecting"); !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("absent step: %v", err)
	}
	if err := repo.UpsertStep(ctx, row.ImportID, "inspecting", "in_progress", "", nil); err != nil {
		t.Fatal(err)
	}
	st, err := repo.GetStep(ctx, row.ImportID, "inspecting")
	if err != nil || st.Attempts != 1 || st.State != "in_progress" {
		t.Fatalf("first book: %+v %v", st, err)
	}
	// re-entry increments attempts (crash/resume visibility)
	for i := 0; i < 2; i++ {
		if err := repo.UpsertStep(ctx, row.ImportID, "inspecting", "in_progress", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	st, _ = repo.GetStep(ctx, row.ImportID, "inspecting")
	if st.Attempts != 3 {
		t.Fatalf("attempts want 3, got %d", st.Attempts)
	}
	// done freezes; provider_ref fills but never clobbers.
	if err := repo.UpsertStep(ctx, row.ImportID, "inspecting", "done", "doc-verified", nil); err != nil {
		t.Fatal(err)
	}
	st, _ = repo.GetStep(ctx, row.ImportID, "inspecting")
	if st.State != "done" || st.ProviderRef != "doc-verified" {
		t.Fatalf("done step: %+v", st)
	}
	if err := repo.UpsertStep(ctx, row.ImportID, "inspecting", "done", "", nil); err != nil {
		t.Fatal(err)
	}
	st, _ = repo.GetStep(ctx, row.ImportID, "inspecting")
	if st.ProviderRef != "doc-verified" || st.Attempts != 3 {
		t.Fatalf("done must freeze (attempts) and keep refs: %+v", st)
	}
}

// ---------------------------------------------------------------------------
// 4. Identifiers

func suiteIdentifiers(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	// normalization rides in the ENGINE (the neutral normalizer): prefix
	// spellings and case must collapse to the same claim.
	if err := repo.ClaimIdentifiers(ctx, "REC-A", "https://doi.org/10.1000/ABC", "978-3-16-148410-0"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := repo.ClaimIdentifiers(ctx, "REC-A", "10.1000/abc", "9783161484100"); err != nil {
		t.Fatalf("re-claim by the same record must be idempotent after normalization: %v", err)
	}
	err := repo.ClaimIdentifiers(ctx, "REC-B", "10.1000/ABC", "")
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassConflict {
		t.Fatalf("foreign claim must be a typed Conflict, got %v", err)
	}
	// the diagnosis names the owner — data state is never auto-merged.
	if err == nil || !strings.Contains(err.Error(), "REC-A") {
		t.Fatalf("conflict must name the owning record, got %v", err)
	}
	// a different DOI is claimable by REC-B.
	if err := repo.ClaimIdentifiers(ctx, "REC-B", "10.1000/xyz", ""); err != nil {
		t.Fatalf("distinct claim: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Provenance

func suiteProvenance(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	f := seedImport(4)
	if err := repo.CreateImport(ctx, f.row(contracts.ImportReceived)); err != nil {
		t.Fatal(err)
	}
	row, _ := repo.GetByIdempotencyKey(ctx, f.key)
	p1 := library.ProvenanceRow{Field: "title", Source: "crossref", ResolverVersion: "v1",
		Confidence: 0.9, Applied: true, Value: "The Book", At: time.Now()}
	if err := repo.AppendProvenance(ctx, row.ImportID, p1); err != nil {
		t.Fatal(err)
	}
	// idempotent: the resumed ladder must not inflate the trail.
	if err := repo.AppendProvenance(ctx, row.ImportID, p1); err != nil {
		t.Fatal(err)
	}
	p2 := library.ProvenanceRow{Field: "title", Source: "open_library", ResolverVersion: "v1",
		Confidence: 0.5, Applied: false, Value: "The Book (Paperback)", At: time.Now()}
	if err := repo.AppendProvenance(ctx, row.ImportID, p2); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListProvenance(ctx, row.ImportID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 provenance rows (idempotent re-append), got %d", len(rows))
	}
	if rows[0].Source != "crossref" || !rows[0].Applied || rows[1].Applied {
		t.Fatalf("provenance order/content drift: %+v", rows)
	}
}

// ---------------------------------------------------------------------------
// 6. Source revisions

func revisionFixture(n int) library.SourceRevisionDomain {
	return library.SourceRevisionDomain{
		SourceID:    "11111111-1111-4111-8111-111111111111",
		RecordID:    fmt.Sprintf("KEY%d", n),
		RenditionID: fmt.Sprintf("ATT%d", n),
		ContentHash: fmt.Sprintf("sha3-%d", n),
		MediaType:   revision.MediaTypePDF,
		Bibliography: revision.Bibliography{
			RecordID: fmt.Sprintf("KEY%d", n), Title: "Fixture", Year: intPtr(2020),
		},
		LocatorCapabilities: revision.LocatorCapabilities{
			Page: &revision.PageCapability{Trust: revision.TrustPhysicalOnly},
		},
		ContentTicket: fmt.Sprintf("lst:%d", n),
		Origin:        "import",
		CreatedAt:     time.Now(),
	}
}

func intPtr(i int) *int { return &i }

func suiteRevisions(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	r1 := revisionFixture(1)
	id1, minted, err := repo.PublishRevision(ctx, r1)
	if err != nil || !minted || id1 != 1 {
		t.Fatalf("first publish: id=%d minted=%v err=%v", id1, minted, err)
	}
	// idempotent re-publish: surviving id, minted=false, no history
	// inflation.
	idAgain, minted2, err := repo.PublishRevision(ctx, r1)
	if err != nil || minted2 || idAgain != id1 {
		t.Fatalf("re-publish: id=%d minted=%v err=%v", idAgain, minted2, err)
	}
	// changed content: next monotonic id.
	r2 := r1
	r2.ContentHash = "sha3-1-changed"
	id2, minted3, err := repo.PublishRevision(ctx, r2)
	if err != nil || !minted3 || id2 != 2 {
		t.Fatalf("changed publish: id=%d minted=%v err=%v", id2, minted3, err)
	}
	// a DIFFERENT rendition of the SAME record has its own monotonic
	// sequence.
	r3 := r1
	r3.RenditionID = "ATT2"
	r3.ContentTicket = "lst:att2"
	id3, _, err := repo.PublishRevision(ctx, r3)
	if err != nil || id3 != 3 {
		t.Fatalf("record-scoped monotonic sequence (rendition switch continues the record's ids): id=%d err=%v", id3, err)
	}
	// latest-of-rendition
	latest, err := repo.LatestRevision(ctx, r1.SourceID, r1.RecordID, r1.RenditionID)
	if err != nil || latest.RevisionID != 2 || latest.Bibliography.Title != "Fixture" {
		t.Fatalf("latest: %+v %v", latest, err)
	}
	if latest.LocatorCapabilities.Page == nil || latest.LocatorCapabilities.Page.Trust != revision.TrustPhysicalOnly {
		t.Fatalf("locator capabilities JSON roundtrip: %+v", latest.LocatorCapabilities)
	}
	// latest-of-record (any rendition — the newest wins). A third
	// revision on the second rendition makes the winner unambiguous.
	r4 := r3
	r4.ContentHash = "sha3-2-changed"
	if _, _, err := repo.PublishRevision(ctx, r4); err != nil {
		t.Fatal(err)
	}
	r4.ContentHash = "sha3-2-changed-again"
	if _, _, err := repo.PublishRevision(ctx, r4); err != nil {
		t.Fatal(err)
	}
	lr, err := repo.LatestRevisionByRecord(ctx, r1.SourceID, r1.RecordID)
	if err != nil || lr.RenditionID != r3.RenditionID || lr.RevisionID != 5 {
		t.Fatalf("latest by record: %+v %v", lr, err)
	}
	// by ticket
	byTicket, err := repo.LatestRevisionByTicket(ctx, r1.ContentTicket)
	if err != nil || byTicket.ContentHash != "sha3-1-changed" {
		t.Fatalf("by ticket: %+v %v", byTicket, err)
	}
	if _, err := repo.LatestRevisionByTicket(ctx, "no-such-ticket"); !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("absent ticket: %v", err)
	}
	// concurrent publishes of one record serialize — one minted id each,
	// strictly monotonic, no lost row.
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rc := revisionFixture(10)
			rc.ContentHash = fmt.Sprintf("sha3-race-%d", i)
			_, _, err := repo.PublishRevision(ctx, rc)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	raced, err := repo.LatestRevision(ctx, revisionFixture(10).SourceID, revisionFixture(10).RecordID, revisionFixture(10).RenditionID)
	if err != nil || raced.RevisionID != 6 {
		t.Fatalf("race must mint all 6 revisions monotonically, latest=%d err=%v", raced.RevisionID, err)
	}
}

// ---------------------------------------------------------------------------
// 7. Anchors & write audit

func suiteAnchorsAudit(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	const scope = "zotero|https://api.example|users/0"
	if _, _, err := repo.LookupProviderAnchor(ctx, scope, "record", "anchor-1"); !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("absent anchor: %v", err)
	}
	// plain put + readback (surviving id, version refresh)
	if _, err := repo.PutProviderAnchor(ctx, scope, "record", "anchor-1", "ZOTERO-1", 3); err != nil {
		t.Fatal(err)
	}
	id, ver, err := repo.LookupProviderAnchor(ctx, scope, "record", "anchor-1")
	if err != nil || id != "ZOTERO-1" || ver != 3 {
		t.Fatalf("anchor lookup: %q %d %v", id, ver, err)
	}
	if _, err := repo.PutProviderAnchor(ctx, scope, "record", "anchor-1", "ZOTERO-1", 9); err != nil {
		t.Fatal(err)
	}
	_, ver, _ = repo.LookupProviderAnchor(ctx, scope, "record", "anchor-1")
	if ver != 9 {
		t.Fatalf("version must refresh on re-put, got %d", ver)
	}
	// audit rows ride the combined path 1:1
	audit := library.WriteAuditRow{
		Scope: scope, Operation: "ensure_record", Anchor: "anchor-2",
		ProviderRef: "ZOTERO-2", Outcome: "created",
		Readback: map[string]any{"key": "ZOTERO-2"},
	}
	surviving, err := repo.PutProviderAnchorWithAudit(ctx, scope, "record", "anchor-2", "ZOTERO-2", 1, audit)
	if err != nil || surviving != "ZOTERO-2" {
		t.Fatalf("anchor+audit: %q %v", surviving, err)
	}
	n, _ := repo.CountWriteAudit(ctx, scope)
	if n != 1 {
		t.Fatalf("audit rows want 1, got %d", n)
	}
	// the STANDALONE audit append (every engine): a bare mutation line
	// lands and counts — column-order or dialect drift fails here.
	if err := repo.AppendWriteAudit(ctx, library.WriteAuditRow{
		Scope: scope, Operation: "ensure_membership", Anchor: "anchor-2",
		ProviderRef: "COLL-9", Outcome: "reused",
	}); err != nil {
		t.Fatalf("append write audit: %v", err)
	}
	n, _ = repo.CountWriteAudit(ctx, scope)
	if n != 2 {
		t.Fatalf("audit rows want 2 after the standalone append, got %d", n)
	}
	// a losing concurrent put returns the SURVIVING id.
	loser, err := repo.PutProviderAnchorWithAudit(ctx, scope, "record", "anchor-2", "ZOTERO-OTHER", 2, audit)
	if err != nil || loser != "ZOTERO-2" {
		t.Fatalf("dedup loser must see the surviving id, got %q %v", loser, err)
	}
	// eviction is provider_id-guarded: only the DEAD id's row goes.
	if err := repo.EvictProviderAnchor(ctx, scope, "record", "anchor-2", "someone-else"); err != nil {
		t.Fatal(err)
	}
	if id, _, err := repo.LookupProviderAnchor(ctx, scope, "record", "anchor-2"); err != nil || id != "ZOTERO-2" {
		t.Fatalf("foreign evict must not remove the anchor: %q %v", id, err)
	}
	if err := repo.EvictProviderAnchor(ctx, scope, "record", "anchor-2", "ZOTERO-2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.LookupProviderAnchor(ctx, scope, "record", "anchor-2"); !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("guarded evict: %v", err)
	}
	// collection kind is a legal anchor (0003)
	if _, err := repo.PutProviderAnchor(ctx, scope, "collection", "path/seg", "COLL-1", 0); err != nil {
		t.Fatalf("collection anchor (migration 0003): %v", err)
	}
}

// ---------------------------------------------------------------------------
// 8. Writer lease

func suiteWriterLease(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	const scope = "probe|single|writer"
	if err := repo.AcquireWriterLease(ctx, scope, "host-a:1:n", library.DefaultWriterLeaseTTL); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	err := repo.AcquireWriterLease(ctx, scope, "host-b:2:n", library.DefaultWriterLeaseTTL)
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassConflict {
		t.Fatalf("second writer must be refused (Conflict), got %v", err)
	}
	var lc *library.WriterLeaseConflict
	if !errors.As(err, &lc) || lc.Owner != "host-a:1:n" {
		t.Fatalf("refusal must diagnose the live owner, got %+v", lc)
	}
	// renew: holder yes, non-holder no.
	if err := repo.RenewWriterLease(ctx, scope, "host-a:1:n"); err != nil {
		t.Fatalf("holder renew: %v", err)
	}
	if err := repo.RenewWriterLease(ctx, scope, "host-b:2:n"); err == nil {
		t.Fatal("non-holder renew must fail")
	}
	// takeover after silence (TTL): a crashed writer's lease is takeable.
	// The heartbeat must AGE past the acquire's ttl — a real wait, then a
	// small-ttl acquire (both engines use the acquiring call's ttl).
	time.Sleep(20 * time.Millisecond)
	if err := repo.AcquireWriterLease(ctx, scope, "host-c:3:n", 10*time.Millisecond); err != nil {
		t.Fatalf("takeover after TTL: %v", err)
	}
	// the old holder's renewal now loses — takeover is visible.
	if err := repo.RenewWriterLease(ctx, scope, "host-a:1:n"); err == nil {
		t.Fatal("stale holder must lose after takeover")
	}
	// graceful release; releasing a foreign lease is a no-op.
	if err := repo.ReleaseWriterLease(ctx, scope, "host-b:2:n"); err != nil {
		t.Fatalf("foreign release must be a no-op: %v", err)
	}
	if err := repo.ReleaseWriterLease(ctx, scope, "host-c:3:n"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// scope is free again.
	if err := repo.AcquireWriterLease(ctx, scope, "host-d:4:n", library.DefaultWriterLeaseTTL); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 9. Resume scan, staging retention set, mirror absence

func suiteResumeStagingMirror(t *guardedT, repo library.Repository) {
	ctx := context.Background()
	// one inflight + one terminal + one awaiting-confirmation import
	fin := seedImport(11)
	if err := repo.CreateImport(ctx, fin.row(contracts.ImportResolvingMetadata)); err != nil {
		t.Fatal(err)
	}
	fterm := seedImport(12)
	if err := repo.CreateImport(ctx, fterm.row(contracts.ImportCommitted)); err != nil {
		t.Fatal(err)
	}
	fawait := seedImport(13)
	if err := repo.CreateImport(ctx, fawait.row(contracts.ImportAwaitingConfirm)); err != nil {
		t.Fatal(err)
	}
	frun := seedImport(14)
	if err := repo.CreateImport(ctx, frun.row(contracts.ImportReceived)); err != nil {
		t.Fatal(err)
	}
	ids, err := repo.ResumeInflightIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for _, f := range []importFixture{fin, frun} {
		row, _ := repo.GetByIdempotencyKey(ctx, f.key)
		if !set[row.ImportID] {
			t.Fatalf("inflight import %s missing from the scan", f.key)
		}
	}
	for _, f := range []importFixture{fterm, fawait} {
		row, _ := repo.GetByIdempotencyKey(ctx, f.key)
		if set[row.ImportID] {
			t.Fatalf("non-inflight import %s must not resume (status %s)", f.key, row.Status)
		}
	}
	// staging retention set carries the distinct referenced hashes
	hashes, err := repo.ReferencedStagingHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []importFixture{fin, fterm, fawait, frun} {
		if !hashes[f.sha] {
			t.Fatalf("referenced staging hash %s missing from the retention set", f.sha)
		}
	}
	if hashes["never-referenced-sha"] {
		t.Fatal("retention set carries a hash no import references")
	}
	// mirror reads: whatever the engine answers, absence is HONEST —
	// never an internal error (the SQLite engine has no mirror; a
	// standalone PostgreSQL library DB has none either).
	if syncAt, err := repo.LastSyncAt(ctx, "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatalf("LastSyncAt must degrade to absence, not error: %v (%v)", syncAt, err)
	}
	if p, err := repo.MirrorRenditionPath(ctx, "11111111-1111-4111-8111-111111111111", "ATT9"); err != nil && !errors.Is(err, library.ErrRowAbsent) {
		t.Fatalf("MirrorRenditionPath must degrade honestly, got %v", err)
	} else if err == nil && p != "" {
		t.Logf("engine has a mirror; path=%s", p)
	}
	if _, ok, err := repo.MirrorCitation(ctx, "11111111-1111-4111-8111-111111111111", "KEY9"); err != nil || !ok {
		if err != nil {
			t.Fatalf("MirrorCitation must degrade honestly, got %v", err)
		}
	}
}
