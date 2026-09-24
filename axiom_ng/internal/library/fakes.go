// fakes.go — deterministic fake providers (F06, #300). F07 ports Zotero
// behind the same ports; until then these prove the component: the
// durable saga, dedup over a FULLY PAGINATED catalog, and the ladder
// decisions with fixtures for every binding scenario.
//
// Determinism: sequential ids from fixed bases; no clock reads (stamps
// are injected). Dedup semantics are REAL (external-key and content-hash
// anchors) — a crashed-then-resumed step must not double-create.
package library

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
)

// ---------------------------------------------------------------------------
// FakeProvider — RecordWriter + RenditionWriter + CollectionWriter +
// CatalogReader in one in-memory provider.

// fakeRecord is one provider-side record.
type fakeRecord struct {
	providerID  string
	externalKey string
	draft       RecordDraft
	// renditions keyed by content hash (the ensure-idempotency anchor)
	renditions map[string]fakeAttachment
	attOrder   []string // insertion order of hashes
}

// fakeAttachment is one provider-side rendition.
type fakeAttachment struct {
	providerID string
	hash       string
	mediaType  string
	filename   string
}

// fakeCollection is one provider-side collection node.
type fakeCollection struct {
	providerID string
	name       string
	parentID   string
}

// FakeProvider is the in-memory provider behind the Library ports.
type FakeProvider struct {
	mu sync.Mutex

	recSeq int
	attSeq int
	colSeq int

	records     []*fakeRecord              // insertion order
	byExternal  map[string]*fakeRecord     // external key → record
	byProvider  map[string]*fakeRecord     // provider id → record
	collections map[string]*fakeCollection // provider id → node
	colChildren map[string]map[string]string
	memberships map[string]struct{} // "recID|colID"

	// PageSize forces pagination of the catalog (default 2 — tests must
	// trip every multi-page path).
	PageSize int

	// Counters (Zähl-Asserts): provider-side write counts and catalog
	// reads.
	RecordsCreated   int
	RenditionsAdded  int
	MembershipsAdded int
	CollectionsReads int
	PagesServed      int
}

// NewFakeProvider returns an empty fake provider.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		byExternal:  map[string]*fakeRecord{},
		byProvider:  map[string]*fakeRecord{},
		collections: map[string]*fakeCollection{},
		colChildren: map[string]map[string]string{},
		memberships: map[string]struct{}{},
		PageSize:    2,
	}
}

// EnsureRecord idempotently ensures the record; the external key is the
// dedup anchor (same key → same provider id, fields refreshed).
func (f *FakeProvider) EnsureRecord(_ context.Context, d RecordDraft) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.byExternal[d.ExternalKey]; ok {
		r.draft = d
		return r.providerID, nil
	}
	f.recSeq++
	id := fmt.Sprintf("FAKEREC%d", f.recSeq)
	r := &fakeRecord{providerID: id, externalKey: d.ExternalKey, draft: d, renditions: map[string]fakeAttachment{}}
	f.records = append(f.records, r)
	f.byExternal[d.ExternalKey] = r
	f.byProvider[id] = r
	f.RecordsCreated++
	return id, nil
}

// EnsureRendition idempotently ensures the file under the parent; the
// content hash is the dedup anchor.
func (f *FakeProvider) EnsureRendition(_ context.Context, d RenditionDraft) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent, ok := f.byProvider[d.ParentProviderID]
	if !ok {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound,
			"fake provider: parent record "+d.ParentProviderID+" unknown")
	}
	if att, ok := parent.renditions[d.ContentHash]; ok {
		return att.providerID, nil
	}
	f.attSeq++
	id := fmt.Sprintf("FAKEATT%d", f.attSeq)
	parent.renditions[d.ContentHash] = fakeAttachment{providerID: id, hash: d.ContentHash, mediaType: d.MediaType, filename: d.Filename}
	parent.attOrder = append(parent.attOrder, d.ContentHash)
	f.RenditionsAdded++
	return id, nil
}

// EnsureMembership idempotently files the record under the collection.
func (f *FakeProvider) EnsureMembership(_ context.Context, providerRecordID, providerCollectionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byProvider[providerRecordID]; !ok {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound,
			"fake provider: record "+providerRecordID+" unknown")
	}
	if _, ok := f.collections[providerCollectionID]; !ok {
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound,
			"fake provider: collection "+providerCollectionID+" unknown")
	}
	key := providerRecordID + "|" + providerCollectionID
	if _, dup := f.memberships[key]; dup {
		return nil
	}
	f.memberships[key] = struct{}{}
	f.MembershipsAdded++
	return nil
}

// ResolvePath resolves/creates the path parent-first. Same-named siblings
// under one parent are a conflict — never a pick.
func (f *FakeProvider) ResolvePath(_ context.Context, segments []string, createMissing bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := ""
	for _, seg := range segments {
		child, ok := f.colChildren[parent][seg]
		if !ok {
			if !createMissing {
				return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound,
					fmt.Sprintf("collection %q not found under parent (create_missing=false)", seg))
			}
			f.colSeq++
			child = fmt.Sprintf("FAKECOL%d", f.colSeq)
			f.collections[child] = &fakeCollection{providerID: child, name: seg, parentID: parent}
			if f.colChildren[parent] == nil {
				f.colChildren[parent] = map[string]string{}
			}
			f.colChildren[parent][seg] = child
		}
		parent = child
	}
	return parent, nil
}

// ListRecords pages the full catalog snapshot in insertion order.
func (f *FakeProvider) ListRecords(_ context.Context, pageToken string) (CatalogPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CollectionsReads++
	f.PagesServed++
	size := f.PageSize
	if size <= 0 {
		size = 2
	}
	start := 0
	if pageToken != "" {
		if _, err := fmt.Sscanf(pageToken, "%d", &start); err != nil {
			return CatalogPage{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "bad page token")
		}
	}
	end := start + size
	if end > len(f.records) {
		end = len(f.records)
	}
	page := CatalogPage{}
	for _, r := range f.records[start:end] {
		cr := CatalogRecord{
			ProviderRecordID: r.providerID,
			RecordID:         r.providerID,
			RecordType:       r.draft.RecordType,
			Title:            r.draft.Title,
			Authors:          r.draft.Authors,
			Year:             r.draft.Year,
			DOI:              NormalizeDOI(r.draft.DOI),
			ISBN:             NormalizeISBN(r.draft.ISBN),
		}
		for m := range f.memberships { // keyed "recID|colID"
			if strings.HasPrefix(m, r.providerID+"|") {
				cr.Collections = append(cr.Collections, strings.TrimPrefix(m, r.providerID+"|"))
			}
		}
		sort.Strings(cr.Collections)
		for _, h := range r.attOrder {
			att := r.renditions[h]
			cr.Renditions = append(cr.Renditions, CatalogRendition{
				ProviderAttachmentID: att.providerID,
				RenditionID:          att.providerID,
				ContentHash:          att.hash,
				MediaType:            att.mediaType,
				Filename:             att.filename,
			})
		}
		page.Records = append(page.Records, cr)
	}
	if end < len(f.records) {
		page.NextPageToken = fmt.Sprintf("%d", end)
	}
	return page, nil
}

// Snapshot counts for Zähl-Asserts.
func (f *FakeProvider) Snapshot() (records, renditions, memberships, collections int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.records {
		n += len(r.renditions)
	}
	return len(f.records), n, len(f.memberships), len(f.collections)
}

// ---------------------------------------------------------------------------
// FakeResolvers — deterministic ladder fixtures.

// ResolverFixture is one programmed resolver behavior: when Query matches
// (by DOI, ISBN, or title substring), return Candidates.
type ResolverFixture struct {
	MatchDOI   string
	MatchISBN  string
	MatchTitle string // case-insensitive substring
	Candidates []Candidate
}

// FakeResolver is one scripted BibliographicResolver.
type FakeResolver struct {
	name    string
	version string
	fix     []ResolverFixture

	mu    sync.Mutex
	calls []ResolveQuery // the call log (ladder-order witnesses)
}

// NewFakeResolver builds a scripted resolver.
func NewFakeResolver(name, version string, fixtures []ResolverFixture) *FakeResolver {
	return &FakeResolver{name: name, version: version, fix: fixtures}
}

// Name implements BibliographicResolver.
func (f *FakeResolver) Name() string { return f.name }

// Version implements BibliographicResolver.
func (f *FakeResolver) Version() string { return f.version }

// Resolve returns the fixtures whose matcher hits. Exact identifier
// matches (DOI/ISBN) are preferred by the LADDER (ladder.go runs the
// identifier pass first); the resolver itself serves both from one table,
// identifier matches first — DOI vor unscharfer Suche.
func (f *FakeResolver) Resolve(_ context.Context, q ResolveQuery) ([]Candidate, error) {
	f.mu.Lock()
	f.calls = append(f.calls, q)
	f.mu.Unlock()
	var exact, fuzzy []Candidate
	for _, fx := range f.fix {
		hit := false
		if fx.MatchDOI != "" && NormalizeDOI(fx.MatchDOI) == NormalizeDOI(q.DOI) && q.DOI != "" {
			hit = true
		}
		if fx.MatchISBN != "" && NormalizeISBN(fx.MatchISBN) == NormalizeISBN(q.ISBN) && q.ISBN != "" {
			hit = true
		}
		if !hit && fx.MatchTitle != "" && q.Title != "" &&
			strings.Contains(strings.ToLower(q.Title), strings.ToLower(fx.MatchTitle)) {
			hit = true
			fuzzy = append(fuzzy, fx.Candidates...)
			continue
		}
		if hit {
			exact = append(exact, fx.Candidates...)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	sort.SliceStable(fuzzy, func(i, j int) bool { return fuzzy[i].Confidence > fuzzy[j].Confidence })
	return fuzzy, nil
}

// Calls returns the recorded queries (ladder-order witness).
func (f *FakeResolver) Calls() []ResolveQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ResolveQuery(nil), f.calls...)
}

// StandardLadderFixtures — the binding scenarios (DoD): unambiguous DOI
// (direct resolution beats fuzzy search), ambiguous Crossref (two similar
// hits), no-hit, and a type-conflict candidate.
var StandardLadderFixtures = struct {
	UniqueDOI, AmbiguousTitle, NoHitTitle, TypeConflictTitle string
}{
	UniqueDOI:         "10.5555/unique-doi",
	AmbiguousTitle:    "Network Effects",
	NoHitTitle:        "Totally Unknown Work",
	TypeConflictTitle: "Typed Work",
}

// StandardCrossrefFixtures programs the fake Crossref resolver.
func StandardCrossrefFixtures() []ResolverFixture {
	y1, y2 := 2019, 2020
	return []ResolverFixture{
		{
			MatchDOI: StandardLadderFixtures.UniqueDOI,
			Candidates: []Candidate{{
				CandidateID: "crossref-doi-1",
				Confidence:  1.0,
				Fields: ResolvedFields{
					Title: "The Unique DOI Work", Authors: []string{"Ada Example"},
					Year: &y1, Publisher: "Fixture Press", Language: "en",
					RecordType: "book", DOI: StandardLadderFixtures.UniqueDOI,
				},
			}},
		},
		{
			// Two similar fuzzy hits — ambiguous, must surface BOTH.
			MatchTitle: StandardLadderFixtures.AmbiguousTitle,
			Candidates: []Candidate{
				{
					CandidateID: "crossref-amb-1", Confidence: 0.9,
					Fields: ResolvedFields{Title: "Network Effects in Platforms", Authors: []string{"B. First"}, Year: &y1, RecordType: "journalArticle"},
				},
				{
					CandidateID: "crossref-amb-2", Confidence: 0.88,
					Fields: ResolvedFields{Title: "Network Effects: A Survey", Authors: []string{"C. Second"}, Year: &y2, RecordType: "journalArticle"},
				},
			},
		},
		{
			// Type conflict: fuzzy hit of the WRONG record type.
			MatchTitle: StandardLadderFixtures.TypeConflictTitle,
			Candidates: []Candidate{{
				CandidateID: "crossref-type-1", Confidence: 0.95,
				Fields: ResolvedFields{Title: "Typed Work Conference Version", Authors: []string{"D. Third"}, RecordType: "conferencePaper"},
			}},
		},
	}
}

// StandardOpenLibraryFixtures programs the fake Open Library resolver.
func StandardOpenLibraryFixtures() []ResolverFixture {
	y := 2018
	return []ResolverFixture{
		{
			MatchISBN: "9783161484100",
			Candidates: []Candidate{{
				CandidateID: "openlib-isbn-1", Confidence: 1.0,
				Fields: ResolvedFields{
					Title: "The ISBN Work", Authors: []string{"E. Fourth"},
					Year: &y, Publisher: "Open Fixture Press", Language: "de",
					RecordType: "book", ISBN: "9783161484100",
				},
			}},
		},
	}
}

// ---------------------------------------------------------------------------
// FakeDocumentInspector — ladder rung 1. Deterministic derivation: the
// first sentence after the PDF marker is the document title; the language
// is declared in a "%AXIOM-LANG: xx" marker line when present. Fields it
// cannot read stay empty — never guessed.
type FakeDocumentInspector struct{}

// Inspect implements DocumentInspector.
func (FakeDocumentInspector) Inspect(_ context.Context, mediaType, stagingPath string) (ResolvedFields, error) {
	b, err := readFileLimited(stagingPath, 1<<20)
	if err != nil {
		return ResolvedFields{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable, err, "document inspector read")
	}
	s := string(b)
	fields := ResolvedFields{}
	if i := strings.Index(s, "%AXIOM-LANG: "); i >= 0 {
		rest := s[i+len("%AXIOM-LANG: "):]
		if j := strings.IndexAny(rest, "\n\r"); j > 0 {
			fields.Language = strings.TrimSpace(rest[:j])
		}
	}
	// First sentence of the first non-marker line.
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "%AXIOM") || strings.HasPrefix(line, "%PDF") {
			continue
		}
		if j := strings.Index(line, ". "); j > 0 {
			line = line[:j]
		}
		if line != "" {
			fields.Title = strings.TrimSuffix(line, ".")
		}
		break
	}
	return fields, nil
}
