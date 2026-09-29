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
	"fmt"
	"io"
	"log"
	"mime/multipart"
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
	// The control surface sits NEXT to the binding (backend-owned), never
	// ON it — wrappers must not advertise capabilities the backend lacks.
	backend := contractsuite.NewFakeLibrary()
	contractsuite.LibrarySuite(t, faultProxiedLibrary{
		Library:      NewLocalLibraryClient(backend),
		FaultControl: backend,
	})
}

func TestStoreSuiteOverLocalBinding(t *testing.T) {
	backend := contractsuite.NewFakeStore(nil)
	contractsuite.StoreSuite(t, faultProxiedStore{
		Store:            NewLocalStoreClient(backend),
		FaultControl:     backend,
		ContentCorruptor: backend,
	})
}

// TestLocalClientsDoNotAdvertiseAbsentCapabilities — the CI-red lesson,
// pinned: a binding wrapper must not satisfy FaultControl/ContentCorruptor
// when the backing service does not. A vacuous advertisement made the
// suite's fault probes run against a no-op control over the REAL service
// and misclassify its normal answers (local-green/CI-red, four heads).
func TestLocalClientsDoNotAdvertiseAbsentCapabilities(t *testing.T) {
	type plain struct{ library.Library } // implements nothing beyond Library
	var lib library.Library = NewLocalLibraryClient(plain{})
	if _, ok := lib.(contractsuite.FaultControl); ok {
		t.Fatal("LocalLibraryClient advertises FaultControl without a backing capability")
	}
	type plainStore struct{ store.Store }
	var st store.Store = NewLocalStoreClient(plainStore{})
	if _, ok := st.(contractsuite.FaultControl); ok {
		t.Fatal("LocalStoreClient advertises FaultControl without a backing capability")
	}
	if _, ok := st.(contractsuite.ContentCorruptor); ok {
		t.Fatal("LocalStoreClient advertises ContentCorruptor without a backing capability")
	}
}

// ---------------------------------------------------------------------------
// suite parity: HTTP bindings against the internal edge

// faultProxiedLibrary carries the SERVER-side fault book next to the
// client binding (HTTP or local): control stays at the backend — exactly
// how production fault injection works in split.
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

// faultProxiedStore: the store twin — control at the backend, behavior
// through the client binding (HTTP or local).
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

// ---------------------------------------------------------------------------
// review-disposition witnesses (C1/M1/M2 + minors, #305 review round)

// TestLargeFlushedBodySurvivesBudget — C1 regression: the per-request
// budget's cancel must fire at Body.Close, not when the Do call returns.
// A flushed body large enough to outrun the transport's buffering would
// fail mid-read with "context canceled" under the broken lifetime.
func TestLargeFlushedBodySurvivesBudget(t *testing.T) {
	big := store.SearchResult{
		Query: "big", TopN: 64,
		Hits: make([]store.SearchHit, 0, 64),
	}
	pad := strings.Repeat("x", 8<<10) // 8 KiB per hit → ~512 KiB total
	for i := 0; i < 64; i++ {
		big.Hits = append(big.Hits, store.SearchHit{
			ChunkID: fmt.Sprintf("chk-%03d", i), Text: pad, Score: 1,
			Source:  store.Source{Bibliography: contractsuite.SeedBibliography, ContentType: "application/pdf"},
			Locator: store.Locator{Kind: "page", Label: "S. 1"},
			Section: []string{},
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(big)
		// flush in small chunks: nothing is pre-buffered for the client
		for len(raw) > 0 {
			n := 4 << 10
			if n > len(raw) {
				n = len(raw)
			}
			_, _ = w.Write(raw[:n])
			raw = raw[n:]
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Timeout: 5 * time.Second, Logger: quiet()})
	res, err := sc.Search(context.Background(), store.SearchRequest{Query: "big"})
	if err != nil {
		t.Fatalf("large flushed body failed (budget cancel fired before Close?): %v", err)
	}
	if len(res.Hits) != 64 || res.Hits[63].ChunkID != "chk-063" {
		t.Fatalf("body truncated: %d hits, last=%q", len(res.Hits), lastChunk(res))
	}
}

func lastChunk(res store.SearchResult) string {
	if len(res.Hits) == 0 {
		return ""
	}
	return res.Hits[len(res.Hits)-1].ChunkID
}

// TestTruncatedTwoXXBodyIsTypedInternal — M1 sonde: a 2xx answer whose
// body is not decodable JSON surfaces as typed Internal, never a bare
// error out of a contract method.
func TestTruncatedTwoXXBodyIsTypedInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hits":[`)) // truncated JSON, 200 status
	}))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet()})
	_, err := sc.Search(context.Background(), store.SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("truncated body decoded successfully")
	}
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInternal {
		t.Fatalf("truncated 2xx body: class=%v typed=%v, want internal (err: %v)", class, ok, err)
	}
}

// extendedFakeLibrary gives the reference fake the F06 decision surface
// (replay detection + confirm/retry) so the edge's extended routes and
// the client's extended methods are exercised as the real service shape.
type extendedFakeLibrary struct {
	*contractsuite.FakeLibrary
	seen map[string]int // idempotency key → StartImport count
}

func newExtendedFakeLibrary() *extendedFakeLibrary {
	return &extendedFakeLibrary{FakeLibrary: contractsuite.NewFakeLibrary(), seen: map[string]int{}}
}

func (e *extendedFakeLibrary) StartImport(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, error) {
	op, err := e.FakeLibrary.StartImport(ctx, req, content)
	if err == nil {
		e.seen[req.IdempotencyKey]++
	}
	return op, err
}

func (e *extendedFakeLibrary) StartImportDetailed(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, bool, error) {
	op, err := e.StartImport(ctx, req, content)
	return op, e.seen[req.IdempotencyKey] > 1, err
}

func (e *extendedFakeLibrary) ConfirmImport(ctx context.Context, importID, decisionID, candidateID string) (library.ImportOperation, error) {
	op, err := e.FakeLibrary.GetImport(ctx, library.ImportRef{ImportID: importID})
	if err != nil {
		return op, err
	}
	return op, nil
}

func (e *extendedFakeLibrary) RetryImport(ctx context.Context, importID string) (library.ImportOperation, error) {
	return e.FakeLibrary.GetImport(ctx, library.ImportRef{ImportID: importID})
}

// TestExtendedLibrarySurfaceAcrossEdge — the F06 decision surface over
// the edge: replay signal (200 vs 201), confirm, retry — plus the
// capability-honest 404→NotFound for services without the surface.
func TestExtendedLibrarySurfaceAcrossEdge(t *testing.T) {
	backend := newExtendedFakeLibrary()
	srv := httptest.NewServer(LibraryInternalRoutes(backend, nil))
	defer srv.Close()
	c := NewHTTPLibraryClient(Options{BaseURL: srv.URL, Logger: quiet()})
	ctx := context.Background()

	ireq := library.ImportRequest{
		IdempotencyKey: "ext-1", RecordType: "book",
		Target:        library.ImportTarget{LibraryID: "users/0"},
		MetadataHints: library.MetadataHints{Title: contractsuite.SeedBibliography.Title},
	}
	first, replayed, err := c.StartImportDetailed(ctx, ireq, bytes.NewReader(contractsuite.SeedContent))
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("fresh import reported as replay")
	}
	second, replayed, err := c.StartImportDetailed(ctx, ireq, bytes.NewReader(contractsuite.SeedContent))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed {
		t.Fatal("replayed import not detected (same key, same payload)")
	}
	if second.ImportID != first.ImportID {
		t.Fatalf("replay returned %s, want the original %s", second.ImportID, first.ImportID)
	}
	confirmed, err := c.ConfirmImport(ctx, first.ImportID, "dec-1", "cand-1")
	if err != nil || confirmed.ImportID != first.ImportID {
		t.Fatalf("confirm across edge: op=%s err=%v", confirmed.ImportID, err)
	}
	retried, err := c.RetryImport(ctx, first.ImportID)
	if err != nil || retried.ImportID != first.ImportID {
		t.Fatalf("retry across edge: op=%s err=%v", retried.ImportID, err)
	}

	// Capability-honest: the plain fake has no F06 surface — the routes
	// answer 404, the client maps to the contract's NotFound.
	plain := httptest.NewServer(LibraryInternalRoutes(contractsuite.NewFakeLibrary(), nil))
	defer plain.Close()
	pc := NewHTTPLibraryClient(Options{BaseURL: plain.URL, Logger: quiet()})
	if _, err := pc.ConfirmImport(ctx, "imp-void", "dec", "cand"); err == nil {
		t.Fatal("confirm against a surface-less service answered success")
	} else if class, _ := contracterr.ClassOf(err); class != contracterr.ClassNotFound {
		t.Fatalf("confirm against a surface-less service: class=%v, want not_found (err: %v)", class, err)
	}
}

// TestCallerCancelPropagatesUnwrapped — the documented table row: caller
// cancellation propagates as context.Canceled (never wrapped into a
// contract class), and no retry fires after the caller gave up.
func TestCallerCancelPropagatesUnwrapped(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeJSON(w, http.StatusOK, store.SearchResult{})
	}))
	defer srv.Close()
	defer close(release)
	attempts := make(chan struct{}, 4)
	tr := countingTransportWrap(attempts)
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Logger: quiet(), Client: &http.Client{Transport: tr}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := sc.Search(ctx, store.SearchRequest{Query: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancel: err=%v (%T), want context.Canceled unwrapped", err, err)
	}
	if class, ok := contracterr.ClassOf(err); ok {
		t.Fatalf("caller cancel was wrapped into class %s — cancellation is not a contract error", class)
	}
	if n := len(attempts); n > 1 {
		t.Fatalf("retry fired after caller cancellation (%d attempts)", n)
	}
}

// countingTransportWrap records each attempt on a channel.
func countingTransportWrap(attempts chan<- struct{}) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts <- struct{}{}
		return http.DefaultTransport.RoundTrip(req)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestInternalEdgeOversizeMultipartRejected — the edge's multipart bound:
// an over-limit import body answers InvalidArgument, and the bound honors
// the service's MaxImportBytes (the importMaxBounder branch).
func TestInternalEdgeOversizeMultipartRejected(t *testing.T) {
	bounded := boundedFakeLibrary{FakeLibrary: contractsuite.NewFakeLibrary()}
	srv := httptest.NewServer(LibraryInternalRoutes(bounded, nil))
	defer srv.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("request", `{"idempotency_key":"big-1","record_type":"book","target":{"library_id":"users/0"}}`)
	fw, _ := mw.CreateFormFile("file", "big.pdf")
	// The edge bound is the service limit PLUS the request-part headroom
	// (4 MiB, the public route's convention) — the body must exceed both.
	_, _ = fw.Write(bytes.Repeat([]byte("%PDF-1.4 padding"), 340*1024)) // ~4.6 MiB > 1 KiB + 4 MiB
	_ = mw.Close()
	resp, err := http.Post(srv.URL+"/internal/v1/library/imports", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize multipart: status %d, want 400 (body: %s)", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"invalid_argument"`)) {
		t.Fatalf("oversize multipart: body is not the typed InvalidArgument envelope: %s", body)
	}
}

// boundedFakeLibrary advertises a tiny import bound.
type boundedFakeLibrary struct {
	*contractsuite.FakeLibrary
}

func (boundedFakeLibrary) MaxImportBytes() int64 { return 1024 }

// TestAuthHookDeniesStatus403 — pins the documented denial STATUS (the
// class is Internal for the client; the status names the cause).
func TestAuthHookDeniesStatus403(t *testing.T) {
	srv := httptest.NewServer(StoreInternalRoutes(contractsuite.NewFakeStore(nil), nil, denyingAuth{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/internal/v1/store/search")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied auth status = %d, want 403 (the documented denial status)", resp.StatusCode)
	}
}

// TestMidBodyBudgetExpiryClassifiesDeadline — review round 2, Finding 1:
// the budget ctx spans the whole body lifetime (C1 fix); expiring
// MID-BODY must classify Deadline (the mapping table), not Internal —
// the public surface maps Deadline to 504, not 500.
func TestMidBodyBudgetExpiryClassifiesDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"x","top_n":10,"reranked":false,`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // stall mid-body until the budget kills us
	}))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Timeout: 100 * time.Millisecond, Logger: quiet()})
	_, err := sc.Search(context.Background(), store.SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("stalled body answered within the budget")
	}
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassDeadline {
		t.Fatalf("mid-body budget expiry: class=%v typed=%v, want deadline (err: %v)", class, ok, err)
	}
	if contracterr.Retryable(err) {
		t.Fatalf("deadline must not be auto-retryable: %v", err)
	}
}

// TestMidBodyCallerCancelPropagatesUnwrapped — review round 2, Finding 1:
// a caller cancel during the body read propagates as context.Canceled
// (never wrapped into a contract class), exactly like the Do phase.
func TestMidBodyCallerCancelPropagatesUnwrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"x","top_n":10,`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Timeout: 10 * time.Second, Logger: quiet()})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := sc.Search(ctx, store.SearchRequest{Query: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-body caller cancel: err=%v, want context.Canceled", err)
	}
	if _, typed := contracterr.ClassOf(err); typed {
		t.Fatalf("mid-body caller cancel was wrapped into a contract class — cancellation is not a contract error: %v", err)
	}
}

// TestStartImportTransportCeilingRejectsOversize — the 1-GiB ceiling is
// a loud InvalidArgument, never a silent truncation into a wrong-class
// hash conflict (the var is lowered for the probe instead of
// allocating a gigabyte).
func TestStartImportTransportCeilingRejectsOversize(t *testing.T) {
	orig := transportCeiling
	transportCeiling = 1024
	defer func() { transportCeiling = orig }()

	srv := httptest.NewServer(LibraryInternalRoutes(contractsuite.NewFakeLibrary(), nil))
	defer srv.Close()
	c := NewHTTPLibraryClient(Options{BaseURL: srv.URL, Logger: quiet()})
	_, err := c.StartImport(context.Background(),
		library.ImportRequest{IdempotencyKey: "oversize-1", RecordType: "book", Target: library.ImportTarget{LibraryID: "users/0"}},
		bytes.NewReader(bytes.Repeat([]byte("%PDF-1.4 x"), 300)))
	if err == nil {
		t.Fatal("oversize content silently accepted/truncated")
	}
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInvalidArgument {
		t.Fatalf("oversize content: class=%v typed=%v, want invalid_argument (err: %v)", class, ok, err)
	}
}

// TestBaseURLTrailingSlashTolerated — a BaseURL with a trailing slash
// must not double the path (review round minor).
func TestBaseURLTrailingSlashTolerated(t *testing.T) {
	srv := httptest.NewServer(StoreInternalRoutes(contractsuite.NewFakeStore(nil), nil, nil))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL + "/", Logger: quiet()})
	if _, err := sc.Search(context.Background(), store.SearchRequest{Query: contractsuite.SeedToken}); err != nil {
		t.Fatalf("trailing-slash BaseURL: %v", err)
	}
}

// TestMidBodyHardResetIsInternal — the third mid-body outcome, pinned:
// a server that hard-closes mid-JSON (no budget expiry, no cancel)
// classifies Internal — there is no honest other class for it.
func TestMidBodyHardResetIsInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"x","top_n":10,`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // hard close mid-body
	}))
	defer srv.Close()
	sc := NewHTTPStoreClient(Options{BaseURL: srv.URL, Timeout: 5 * time.Second, Logger: quiet()})
	_, err := sc.Search(context.Background(), store.SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("hard-closed body decoded successfully")
	}
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassInternal {
		t.Fatalf("mid-body hard reset: class=%v typed=%v, want internal (err: %v)", class, ok, err)
	}
	if contracterr.Retryable(err) {
		t.Fatalf("mid-body hard reset must not be retryable: %v", err)
	}
}
