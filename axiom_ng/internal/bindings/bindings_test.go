// bindings_test.go — the F11 parity proofs (#305):
//
//   - the F03 contract suites run against BOTH bindings with the same
//     fixtures (local wrappers; HTTP clients against the internal edge
//     over the reference fakes) — no binding-specific skip,
//   - the hash-mismatch Conflict is visible identically per binding,
//   - transport mapping: refused→Unavailable (retryable, leak-free),
//     deadline→Deadline (not auto-retryable), envelope classes, the
//     IdempotencyMismatch reconstruction,
//   - retry fires exactly once on GETs, never on keyed writes,
//   - the AuthN hook denies with the envelope,
//   - the public adapters reproduce the local public JSON byte-for-byte
//     (through the REAL store forward mapping),
//   - the event relay bridges the bus across the edge.
package bindings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contractsuite"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
	axstore "github.com/Cyb3rDudu/axiom/axiom_ng/internal/store"
)

// quiet drops the client's raw-transport log lines (they carry hosts).
func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

// ---------------------------------------------------------------------------
// suite parity: local bindings

func TestLibrarySuiteOverLocalBinding(t *testing.T) {
	contractsuite.LibrarySuite(t, NewLocalLibraryClient(contractsuite.NewFakeLibrary()))
}

func TestStoreSuiteOverLocalBinding(t *testing.T) {
	contractsuite.StoreSuite(t, NewLocalStoreClient(contractsuite.NewFakeStore(nil)))
}

// ---------------------------------------------------------------------------
// suite parity: HTTP bindings against the internal edge

// faultProxiedLibrary carries the SERVER-side fault book next to the
// HTTP client: behavior flows over the edge, control stays at the
// backend — exactly how production fault injection works in split.
type faultProxiedLibrary struct {
	library.Library
	contractsuite.FaultControl
}

func TestLibrarySuiteOverHTTPBinding(t *testing.T) {
	backend := contractsuite.NewFakeLibrary()
	srv := httptest.NewServer(LibraryInternalRoutes(backend, nil))
	defer srv.Close()
	client := NewHTTPLibraryClient(Options{BaseURL: srv.URL, Logger: quiet()})
	contractsuite.LibrarySuite(t, faultProxiedLibrary{Library: client, FaultControl: backend})
}

type faultProxiedStore struct {
	store.Store
	contractsuite.FaultControl
	contractsuite.ContentCorruptor
}

func TestStoreSuiteOverHTTPBinding(t *testing.T) {
	backend := contractsuite.NewFakeStore(nil)
	srv := httptest.NewServer(StoreInternalRoutes(backend, nil, nil))
	defer srv.Close()
	client := NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet()})
	contractsuite.StoreSuite(t, faultProxiedStore{Store: client, FaultControl: backend, ContentCorruptor: backend})
}

// TestHashMismatchConflictParityPerBinding — the F02/F03-registered
// sonde, side by side: the SAME fresh intake under the SAME armed
// corruption knob answers Conflict through the local binding AND
// through the HTTP binding, in class and semantics (never
// IdempotencyMismatch, never retryable).
func TestHashMismatchConflictParityPerBinding(t *testing.T) {
	run := func(name string, impl store.Store, cc contractsuite.ContentCorruptor) contracterr.Class {
		t.Helper()
		cc.CorruptNextContent()
		_, err := impl.IngestRevision(context.Background(), store.IngestRevisionRequest{
			IdempotencyKey: "hashmismatch-" + name, Revision: contractsuite.SeedRevision,
		})
		if err == nil {
			t.Fatalf("%s: armed corruption accepted silently", name)
		}
		class, ok := contracterr.ClassOf(err)
		if !ok {
			t.Fatalf("%s: untyped error %v", name, err)
		}
		var mm *contracterr.IdempotencyMismatch
		if errors.As(err, &mm) {
			t.Fatalf("%s: hash mismatch surfaced as idempotency mismatch: %v", name, err)
		}
		if contracterr.Retryable(err) {
			t.Fatalf("%s: hash mismatch retryable: %v", name, err)
		}
		return class
	}

	localFake := contractsuite.NewFakeStore(nil)
	localClass := run("local", NewLocalStoreClient(localFake), localFake)

	httpFake := contractsuite.NewFakeStore(nil)
	srv := httptest.NewServer(StoreInternalRoutes(httpFake, nil, nil))
	defer srv.Close()
	httpClass := run("http", NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet()}), httpFake)

	if localClass != contracterr.ClassConflict || httpClass != contracterr.ClassConflict {
		t.Fatalf("conflict parity broken: local=%s http=%s, want conflict on both", localClass, httpClass)
	}
}

// ---------------------------------------------------------------------------
// transport mapping

// TestTransportRefusedMapsUnavailableRetryableAndDoesNotLeak — the
// kill-probe shape: a dead backend (connection refused) classifies
// Unavailable (retryable) and the error text carries no host, port, or
// dial detail (the leak sonde's guarantee).
func TestTransportRefusedMapsUnavailableRetryableAndDoesNotLeak(t *testing.T) {
	// A listener that is closed = refused, with a real ephemeral port.
	dead := httptest.NewServer(http.HandlerFunc(http.NotFound))
	deadURL := dead.URL
	dead.Close()

	sc := NewHTTPStoreClient(Options{BaseURL: deadURL, Logger: quiet()})
	_, err := sc.Search(context.Background(), store.SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("refused backend answered success")
	}
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassUnavailable {
		t.Fatalf("refused: class=%v typed=%v, want unavailable (err: %v)", class, ok, err)
	}
	if !contracterr.Retryable(err) {
		t.Fatalf("refused must classify retryable (kill-probe parity with local crash injection): %v", err)
	}
	for _, leak := range []string{"127.0.0.1", "localhost", "dial", "http://", "connect:"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("transport error leaks topology detail %q: %s", leak, err.Error())
		}
	}

	lc := NewHTTPLibraryClient(Options{BaseURL: deadURL, Logger: quiet()})
	_, lerr := lc.GetSource(context.Background(), library.SourceRef{SourceID: "s"})
	if class, _ := contracterr.ClassOf(lerr); class != contracterr.ClassUnavailable {
		t.Fatalf("library refused: class=%v, want unavailable (err: %v)", class, lerr)
	}
}

// TestTransportDeadlineClassifiedNotAutoRetryable — an expired request
// budget classifies Deadline and is NOT retryable (the retry-class
// red/green witness; same-deadline retry storms live here).
func TestTransportDeadlineClassifiedNotAutoRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		writeJSON(w, http.StatusOK, store.SearchResult{})
	}))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Timeout: 50 * time.Millisecond, Logger: quiet()})
	_, err := sc.Search(context.Background(), store.SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("slow backend answered within the budget")
	}
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassDeadline {
		t.Fatalf("budget expiry: class=%v typed=%v, want deadline (err: %v)", class, ok, err)
	}
	if contracterr.Retryable(err) {
		t.Fatalf("deadline must not be auto-retryable: %v", err)
	}
}

// TestEnvelopeClassesRoundTrip — every envelope class the edge can send
// reconstructs into the same contract class client-side (typed wire,
// no status guessing beyond the fallback table).
func TestEnvelopeClassesRoundTrip(t *testing.T) {
	cases := []struct {
		class contracterr.Class
	}{
		{contracterr.ClassNotFound},
		{contracterr.ClassInvalidArgument},
		{contracterr.ClassConflict},
		{contracterr.ClassUnavailable},
		{contracterr.ClassDeadline},
		{contracterr.ClassInternal},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, contracterr.ComponentStore, contracterr.New(contracterr.ComponentStore, c.class, "probe "+string(c.class)))
		}))
		sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Timeout: 2 * time.Second, Logger: quiet()})
		_, err := sc.GetPassage(context.Background(), store.PassageRef{ChunkID: "chk"})
		srv.Close()
		if err == nil {
			t.Fatalf("%s: envelope answered success", c.class)
		}
		got, ok := contracterr.ClassOf(err)
		if !ok || got != c.class {
			t.Fatalf("envelope %s: got class %v (typed=%v), err=%v", c.class, got, ok, err)
		}
		if got == contracterr.ClassUnavailable && !contracterr.Retryable(err) {
			t.Fatalf("envelope unavailable not retryable: %v", err)
		}
	}
}

// TestIdempotencyMismatchReconstruction — a conflict envelope carrying
// the key reconstructs as *contracterr.IdempotencyMismatch with that
// key (the suite's diverged-replay probe runs this through the HTTP
// binding end to end; this pins the wire detail directly).
func TestIdempotencyMismatchReconstruction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"component":"store","class":"conflict","message":"probe","idempotency_key":"key-7"}}`))
	}))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet()})
	_, err := sc.IngestRevision(context.Background(), store.IngestRevisionRequest{IdempotencyKey: "key-7"})
	if err == nil {
		t.Fatal("conflict envelope answered success")
	}
	var mm *contracterr.IdempotencyMismatch
	if !errors.As(err, &mm) || mm.Key != "key-7" {
		t.Fatalf("envelope conflict did not reconstruct as *IdempotencyMismatch{key-7}: %v", err)
	}
}

// ---------------------------------------------------------------------------
// retry policy

// countingTransport witnesses per-URL attempts.
type countingTransport struct {
	mu       sync.Mutex
	attempts map[string]int
	fail     map[string]int // path → first-N transport failures
	stub     http.RoundTripper
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.attempts[req.URL.Path]++
	n := c.attempts[req.URL.Path]
	failN := c.fail[req.URL.Path]
	c.mu.Unlock()
	if n <= failN {
		return nil, errors.New("connection refused (probe)")
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (c *countingTransport) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[path]
}

// TestReadRetryFiresOnceOnTransportFailure — a GET riding out one
// transport blip succeeds on the second attempt (the retry red/green
// witness: killing the retry logic turns this red).
func TestReadRetryFiresOnceOnTransportFailure(t *testing.T) {
	tr := &countingTransport{attempts: map[string]int{}, fail: map[string]int{"/internal/v1/store/passages/chk-1": 1}}
	srv := httptest.NewServer(StoreInternalRoutes(contractsuite.NewFakeStore(nil), nil, nil))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet(), Client: &http.Client{Transport: tr}})
	if _, err := sc.GetPassage(context.Background(), store.PassageRef{ChunkID: "chk-void"}); err == nil {
		t.Fatal("unknown passage answered success")
	}
	if got := tr.count("/internal/v1/store/passages/chk-void"); got != 1 {
		t.Fatalf("unknown-passage GET retried (%d attempts) — only transport failures retry", got)
	}
	// A resolvable passage behind a one-shot transport failure: the
	// retry must deliver it.
	backend := contractsuite.NewFakeStore(nil)
	if _, err := backend.IngestRevision(context.Background(), store.IngestRevisionRequest{
		IdempotencyKey: "retry-probe", Revision: contractsuite.SeedRevision,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := backend.Search(context.Background(), store.SearchRequest{Query: contractsuite.SeedToken})
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("seed search: %v hits=%d", err, len(res.Hits))
	}
	srv2 := httptest.NewServer(StoreInternalRoutes(backend, nil, nil))
	defer srv2.Close()
	tr2 := &countingTransport{attempts: map[string]int{}, fail: map[string]int{"/internal/v1/store/passages/" + res.Hits[0].ChunkID: 1}}
	sc2 := NewHTTPStoreClient(Options{BaseURL: srv2.URL, Logger: quiet(), Client: &http.Client{Transport: tr2}})
	if _, err := sc2.GetPassage(context.Background(), store.PassageRef{ChunkID: res.Hits[0].ChunkID}); err != nil {
		t.Fatalf("retryable GET did not ride out the blip: %v", err)
	}
	if got := tr2.count("/internal/v1/store/passages/" + res.Hits[0].ChunkID); got != 2 {
		t.Fatalf("attempts=%d, want exactly 2 (one retry)", got)
	}
}

// TestKeyedWritesFlyExactlyOnce — the keyed writes never retry, even on
// transport failure (a hidden second flight is the silent-divergence
// risk the contract forbids).
func TestKeyedWritesFlyExactlyOnce(t *testing.T) {
	tr := &countingTransport{attempts: map[string]int{}, fail: map[string]int{"/internal/v1/store/ingest": 1}}
	srv := httptest.NewServer(StoreInternalRoutes(contractsuite.NewFakeStore(nil), nil, nil))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet(), Client: &http.Client{Transport: tr}})
	if _, err := sc.IngestRevision(context.Background(), store.IngestRevisionRequest{IdempotencyKey: "once", Revision: contractsuite.SeedRevision}); err == nil {
		t.Fatal("transport-failed write answered success")
	}
	if got := tr.count("/internal/v1/store/ingest"); got != 1 {
		t.Fatalf("ingest attempted %d times, want exactly 1", got)
	}
}

// ---------------------------------------------------------------------------
// AuthN hook

type denyingAuth struct{}

func (denyingAuth) Authenticate(*http.Request) error { return errors.New("no") }

func TestAuthHookDenies(t *testing.T) {
	srv := httptest.NewServer(StoreInternalRoutes(contractsuite.NewFakeStore(nil), nil, denyingAuth{}))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet()})
	_, err := sc.Search(context.Background(), store.SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("denying authenticator let the call through")
	}
	if class, _ := contracterr.ClassOf(err); class != contracterr.ClassInternal {
		t.Fatalf("denied call: class=%v, want internal (err: %v)", class, err)
	}
}

// ---------------------------------------------------------------------------
// public adapter byte-identity

// fakeBackend returns a fixed, field-rich response for the byte-identity
// proof (every optional field exercised).
type fakeBackend struct {
	resp    *search.Response
	passage *search.Passage
}

func (f fakeBackend) Search(ctx context.Context, req search.Request) (*search.Response, error) {
	return f.resp, nil
}

func (f fakeBackend) GetPassage(ctx context.Context, chunkID string) (*search.Passage, error) {
	if f.passage != nil && f.passage.ChunkID == chunkID {
		return f.passage, nil
	}
	return nil, search.ErrPassageNotFound
}

// TestPublicSearchPassageJSONByteIdentity — the strangler witness: the
// public JSON served through the HTTP binding (store forward mapping →
// wire → reverse mapping) is byte-identical to the local public JSON
// (the search stack type serialized directly).
func TestPublicSearchPassageJSONByteIdentity(t *testing.T) {
	year := 2019
	chapter, pages := 3, 47
	rich := &search.Response{
		Query: "byte identity", TopN: 10, Reranked: true,
		Arms:   search.Arms{Dense: true, BM25: true, Sparse: true},
		TookMS: 12,
		Hits: []search.Hit{{
			ChunkID: "chk-1", Text: "the text", Score: 1.5,
			Source: richSource(year),
			Locator: search.LocatorView{
				Kind: "page", Label: "S. 47", Chapter: "Kapitel Eins", ChapterNumber: &chapter,
				PageSource: "folio_verified", PageStart: &pages, PageEnd: &pages,
				ParagraphInChapter: &chapter, SectionTitle: "§1",
				ParagraphPages: [][]string{{"0", "47"}, {"120", "48"}},
			},
			Section:                 []string{"A", "B"},
			CaptionText:             "[machine image caption: x] [document figure caption: y]",
			CollapsedNearDuplicates: 2,
			Images: []search.ImageView{{Ref: "image-0001", Marker: "image_0.jpg",
				MachineCaption: "machine", FigureCaption: "figure"}},
		}},
	}
	passage := &search.Passage{
		ChunkID: "chk-1", DocumentID: "doc-1", SnapshotID: "snap-1", AttachmentID: "att-1",
		ChunkIndex: 4, Text: "the text", Section: []string{"A"},
		Locator: search.LocatorView{Kind: "epub_cfi", Label: "Kap. 3", CFI: "/6/4!/4/10"},
		Source:  richSource(year),
		Neighbors: []search.PassageNeighbor{{
			ChunkID: "chk-0", ChunkIndex: 3, Text: "prev", Section: []string{},
			Locator: search.LocatorView{Kind: "epub_cfi", Label: "Kap. 3"},
		}},
		ParagraphPages: [][]string{{"0", "47"}},
		CaptionText:    "capt",
		Images:         []search.ImageView{{Ref: "image-0002", Marker: "image_1.jpg"}},
	}

	// The REAL store service applies the forward mapping (what the store
	// process serves); the HTTP binding + reverse mapping rebuild the
	// public shape.
	backend := fakeBackend{resp: rich, passage: passage}
	svc := axstore.New(nil, backend, quiet())
	srv := httptest.NewServer(StoreInternalRoutes(svc, nil, nil))
	defer srv.Close()
	client := NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet()})

	localSearch, err := json.Marshal(rich)
	if err != nil {
		t.Fatal(err)
	}
	viaBinding, err := json.Marshal(searchResponseFromDTO(mustSearch(t, client)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(localSearch, viaBinding) {
		t.Fatalf("search JSON diverged:\nlocal: %s\nhttp:  %s", localSearch, viaBinding)
	}

	localPassage, err := json.Marshal(passage)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := client.GetPassage(context.Background(), store.PassageRef{ChunkID: "chk-1"})
	if err != nil {
		t.Fatal(err)
	}
	viaBindingP, err := json.Marshal(passageFromDTO(p2))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(localPassage, viaBindingP) {
		t.Fatalf("passage JSON diverged:\nlocal: %s\nhttp:  %s", localPassage, viaBindingP)
	}
}

func mustSearch(t *testing.T, c *HTTPStoreClient) store.SearchResult {
	t.Helper()
	res, err := c.Search(context.Background(), store.SearchRequest{Query: "byte identity"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func richSource(year int) (s repo.SourceView) {
	return repo.SourceView{
		DocID: "doc-1", Title: "T", Authors: []string{"A", "B"}, Year: &year,
		Publisher: "P", Language: "de", Tags: []string{"x"},
		ContentType: "application/pdf", CitationClass: "citable",
	}
}

// ---------------------------------------------------------------------------
// event relay

func TestEventRelayBridgesAndSkipsDerived(t *testing.T) {
	source := events.NewBroker()
	srv := httptest.NewServer(StoreInternalRoutes(contractsuite.NewFakeStore(nil), source, nil))
	defer srv.Close()

	target := events.NewBroker()
	got := make(chan events.Event, 16)
	sub := events.NewSubscription().WithMatch(func(e events.Event) bool { return true })
	target.Subscribe(sub, 0)
	consumerDone := make(chan struct{})
	go func() {
		for {
			ev, _, ok := sub.Next(consumerDone)
			if !ok {
				return
			}
			got <- ev
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go BridgeEvents(ctx, srv.URL, target, &http.Client{}, quiet())

	// SSE has no replay: an event published before the bridge subscribed
	// is lost (the same best-effort semantics the WS live stream has; the
	// durable truth is the DB). The probe republishes round by round until
	// both source events are observed.
	deadline := time.Now().Add(5 * time.Second)
	var seenClaimed, seenDrained bool
	for time.Now().Before(deadline) && !(seenClaimed && seenDrained) {
		source.Publish(events.JobClaimed{JobID: "j-1", RunnerName: "r-1"})
		source.Publish(events.RunnerStateChanged{RunnerName: "r-1", State: "busy"}) // must be skipped
		source.Publish(events.OutboxDrained{Count: 3})
		select {
		case ev := <-got:
			switch e := ev.(type) {
			case events.JobClaimed:
				seenClaimed = seenClaimed || e.JobID == "j-1"
			case events.OutboxDrained:
				seenDrained = seenDrained || e.Count == 3
			case events.RunnerStateChanged:
				t.Fatalf("derived frame relayed: %v", ev)
			default:
				t.Fatalf("unexpected event: %v", ev)
			}
		case <-time.After(150 * time.Millisecond):
		}
	}
	if !seenClaimed || !seenDrained {
		t.Fatal("relay did not deliver both source events in time")
	}
	// Quiet window: only duplicates of the republished pair may still
	// arrive (overlapping rounds in flight); anything else — especially a
	// derived RunnerStateChanged — is a relay bug.
	quiet := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(quiet) {
		select {
		case ev := <-got:
			switch e := ev.(type) {
			case events.JobClaimed:
				if e.JobID != "j-1" {
					t.Fatalf("unexpected event: %v", ev)
				}
			case events.OutboxDrained:
				if e.Count != 3 {
					t.Fatalf("unexpected event: %v", ev)
				}
			case events.RunnerStateChanged:
				t.Fatalf("derived frame relayed: %v", ev)
			default:
				t.Fatalf("unexpected event: %v", ev)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	close(consumerDone)
}
