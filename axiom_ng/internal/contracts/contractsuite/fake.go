// fake.go — the in-memory reference implementations of the F03
// contracts (#297). They prove the suites green today and become the
// test partners of F06/F09 (extraction) and F11 (adapter parity).
//
// Deterministic by design: sequential ids from a fixed base time so
// golden-style debugging never races. FaultControl is implemented by
// both fakes — the suites' classification probes always run against
// them.
package contractsuite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
)

// fakeClock: fixed base + per-event microsecond steps (UTC, RFC3339 µs —
// the DM03-compatible form the DTOs freeze).
var fakeBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// faultBook is the shared fault-injection state.
type faultBook struct {
	mu     sync.Mutex
	faults map[string]error
}

func (f *faultBook) InjectFault(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.faults == nil {
		f.faults = map[string]error{}
	}
	f.faults[method] = err
}

func (f *faultBook) ClearFaults() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = nil
}

// trip returns the injected fault for method, mapped the way real seams
// map: contracterr values pass through, foreign errors surface as
// Internal (never leak).
func (f *faultBook) trip(component contracterr.Component, method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.faults[method]
	if err == nil {
		return nil
	}
	var ce *contracterr.Error
	if errors.As(err, &ce) {
		return err // pre-typed contract errors pass through unmapped
	}
	var im *contracterr.IdempotencyMismatch
	if errors.As(err, &im) {
		return err
	}
	return contracterr.Wrap(component, contracterr.ClassInternal, err, "injected fault surfaced")
}

// ---------------------------------------------------------------------------
// FakeLibrary

// FakeLibrary is the reference library.Library: synchronous happy-path
// imports (straight to committed), idempotency by request+content hash,
// tickets for stored rendition bytes.
type FakeLibrary struct {
	mu        sync.Mutex
	faultBook faultBook
	seq       int
	sources   map[string]library.Source
	records   map[string]revision.Bibliography // record id → normalized record
	imports   map[string]library.ImportOperation
	byKey     map[string]string // idempotency key → import id
	keyHash   map[string]string // idempotency key → payload hash
	tickets   map[string][]byte // content ticket → rendition bytes
}

// NewFakeLibrary returns an empty fake.
func NewFakeLibrary() *FakeLibrary {
	return &FakeLibrary{
		sources: map[string]library.Source{},
		records: map[string]revision.Bibliography{},
		imports: map[string]library.ImportOperation{},
		byKey:   map[string]string{},
		keyHash: map[string]string{},
		tickets: map[string][]byte{},
	}
}

// InjectFault/ClearFaults satisfy FaultControl.
func (f *FakeLibrary) InjectFault(method string, err error) { f.faultBook.InjectFault(method, err) }
func (f *FakeLibrary) ClearFaults()                         { f.faultBook.ClearFaults() }

func (f *FakeLibrary) next() (n int, at time.Time) {
	f.seq++
	return f.seq, fakeBase.Add(time.Duration(f.seq) * time.Microsecond)
}

func (f *FakeLibrary) GetSource(ctx context.Context, ref library.SourceRef) (library.Source, error) {
	if err := f.faultBook.trip(contracterr.ComponentLibrary, "GetSource"); err != nil {
		return library.Source{}, err
	}
	if ref.SourceID == "" {
		return library.Source{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source ref is blank")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	src, ok := f.sources[ref.SourceID]
	if !ok {
		return library.Source{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "source "+ref.SourceID)
	}
	return src, nil
}

func (f *FakeLibrary) StartImport(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, error) {
	if err := f.faultBook.trip(contracterr.ComponentLibrary, "StartImport"); err != nil {
		return library.ImportOperation{}, err
	}
	if req.IdempotencyKey == "" {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "idempotency key is required")
	}
	if req.RecordType == "" {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "record_type is required")
	}
	if req.Target.CollectionID != "" && len(req.Target.CollectionPath) > 0 {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "collection_id and collection_path are mutually exclusive")
	}
	if req.RecordType == "webpage" && (req.Source == nil || req.Source.OriginalURL == "") {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "webpage imports require source.original_url")
	}
	b, err := io.ReadAll(content)
	if err != nil {
		return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, err, "reading import content")
	}
	if len(b) == 0 {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "import content is empty")
	}
	// Format derives from magic bytes ONLY — the F06 intake rule — and
	// BEFORE any state is written: a rejected import leaves no trace (no
	// orphaned source/record/ticket, no consumed sequence number).
	media, err := mediaTypeFromMagic(b)
	if err != nil {
		return library.ImportOperation{}, err
	}
	// Payload identity = canonical JSON of the FULL request DTO + the
	// content bytes — the documented "metadata JSON and content"
	// (FakeStore hashes the canonical JSON of the revision the same
	// way). Any difference — content, hints, target, enrichment —
	// diverges the key.
	meta, err := json.Marshal(req)
	if err != nil {
		return library.ImportOperation{}, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "canonicalizing import request")
	}
	payload := revision.HashContent(append(append([]byte{}, meta...), b...))

	f.mu.Lock()
	defer f.mu.Unlock()
	if priorID, seen := f.byKey[req.IdempotencyKey]; seen {
		if f.keyHash[req.IdempotencyKey] != payload {
			return library.ImportOperation{}, &contracterr.IdempotencyMismatch{Component: contracterr.ComponentLibrary, Key: req.IdempotencyKey}
		}
		return f.imports[priorID], nil // replay: same operation, no side effects
	}

	n, at := f.next()
	importID := fmt.Sprintf("imp-%d", n)
	recordID := fmt.Sprintf("rec-%d", n)
	sourceID := fmt.Sprintf("src-%d", n)
	renditionID := fmt.Sprintf("ren-%d", n)
	ticket := fmt.Sprintf("ticket-%d", n)

	f.sources[sourceID] = library.Source{SourceID: sourceID, Provider: "fixture", LibraryID: req.Target.LibraryID, SyncedAt: &at}
	bib := SeedBibliography
	bib.RecordID = recordID
	if req.MetadataHints.Title != "" {
		bib.Title = req.MetadataHints.Title
	}
	f.records[recordID] = bib
	f.tickets[ticket] = append([]byte{}, b...)

	rev := revision.SourceRevision{
		SourceID:            sourceID,
		RevisionID:          fmt.Sprintf("%d", n),
		ContentHash:         revision.HashContent(b),
		MediaType:           media,
		Bibliography:        bib,
		LocatorCapabilities: revision.LocatorCapabilities{Page: &revision.PageCapability{Trust: revision.TrustFolioVerified}},
		ContentTicket:       ticket,
	}
	op := library.ImportOperation{
		ImportID:  importID,
		Status:    library.ImportCommitted,
		Result:    &library.ImportResult{RecordID: recordID, RenditionID: renditionID, Revision: rev},
		UpdatedAt: at,
	}
	f.imports[importID] = op
	f.byKey[req.IdempotencyKey] = importID
	f.keyHash[req.IdempotencyKey] = payload
	return op, nil
}

func (f *FakeLibrary) GetImport(ctx context.Context, ref library.ImportRef) (library.ImportOperation, error) {
	if err := f.faultBook.trip(contracterr.ComponentLibrary, "GetImport"); err != nil {
		return library.ImportOperation{}, err
	}
	if ref.ImportID == "" {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "import ref is blank")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	op, ok := f.imports[ref.ImportID]
	if !ok {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "import "+ref.ImportID)
	}
	return op, nil
}

func (f *FakeLibrary) OpenRendition(ctx context.Context, ticket library.ContentTicket) (io.ReadCloser, error) {
	if err := f.faultBook.trip(contracterr.ComponentLibrary, "OpenRendition"); err != nil {
		return nil, err
	}
	if ticket == "" {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "content ticket is blank")
	}
	f.mu.Lock()
	b, ok := f.tickets[string(ticket)]
	f.mu.Unlock()
	if !ok {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "content ticket unknown or expired")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *FakeLibrary) ProjectCitation(ctx context.Context, req library.CitationRequest) (library.CitationProjection, error) {
	if err := f.faultBook.trip(contracterr.ComponentLibrary, "ProjectCitation"); err != nil {
		return library.CitationProjection{}, err
	}
	if req.RecordID == "" {
		return library.CitationProjection{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "record id is blank")
	}
	switch req.Locator.Kind {
	case "page", "epub_cfi":
	default:
		return library.CitationProjection{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "locator kind "+req.Locator.Kind+" is not citable")
	}
	f.mu.Lock()
	bib, ok := f.records[req.RecordID]
	f.mu.Unlock()
	if !ok {
		return library.CitationProjection{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "record "+req.RecordID)
	}
	year := "n.d."
	if bib.Year != nil {
		year = fmt.Sprintf("%d", *bib.Year)
	}
	author := "Unknown"
	if len(bib.Authors) > 0 {
		author = bib.Authors[0]
		surname := author
		if sp := strings.IndexAny(author, " "); sp > 0 {
			surname = author[sp+1:]
		}
		author = surname
	}
	locus := ""
	if req.Locator.Kind == "page" && req.Locator.PageStart != nil {
		locus = fmt.Sprintf(", S. %d", *req.Locator.PageStart)
	}
	style := req.Style
	if style == "" {
		style = DefaultCitationStyle
	}
	return library.CitationProjection{
		RecordID:  req.RecordID,
		Citation:  fmt.Sprintf("(%s, %s%s)", author, year, locus),
		Reference: fmt.Sprintf("%s (%s). %s. %s.", author, year, bib.Title, bib.Publisher),
		Style:     style,
		Locator:   req.Locator,
	}, nil
}

// ---------------------------------------------------------------------------
// FakeStore

// fakeChunk is one indexed chunk of the fake.
type fakeChunk struct {
	chunkID  string
	docID    string
	renID    string
	snapID   string
	index    int
	text     string
	source   store.Source
	sections []string
}

// FakeStore is the reference store.Store: synchronous commit, paragraph
// chunking, substring retrieval. Content arrives through the resolver
// the constructor receives — exactly how F09's real store pulls via the
// Library seam (in-process or HTTP), never a byte smuggled in the
// request.
type FakeStore struct {
	mu         sync.Mutex
	faultBook  faultBook
	resolve    func(ticket string) ([]byte, error)
	seq        int
	jobs       map[string]store.IngestJob
	byKey      map[string]string // idempotency key → job id
	keyPayload map[string]string
	chunks     []fakeChunk
}

// NewFakeStore returns a fake whose intake resolves content tickets with
// resolve (nil: tickets resolve to SeedContent).
func NewFakeStore(resolve func(ticket string) ([]byte, error)) *FakeStore {
	if resolve == nil {
		resolve = func(string) ([]byte, error) { return SeedContent, nil }
	}
	return &FakeStore{
		resolve:    resolve,
		jobs:       map[string]store.IngestJob{},
		byKey:      map[string]string{},
		keyPayload: map[string]string{},
	}
}

// InjectFault/ClearFaults satisfy FaultControl.
func (f *FakeStore) InjectFault(method string, err error) { f.faultBook.InjectFault(method, err) }
func (f *FakeStore) ClearFaults()                         { f.faultBook.ClearFaults() }

func (f *FakeStore) next() time.Time {
	f.seq++
	return fakeBase.Add(time.Duration(f.seq) * time.Microsecond)
}

func (f *FakeStore) IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error) {
	if err := f.faultBook.trip(contracterr.ComponentStore, "IngestRevision"); err != nil {
		return store.IngestJob{}, err
	}
	if req.IdempotencyKey == "" {
		return store.IngestJob{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "idempotency key is required")
	}
	if err := req.Revision.Validate(); err != nil {
		return store.IngestJob{}, err
	}
	// Payload identity = canonical JSON of the revision DTO.
	payload, err := json.Marshal(req.Revision)
	if err != nil {
		return store.IngestJob{}, contracterr.Wrap(contracterr.ComponentStore, contracterr.ClassInternal, err, "canonicalizing revision")
	}

	// First idempotency pass: cheap map lookups under the lock; replays
	// never reach the ticket resolver.
	f.mu.Lock()
	if priorID, seen := f.byKey[req.IdempotencyKey]; seen {
		defer f.mu.Unlock()
		if f.keyPayload[req.IdempotencyKey] != string(payload) {
			return store.IngestJob{}, &contracterr.IdempotencyMismatch{Component: contracterr.ComponentStore, Key: req.IdempotencyKey}
		}
		return f.jobs[priorID], nil // replay
	}
	f.mu.Unlock()

	// Resolve the ticket WITHOUT holding the lock — the resolver is an
	// external callback (the Library binding); calling it under the
	// mutex would serialize every intake behind the slowest fetch.
	content, err := f.resolve(req.Revision.ContentTicket)
	if err != nil {
		return store.IngestJob{}, contracterr.Wrap(contracterr.ComponentStore, contracterr.ClassUnavailable, err, "resolving content ticket")
	}
	if revision.HashContent(content) != req.Revision.ContentHash {
		return store.IngestJob{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassConflict, "content does not match the revision hash")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Second pass: a concurrent intake with the same key may have won
	// the race while this one was resolving — replay its job instead of
	// double-indexing.
	if priorID, seen := f.byKey[req.IdempotencyKey]; seen {
		if f.keyPayload[req.IdempotencyKey] != string(payload) {
			return store.IngestJob{}, &contracterr.IdempotencyMismatch{Component: contracterr.ComponentStore, Key: req.IdempotencyKey}
		}
		return f.jobs[priorID], nil
	}

	jobID := fmt.Sprintf("job-%d", f.seq+1)
	snapID := fmt.Sprintf("snap-%d", f.seq+1)
	at := f.next()
	job := store.IngestJob{
		JobID:       jobID,
		Status:      store.IngestCommitted,
		RevisionID:  req.Revision.RevisionID,
		ContentHash: req.Revision.ContentHash,
		Attempt:     1,
		MaxAttempts: 3,
		UpdatedAt:   at,
	}
	f.jobs[jobID] = job
	f.byKey[req.IdempotencyKey] = jobID
	f.keyPayload[req.IdempotencyKey] = string(payload)

	// Chunk by blank line; index verbatim (the suite's parity invariants
	// hold over whatever chunking the implementation chooses).
	src := store.Source{Bibliography: req.Revision.Bibliography, ContentType: req.Revision.MediaType}
	for i, para := range strings.Split(string(content), "\n\n") {
		f.seq++
		f.chunks = append(f.chunks, fakeChunk{
			chunkID:  fmt.Sprintf("chk-%s-%d", jobID, i),
			docID:    req.Revision.Bibliography.RecordID,
			renID:    "ren-" + req.Revision.RevisionID,
			snapID:   snapID,
			index:    i,
			text:     para,
			source:   src,
			sections: []string{},
		})
	}
	return job, nil
}

func (f *FakeStore) Search(ctx context.Context, req store.SearchRequest) (store.SearchResult, error) {
	if err := f.faultBook.trip(contracterr.ComponentStore, "Search"); err != nil {
		return store.SearchResult{}, err
	}
	if strings.TrimSpace(req.Query) == "" {
		return store.SearchResult{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "query must not be blank")
	}
	if req.TopN > store.MaxTopN {
		return store.SearchResult{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, fmt.Sprintf("top_n must be <= %d", store.MaxTopN))
	}
	if req.TopN <= 0 {
		req.TopN = 10
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	hits := []store.SearchHit{} // non-nil: the frozen wire shape is an array
	for _, c := range f.chunks {
		if req.Filters != nil && len(req.Filters.DocumentIDs) > 0 && !contains(req.Filters.DocumentIDs, c.docID) {
			continue
		}
		if !strings.Contains(c.text, req.Query) {
			continue
		}
		hits = append(hits, store.SearchHit{
			ChunkID: c.chunkID,
			Text:    c.text,
			Score:   1.0,
			Source:  c.source,
			Locator: pageLocator(),
			Section: c.sections,
		})
		if len(hits) == req.TopN {
			break
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return store.SearchResult{
		Query:    req.Query,
		TopN:     req.TopN,
		Reranked: false,
		Arms:     store.SearchArms{Dense: true, BM25: true},
		Hits:     hits,
		TookMS:   1,
	}, nil
}

func (f *FakeStore) GetPassage(ctx context.Context, ref store.PassageRef) (store.Passage, error) {
	if err := f.faultBook.trip(contracterr.ComponentStore, "GetPassage"); err != nil {
		return store.Passage{}, err
	}
	if ref.ChunkID == "" {
		return store.Passage{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "passage ref is blank")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.chunks {
		if c.chunkID != ref.ChunkID {
			continue
		}
		neighbors := []store.PassageNeighbor{}
		for _, d := range f.chunks {
			if d.docID == c.docID && d.snapID == c.snapID && (d.index == c.index-1 || d.index == c.index+1) {
				neighbors = append(neighbors, store.PassageNeighbor{
					ChunkID:    d.chunkID,
					ChunkIndex: d.index,
					Text:       d.text,
					Section:    d.sections,
					Locator:    pageLocator(),
				})
			}
		}
		return store.Passage{
			ChunkID:     c.chunkID,
			DocumentID:  c.docID,
			SnapshotID:  c.snapID,
			RenditionID: c.renID,
			ChunkIndex:  c.index,
			Text:        c.text,
			Section:     c.sections,
			Locator:     pageLocator(),
			Source:      c.source,
			Neighbors:   neighbors,
		}, nil
	}
	return store.Passage{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassNotFound, "passage "+ref.ChunkID)
}

// pageLocator is the fake's uniform page locator (neighbors carry it
// too — zero-value locators would diverge from real passage delivery).
func pageLocator() store.Locator {
	return store.Locator{
		Kind:       "page",
		Label:      "S. 47",
		PageSource: revision.TrustFolioVerified,
		PageStart:  intPtr(47),
		PageEnd:    intPtr(47),
	}
}

// mediaTypeFromMagic derives the rendition format from magic bytes
// ONLY — the F06 intake rule (declared extensions and client MIME
// types are never sufficient). Suite fixtures are PDF-shaped for this
// reason; direct fake users feeding foreign bytes get the same
// InvalidArgument a real intake reports.
func mediaTypeFromMagic(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte("%PDF-")):
		return revision.MediaTypePDF, nil
	case bytes.HasPrefix(b, []byte("PK\x03\x04")):
		return revision.MediaTypeEPUB, nil
	}
	return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "content magic bytes match neither PDF nor EPUB")
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func intPtr(i int) *int { return &i }
