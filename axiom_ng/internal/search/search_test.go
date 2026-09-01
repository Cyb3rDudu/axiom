package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// --- fakes -----------------------------------------------------------------

type fakeProcessor struct {
	embedErr       error
	embedVec       []float32
	embedSparse    map[string]float64
	embedSparseErr error // fails only the combined sparse call (dense fallback path)
	rerankErr      error
	rerankRes      []processor.RerankScore
	lastRerank     *RerankCapture
	embedCalls     int // any embed path (dense or dense+sparse)
}

// RerankCapture records what the pipeline sent to the reranker.
type RerankCapture struct {
	Query string
	Texts []string
	TopN  int
}

func (f *fakeProcessor) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	f.embedCalls++
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = f.embedVec
	}
	return out, nil
}

func (f *fakeProcessor) EmbedQueriesSparse(ctx context.Context, texts []string) ([][]float32, []map[string]float64, error) {
	f.embedCalls++
	if f.embedErr != nil {
		return nil, nil, f.embedErr
	}
	if f.embedSparseErr != nil {
		return nil, nil, f.embedSparseErr
	}
	out := make([][]float32, len(texts))
	sp := make([]map[string]float64, len(texts))
	for i := range out {
		out[i] = f.embedVec
		sp[i] = f.embedSparse
	}
	return out, sp, nil
}

func (f *fakeProcessor) Rerank(ctx context.Context, query string, texts []string, topN int) ([]processor.RerankScore, error) {
	f.lastRerank = &RerankCapture{Query: query, Texts: texts, TopN: topN}
	if f.rerankErr != nil {
		return nil, f.rerankErr
	}
	return f.rerankRes, nil
}

type fakeDocs struct {
	meta map[string]repo.DocumentMeta
	err  error
}

func (f fakeDocs) DocumentMetaByIDs(ctx context.Context, ids []string) (map[string]repo.DocumentMeta, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.meta, nil
}

// osServer is a scripted OpenSearch stub: it answers _search by inspecting
// the query body (knn vs match) so both arms can return distinct lists.
// chunkFixture is a passage-shaped indexed chunk for the stub.
type chunkFixture struct {
	ChunkID      string          `json:"chunk_id"`
	DocumentID   string          `json:"document_id"`
	SnapshotID   string          `json:"snapshot_id"`
	AttachmentID string          `json:"attachment_id"`
	ChunkIndex   int             `json:"chunk_index"`
	Text         string          `json:"text"`
	Sections     []string        `json:"section_titles"`
	Locator      json.RawMessage `json:"locator"`
}

type osServer struct {
	*httptest.Server
	docChunks      map[string]chunkFixture
	knnHits        []osHit
	bm25Hits       []osHit
	sparseHits     []osHit
	failKnn        bool
	failBM25       bool
	failSparse     bool
	lastKnnBody    map[string]any
	lastBM25Body   map[string]any
	lastSparseBody map[string]any
}

func newOSServer(t *testing.T) *osServer {
	t.Helper()
	s := &osServer{docChunks: map[string]chunkFixture{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A1 #165 passage surfaces: _doc/{id}, _mget, and the neighbor
		// _search (identified by its attachment_id filter).
		if strings.Contains(r.URL.Path, "/_doc/") {
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if fx, ok := s.docChunks[id]; ok {
				_ = json.NewEncoder(w).Encode(map[string]any{"found": true, "_source": fx})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"found": false})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/_mget") {
			var body struct {
				IDs  []string `json:"ids"`
				Docs []struct {
					ID string `json:"_id"`
				} `json:"docs"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			ids := body.IDs
			for _, d := range body.Docs {
				ids = append(ids, d.ID)
			}
			docs := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				if fx, ok := s.docChunks[id]; ok {
					docs = append(docs, map[string]any{"found": true, "_source": fx})
				} else {
					docs = append(docs, map[string]any{"found": false})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"docs": docs})
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/_search") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if rawQ, _ := json.Marshal(body["query"]); strings.Contains(string(rawQ), "attachment_id") {
			// neighbor query: attachment term filter + chunk_index range filter,
			// modeled with OS semantics — size truncates BEFORE the caller can
			// filter, and must_not excludes its term — so window bugs in the
			// query can't hide behind stub leniency again (auto-review C1).
			var att string
			lo, hi := -1, -1
			size := 10 // OpenSearch default
			var excluded string
			if v, ok := body["size"].(float64); ok {
				size = int(v)
			}
			b, _ := body["query"].(map[string]any)["bool"].(map[string]any)
			if filters, ok := b["filter"].([]any); ok {
				for _, f := range filters {
					fm, _ := f.(map[string]any)
					if term, ok := fm["term"].(map[string]any); ok {
						switch inner := term["attachment_id.keyword"].(type) {
						case string: // canonical shorthand {"term": {"field": "value"}}
							att = inner
						case map[string]any: // explicit {"term": {"field": {"term": "value"}}}
							att, _ = inner["term"].(string)
						}
					}
					if rng, ok := fm["range"].(map[string]any); ok {
						if idx, ok := rng["chunk_index"].(map[string]any); ok {
							if v, ok := idx["gte"].(float64); ok {
								lo = int(v)
							}
							if v, ok := idx["lte"].(float64); ok {
								hi = int(v)
							}
						}
					}
				}
			}
			if mustNots, ok := b["must_not"].([]any); ok {
				for _, m := range mustNots {
					fm, _ := m.(map[string]any)
					if term, ok := fm["term"].(map[string]any); ok {
						switch inner := term["chunk_id.keyword"].(type) {
						case string:
							excluded = inner
						case map[string]any:
							excluded, _ = inner["term"].(string)
						}
					}
				}
			}
			fxs := make([]chunkFixture, 0, 2)
			for _, fx := range s.docChunks {
				if fx.AttachmentID == att && fx.ChunkIndex >= lo && fx.ChunkIndex <= hi && fx.ChunkID != excluded {
					fxs = append(fxs, fx)
				}
			}
			sort.Slice(fxs, func(i, j int) bool { return fxs[i].ChunkIndex < fxs[j].ChunkIndex })
			if len(fxs) > size {
				fxs = fxs[:size]
			}
			hits := make([]map[string]any, 0, len(fxs))
			for _, fx := range fxs {
				hits = append(hits, map[string]any{"_id": fx.ChunkID, "_source": fx})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": hits}})
			return
		}
		q, _ := body["query"].(map[string]any)
		raw, _ := json.Marshal(q)
		isKnn := strings.Contains(string(raw), `"knn"`)
		isSparse := strings.Contains(string(raw), `"rank_feature"`)
		var hits []osHit
		if isSparse {
			s.lastSparseBody = body
			if s.failSparse {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			hits = s.sparseHits
		} else if isKnn {
			s.lastKnnBody = body
			if s.failKnn {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			hits = s.knnHits
		} else {
			s.lastBM25Body = body
			if s.failBM25 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			hits = s.bm25Hits
		}
		out := map[string]any{"hits": map[string]any{"hits": hitsList(hits)}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(s.Close)
	return s
}

func hitsList(hits []osHit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"_id":     h.ID,
			"_source": h,
		})
	}
	return out
}

func hit(id, doc, text string) osHit {
	return osHit{ID: id, DocumentID: doc, Text: text}
}

func newService(osURL string, p Processor, d DocSource) *Service {
	svc := New(osURL, "", "", p, d, log.New(io.Discard, "", 0))
	// Capability tests run the full arm set; the production default for
	// SparseArm is benchmark-driven OFF (R7) — tests that verify the
	// off-switch override this explicitly.
	svc.SparseArm = true
	return svc
}

// --- RRF -------------------------------------------------------------------

func TestRRFMerge_BothArmsBeatSingleArm(t *testing.T) {
	knn := []osHit{hit("a", "d1", "alpha"), hit("b", "d1", "beta"), hit("c", "d2", "gamma")}
	bm25 := []osHit{hit("b", "d1", "beta"), hit("x", "d3", "delta"), hit("a", "d1", "alpha")}
	merged := rrfMerge([][]osHit{knn, bm25}, 10)
	if len(merged) != 4 {
		t.Fatalf("want 4 merged candidates (union), got %d", len(merged))
	}
	// "a" (knn rank1+bm25 rank3) and "b" (knn rank2+bm25 rank1) must outrank
	// any single-arm hit.
	if merged[0].ID != "a" && merged[0].ID != "b" {
		t.Fatalf("top candidate must come from both arms, got %q", merged[0].ID)
	}
	if merged[1].ID != "a" && merged[1].ID != "b" {
		t.Fatalf("second candidate must come from both arms, got %q", merged[1].ID)
	}
	// Exact RRF math, pinned literally (k=60 is the spec value):
	// a = 1/(60+1) + 1/(60+3) — knn rank 1, bm25 rank 3.
	wantA := 1.0/61.0 + 1.0/63.0
	for _, c := range merged {
		if c.ID == "a" && c.RRFScore != wantA {
			t.Fatalf("rrf(a) = %v, want %v", c.RRFScore, wantA)
		}
	}
}

func TestRRFMerge_DeterministicTieOrder(t *testing.T) {
	// Ties keep insertion order (dense arm first) — pinned so a sort change
	// cannot silently reshuffle equal candidates.
	knn := []osHit{hit("a", "d", "x"), hit("b", "d", "y")}
	bm25 := []osHit{hit("b", "d", "y"), hit("a", "d", "x")}
	m1 := rrfMerge([][]osHit{knn, bm25}, 10)
	m2 := rrfMerge([][]osHit{knn, bm25}, 10)
	for i := range m1 {
		if m1[i].ID != m2[i].ID {
			t.Fatalf("nondeterministic merge at %d: %q vs %q", i, m1[i].ID, m2[i].ID)
		}
	}
	if m1[0].ID != "a" {
		t.Fatalf("tie must keep dense-first order, got %q", m1[0].ID)
	}
}

// --- pipeline --------------------------------------------------------------

func TestSearch_HybridUsesBothArms(t *testing.T) {
	// The hybrid mutation target: if the pipeline only queried one arm, the
	// exclusive hit of the other arm would vanish from the result.
	os := newOSServer(t)
	os.knnHits = []osHit{hit("dense-only", "d1", "dense text")}
	os.bm25Hits = []osHit{hit("bm25-only", "d2", "bm25 text")}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{0.1}},
		fakeDocs{meta: map[string]repo.DocumentMeta{}})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Arms.Dense || !res.Arms.BM25 {
		t.Fatalf("both arms must contribute, got %+v", res.Arms)
	}
	ids := map[string]bool{}
	for _, h := range res.Hits {
		ids[h.ChunkID] = true
	}
	if !ids["dense-only"] || !ids["bm25-only"] {
		t.Fatalf("hybrid result must contain exclusive hits of BOTH arms, got %v", ids)
	}
}

func TestSearch_OverfetchAndTopN(t *testing.T) {
	os := newOSServer(t)
	// distinct text + document per hit: this test pins overfetch/TopN
	// mechanics; #160 hygiene (collapse/diversity) has its own tests and
	// would legitimately fold identical-text same-doc fixtures to one.
	for i := 0; i < 20; i++ {
		os.knnHits = append(os.knnHits, hit(fmt.Sprintf("k%d", i), fmt.Sprintf("d%d", i), fmt.Sprintf("text k%d distinct", i)))
		os.bm25Hits = append(os.bm25Hits, hit(fmt.Sprintf("b%d", i), fmt.Sprintf("e%d", i), fmt.Sprintf("text b%d distinct", i)))
	}
	fp := &fakeProcessor{embedVec: []float32{0.1}}
	fp.rerankRes = make([]processor.RerankScore, 30) // 3x top_n = 30 candidates
	for i := range fp.rerankRes {
		fp.rerankRes[i] = processor.RerankScore{Index: i, Score: float64(len(fp.rerankRes) - i)}
	}
	svc := newService(os.URL, fp, fakeDocs{})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 10 {
		t.Fatalf("want 10 hits, got %d", len(res.Hits))
	}
	if fp.lastRerank == nil || len(fp.lastRerank.Texts) != 30 {
		t.Fatalf("rerank must see 3x top_n candidates, got %d", len(fp.lastRerank.Texts))
	}
	if !res.Reranked {
		t.Fatal("valid rerank scores must set reranked=true")
	}
	if res.Hits[0].ChunkID != "k0" {
		t.Fatalf("rerank order (descending by index here) broken: %q", res.Hits[0].ChunkID)
	}
}

func TestSearch_RerankFallbackServesRRFOrder(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("a", "d1", "x"), hit("b", "d1", "y")}
	os.bm25Hits = []osHit{hit("c", "d2", "z")}
	fp := &fakeProcessor{embedVec: []float32{0.1}, rerankErr: errors.New("runner down")}
	svc := newService(os.URL, fp, fakeDocs{})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reranked {
		t.Fatal("failed rerank must be reported as reranked=false")
	}
	if len(res.Hits) != 3 {
		t.Fatalf("fallback must still serve hits, got %d", len(res.Hits))
	}
	// RRF order: "a" is dense rank 1 only; "c" bm25 rank 1 only — both score
	// 1/61; "b" 1/62. Ties keep dense-first: a, c, b.
	if res.Hits[0].ChunkID != "a" || res.Hits[2].ChunkID != "b" {
		t.Fatalf("fallback must preserve RRF order, got %s", orderOf(res))
	}
}

func TestSearch_RerankShapeGuardFallsBack(t *testing.T) {
	// Malformed rerank responses (wrong count, duplicate index, index out
	// of range) must fall back to RRF order instead of trusting garbage.
	cases := []struct {
		name   string
		scores []processor.RerankScore
	}{
		{"wrong count", []processor.RerankScore{{Index: 0, Score: 1}}},
		{"duplicate index", []processor.RerankScore{{Index: 0, Score: 1}, {Index: 0, Score: 0.5}}},
		{"index out of range", []processor.RerankScore{{Index: 0, Score: 1}, {Index: 99, Score: 0.5}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os := newOSServer(t)
			os.knnHits = []osHit{hit("a", "d", "x")}
			os.bm25Hits = []osHit{hit("b", "d", "y")}
			fp := &fakeProcessor{embedVec: []float32{0.1}, rerankRes: tc.scores}
			svc := newService(os.URL, fp, fakeDocs{})
			res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2})
			if err != nil {
				t.Fatal(err)
			}
			if res.Reranked {
				t.Fatalf("%s: shape-invalid scores must fall back to RRF", tc.name)
			}
			// RRF tie (both rank 1) keeps dense-first order.
			if orderOf(res) != "a,b" {
				t.Fatalf("%s: fallback must serve RRF order, got %s", tc.name, orderOf(res))
			}
		})
	}
}

func TestSearch_RerankTiesKeepRRFOrder(t *testing.T) {
	// Equal rerank scores are a valid ranking but must not reshuffle the
	// candidates: ties keep RRF order (stable sort). The b/c tie under a
	// later-starting higher score (d=2.0) is the case an unstable
	// selection sort inverts.
	os := newOSServer(t)
	os.knnHits = []osHit{hit("a", "d", "x"), hit("b", "d", "y"), hit("c", "d", "z"), hit("d", "d", "w")}
	fp := &fakeProcessor{embedVec: []float32{1}}
	fp.rerankRes = []processor.RerankScore{
		{Index: 0, Score: 1.0}, // a
		{Index: 1, Score: 0.5}, // b (tied with c)
		{Index: 2, Score: 0.5}, // c
		{Index: 3, Score: 2.0}, // d
	}
	svc := newService(os.URL, fp, fakeDocs{})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reranked {
		t.Fatal("valid scores must set reranked=true")
	}
	if orderOf(res) != "d,a,b,c" {
		t.Fatalf("ties must keep RRF order (want d,a,b,c), got %s", orderOf(res))
	}
}

func TestSearch_EmbedFailureDegradesToBM25(t *testing.T) {
	os := newOSServer(t)
	os.bm25Hits = []osHit{hit("b", "d", "y")}
	fp := &fakeProcessor{embedErr: errors.New("runner down")}
	svc := newService(os.URL, fp, fakeDocs{})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Arms.Dense {
		t.Fatal("failed embed must clear the dense arm")
	}
	if !res.Arms.BM25 || len(res.Hits) != 1 {
		t.Fatalf("bm25-only degradation broken: %+v", res)
	}
}

func TestSearch_AllArmsDownIsError(t *testing.T) {
	os := newOSServer(t)
	os.failKnn, os.failBM25 = true, true
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}}, fakeDocs{})
	if _, err := svc.Search(context.Background(), Request{Query: "q"}); err == nil {
		t.Fatal("no recall arm must be an error, not an empty 200")
	}
}

func TestSearch_Guards(t *testing.T) {
	// The service is healthy (both arms answer) so the guard assertions
	// discriminate: without the guards these requests would succeed.
	os := newOSServer(t)
	os.knnHits = []osHit{hit("a", "d1", "x")}
	os.bm25Hits = []osHit{hit("b", "d1", "y")}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}}, fakeDocs{})
	if _, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2}); err != nil {
		t.Fatalf("valid request must succeed (control): %v", err)
	}
	var ebr ErrBadRequest
	if _, err := svc.Search(context.Background(), Request{Query: "  "}); !errors.As(err, &ebr) {
		t.Fatalf("blank query must be ErrBadRequest, got %v", err)
	}
	if _, err := svc.Search(context.Background(), Request{Query: "q", TopN: maxCandidates + 1}); !errors.As(err, &ebr) {
		t.Fatalf("top_n above the rerank cap must be ErrBadRequest, got %v", err)
	}
}

func TestSearch_FiltersPassedToBothArms(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("a", "d1", "x")}
	os.bm25Hits = []osHit{hit("b", "d1", "y")}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}}, fakeDocs{})
	_, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2,
		Filters: &Filters{DocumentIDs: []string{"d1"}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]map[string]any{"knn": os.lastKnnBody, "bm25": os.lastBM25Body} {
		if body == nil {
			t.Fatalf("%s arm never queried", name)
		}
		raw, _ := json.Marshal(body)
		// The filter MUST target the keyword subfield: the mapped
		// document_id is a text field whose analyzer splits UUIDs at
		// hyphens — a terms clause on the plain field matches nothing.
		const want = `"terms":{"document_id.keyword":["d1"]}`
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s arm query missing exact document filter %s: %s", name, want, raw)
		}
	}
}

func TestSearch_SourceHydration(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("a", "doc-1", "x")}
	os.bm25Hits = []osHit{hit("b", "doc-2", "y")}
	year := 2020
	docs := fakeDocs{meta: map[string]repo.DocumentMeta{
		"doc-1": {Title: "CSR Handbuch", Authors: []string{"Rene Schmidpeter"}, Year: &year, Publisher: "Springer"},
	}}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}}, docs)
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, h := range res.Hits {
		if h.ChunkID == "a" {
			found = true
			if h.Source.Title != "CSR Handbuch" || h.Source.Authors[0] != "Rene Schmidpeter" || *h.Source.Year != 2020 {
				t.Fatalf("source hydration broken: %+v", h.Source)
			}
		}
	}
	if !found {
		t.Fatal("hit a missing")
	}
}

func TestSearch_SourceHydrationErrorDegrades(t *testing.T) {
	// DB down during hydration must degrade the source block, not the hits.
	os := newOSServer(t)
	os.knnHits = []osHit{hit("a", "doc-1", "x")}
	os.bm25Hits = []osHit{hit("b", "doc-2", "y")}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}},
		fakeDocs{err: errors.New("db down")})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("hits must survive hydration failure, got %d", len(res.Hits))
	}
	for _, h := range res.Hits {
		if h.Source.Title != "" {
			t.Fatalf("no metadata expected on hydration failure: %+v", h.Source)
		}
		if h.Source.DocID == "" {
			t.Fatal("document provenance must survive hydration failure")
		}
	}
}

func orderOf(res *Response) string {
	ids := make([]string, len(res.Hits))
	for i, h := range res.Hits {
		ids[i] = h.ChunkID
	}
	return strings.Join(ids, ",")
}

// --- locators --------------------------------------------------------------

func TestLocatorViewPageSpan(t *testing.T) {
	v := locatorView(json.RawMessage(`{"type":"page_span","page_label_start":"47","page_label_end":"47"}`),
		[]string{"Kapitel 3", "3.1 Grundlagen"})
	if v.Kind != "page" || v.Label != "3.1 Grundlagen · S. 47" || v.Chapter != "3.1 Grundlagen" {
		t.Fatalf("page_span label wrong: %+v", v)
	}
}

func TestLocatorViewPhysicalFallbackAndRange(t *testing.T) {
	p := 46
	v := locatorView(json.RawMessage(fmt.Sprintf(`{"type":"page_span","physical_page_start":%d,"page_label_end":"48"}`, p)), nil)
	if v.Label != "S. 47-48" {
		t.Fatalf("physical fallback/range wrong: %+v", v)
	}
}

func TestLocatorViewPageSpanWithoutPageInfo(t *testing.T) {
	// Neither page_label nor physical page: the label degrades to the bare
	// chapter, never a dangling "S. ".
	v := locatorView(json.RawMessage(`{"type":"page_span"}`), []string{"Kapitel 3"})
	if v.Kind != "page" || v.Label != "Kapitel 3" {
		t.Fatalf("page_span without page info wrong: %+v", v)
	}
}

func TestLocatorViewEpubCFI(t *testing.T) {
	v := locatorView(json.RawMessage(`{"type":"epub_cfi","cfi_start":"epubcfi(/6/4!/4/10,/1:0)"}`),
		[]string{"Kapitel 2"})
	if v.Kind != "epub_cfi" || v.Chapter != "Kapitel 2" || v.CFI != "/6/4!/4/10" {
		t.Fatalf("epub_cfi view wrong: %+v", v)
	}
	if v.Label != "Kapitel 2" {
		t.Fatalf("epub label should be the chapter: %+v", v)
	}
}

// W4: chapter-relative pagination (folios restart per chapter — World Bank
// report style). The chapter ordinal disambiguates "S. 5" as page 5 OF THAT
// CHAPTER per APA7 (German citation form "Kap. 3, S. 5"; clients compose the
// English "ch. 3, p. 5" from chapter_number + the page part).
func TestLocatorViewChapterRelative(t *testing.T) {
	v := locatorView(json.RawMessage(`{"type":"page_span","page_label_start":"5","page_label_end":"7","page_source":"folio_verified","chapter":3}`),
		[]string{"Handlungsempfehlungen", "3.2 Umsetzung"})
	if v.Kind != "page" || v.Label != "Kap. 3, S. 5-7" {
		t.Fatalf("chapter-relative range wrong: %+v", v)
	}
	if v.ChapterNumber == nil || *v.ChapterNumber != 3 {
		t.Fatalf("chapter_number must expose the ordinal: %+v", v)
	}
	if v.PageSource != "folio_verified" || v.Chapter != "3.2 Umsetzung" {
		t.Fatalf("chapter-relative fields wrong: %+v", v)
	}
}

func TestLocatorViewChapterRelativeSingleAndPhysical(t *testing.T) {
	v := locatorView(json.RawMessage(`{"type":"page_span","page_label_start":"5","page_source":"folio_verified","chapter":12}`),
		[]string{"Chapter Twelve: Outlook"})
	if v.Label != "Kap. 12, S. 5" {
		t.Fatalf("chapter-relative single page wrong: %+v", v)
	}
	// physical_only keeps its PDF-index semantics under a chapter ordinal.
	p := 11
	v = locatorView(json.RawMessage(fmt.Sprintf(`{"type":"page_span","physical_page_start":%d,"page_source":"physical_only","chapter":2}`, p)),
		[]string{"Methods"})
	if v.Label != "Kap. 2, PDF-S. 12" {
		t.Fatalf("chapter-relative physical_only wrong: %+v", v)
	}
}

func TestLocatorViewChapterOrdinalWithoutPage(t *testing.T) {
	// Ordinal but no page info at all: "Kap. 3", not the section title —
	// the ordinal is the citation anchor when no page exists.
	v := locatorView(json.RawMessage(`{"type":"page_span","chapter":3}`), []string{"Kapitel 3"})
	if v.Label != "Kap. 3" || v.ChapterNumber == nil || *v.ChapterNumber != 3 {
		t.Fatalf("chapter ordinal without page wrong: %+v", v)
	}
}

func TestCfiShort(t *testing.T) {
	if got := cfiShort("epubcfi(/6/10!/4/28)"); got != "/6/10!/4/28" {
		t.Fatalf("cfiShort plain: %q", got)
	}
	if got := cfiShort("epubcfi(/6/4!/4/10,/1:0)"); got != "/6/4!/4/10" {
		t.Fatalf("cfiShort range: %q", got)
	}
}

func TestTruncateChars(t *testing.T) {
	if got := truncateChars("abcdef", 3); got != "abc" {
		t.Fatalf("truncate: %q", got)
	}
	if got := truncateChars("ab", 3); got != "ab" {
		t.Fatalf("truncate noop: %q", got)
	}
	// Rune-safe: cutting at n bytes inside a multibyte char backs off to
	// the last full rune instead of emitting broken UTF-8.
	if got := truncateChars("aäbc", 2); got != "a" {
		t.Fatalf("truncate must never split a rune: %q", got)
	}
}

// ---------------------------------------------------------------------------
// 7. R5 (#135): the sparse rank_features arm
// ---------------------------------------------------------------------------

func TestSearch_SparseArmContributesAndWinsRRF(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	os.bm25Hits = []osHit{hit("b", "doc", "bm25")}
	os.sparseHits = []osHit{hit("d", "doc", "dense"), hit("s", "doc", "sparse-only")}
	// Zero/negative query weights must never reach the OS query — only w>0
	// tokens become rank_feature clauses.
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1},
		embedSparse: map[string]float64{"12": 0.5, "7": 0.2, "neg": -0.3, "zero": 0}}, fakeDocs{})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Arms.Sparse {
		t.Fatalf("sparse arm must contribute: %+v", res.Arms)
	}
	// The query weights reached the OS as bool-should rank_feature queries.
	if os.lastSparseBody == nil {
		t.Fatal("sparse arm never queried OS")
	}
	raw, _ := json.Marshal(os.lastSparseBody)
	if !strings.Contains(string(raw), `"sparse.12"`) || strings.Count(string(raw), "rank_feature") != 2 {
		t.Fatalf("sparse query shape wrong: %s", raw)
	}
	if strings.Contains(string(raw), "sparse.neg") || strings.Contains(string(raw), "sparse.zero") {
		t.Fatalf("non-positive weights must not reach the query: %s", raw)
	}
	// "d" appears in dense+sparse (2 arms) and must outrank the 1-arm hits.
	if res.Hits[0].ChunkID != "d" {
		t.Fatalf("2-arm candidate must win, got %q", res.Hits[0].ChunkID)
	}
	ids := map[string]bool{}
	for _, h := range res.Hits {
		ids[h.ChunkID] = true
	}
	if !ids["s"] {
		t.Fatal("sparse-only hit must surface through the third arm")
	}
}

func TestSearch_SparseArmTopKAndFilter(t *testing.T) {
	os := newOSServer(t)
	os.sparseHits = []osHit{hit("s", "doc", "sparse")}
	w := map[string]float64{}
	for i := 0; i < 100; i++ {
		w[fmt.Sprintf("t%d", i)] = float64(i) // weight == rank order
	}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}, embedSparse: w}, fakeDocs{})
	if _, err := svc.Search(context.Background(), Request{Query: "q", TopN: 1,
		Filters: &Filters{DocumentIDs: []string{"doc"}}}); err != nil {
		t.Fatal(err)
	}
	if os.lastSparseBody == nil {
		t.Fatal("sparse never queried")
	}
	raw, _ := json.Marshal(os.lastSparseBody["query"])
	if strings.Count(string(raw), "rank_feature") != sparseTopK {
		t.Fatalf("want exactly %d rank_feature clauses, got %s", sparseTopK, raw)
	}
	if !strings.Contains(string(raw), `"sparse.t99"`) || !strings.Contains(string(raw), `"sparse.t36"`) || strings.Contains(string(raw), `"sparse.t35"`) {
		t.Fatalf("top-K selection by weight wrong (want sparse.t99..sparse.t36): %s", raw)
	}
	if !strings.Contains(string(raw), `"terms":{"document_id.keyword":["doc"]}`) {
		t.Fatalf("document filter must wrap the sparse arm via the keyword subfield: %s", raw)
	}
}

func TestSearch_SparseArmQueryFailureDegrades(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	os.bm25Hits = []osHit{hit("b", "doc", "bm25")}
	os.failSparse = true
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1},
		embedSparse: map[string]float64{"1": 1}}, fakeDocs{})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Arms.Sparse {
		t.Fatal("failed sparse query must clear the sparse arm")
	}
	if !res.Arms.Dense || !res.Arms.BM25 || len(res.Hits) != 2 {
		t.Fatalf("2-arm hybrid must survive: %+v", res)
	}
}

func TestSearch_SparseEmbedFailureFallsBackToDenseOnly(t *testing.T) {
	// Combined call fails, plain dense embed still works: 2-arm hybrid.
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	os.bm25Hits = []osHit{hit("b", "doc", "bm25")}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1},
		embedSparseErr: errors.New("runner old, no include_sparse")}, fakeDocs{})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Arms.Sparse || !res.Arms.Dense || !res.Arms.BM25 {
		t.Fatalf("dense-only fallback broken: %+v", res.Arms)
	}
}

func TestSearch_SparseArmOffSwitch(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	os.sparseHits = []osHit{hit("s", "doc", "sparse")}
	fp := &fakeProcessor{embedVec: []float32{1}, embedSparse: map[string]float64{"1": 1}}
	svc := newService(os.URL, fp, fakeDocs{})
	svc.SparseArm = false
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Arms.Sparse {
		t.Fatal("disabled sparse arm must not report")
	}
	if os.lastSparseBody != nil {
		t.Fatal("disabled sparse arm must not query OS")
	}
	ids := map[string]bool{}
	for _, h := range res.Hits {
		ids[h.ChunkID] = true
	}
	if ids["s"] {
		t.Fatal("sparse-only hit must not surface when the arm is off")
	}
}

// The DenseArm/BM25Arm/Rerank off-switches mirror the sparse one: each pins
// all three faces of its honor-point — no downstream call, honest Arms/
// Reranked flag, and results still served by the remaining arm(s).
func TestSearch_DenseArmOff(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	os.bm25Hits = []osHit{hit("b", "doc", "bm25")}
	fp := &fakeProcessor{embedVec: []float32{1}, embedSparse: map[string]float64{"1": 1}}
	svc := newService(os.URL, fp, fakeDocs{})
	svc.DenseArm = false
	// SparseArm stays true (newService default): it rides the dense embed
	// call, so a disabled DenseArm must leave it inert — no embed at all.
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2})
	if err != nil {
		t.Fatal(err)
	}
	if fp.embedCalls != 0 {
		t.Fatalf("disabled dense arm must not embed, got %d embed calls", fp.embedCalls)
	}
	if res.Arms.Dense {
		t.Fatal("disabled dense arm must not report")
	}
	if os.lastKnnBody != nil {
		t.Fatal("disabled dense arm must not query OS (kNN)")
	}
	if os.lastSparseBody != nil {
		t.Fatal("sparse arm must be inert when DenseArm is off")
	}
	ids := map[string]bool{}
	for _, h := range res.Hits {
		ids[h.ChunkID] = true
	}
	if !ids["b"] {
		t.Fatal("bm25 must still serve results with the dense arm off")
	}
	if ids["d"] {
		t.Fatal("dense-only hit must not surface when the arm is off")
	}
}

func TestSearch_BM25ArmOff(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	os.bm25Hits = []osHit{hit("b", "doc", "bm25")}
	fp := &fakeProcessor{embedVec: []float32{1}}
	svc := newService(os.URL, fp, fakeDocs{})
	svc.BM25Arm = false
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Arms.BM25 {
		t.Fatal("disabled bm25 arm must not report")
	}
	if os.lastBM25Body != nil {
		t.Fatal("disabled bm25 arm must not query OS")
	}
	ids := map[string]bool{}
	for _, h := range res.Hits {
		ids[h.ChunkID] = true
	}
	if !ids["d"] {
		t.Fatal("dense must still serve results with the bm25 arm off")
	}
	if ids["b"] {
		t.Fatal("bm25-only hit must not surface when the arm is off")
	}
}

func TestSearch_RerankOffSwitch(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("a", "d1", "x"), hit("b", "d1", "y")}
	os.bm25Hits = []osHit{hit("c", "d2", "z")}
	// rerankRes would flip the order if rerank ran — proves it cannot have.
	fp := &fakeProcessor{embedVec: []float32{0.1},
		rerankRes: []processor.RerankScore{{Index: 2, Score: 9}}}
	svc := newService(os.URL, fp, fakeDocs{})
	svc.Rerank = false
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 3})
	if err != nil {
		t.Fatal(err)
	}
	if fp.lastRerank != nil {
		t.Fatal("disabled rerank must not invoke the runner")
	}
	if res.Reranked {
		t.Fatal("disabled rerank must be reported as reranked=false")
	}
	// RRF order (same as the fallback test): "a" dense rank 1, "c" bm25
	// rank 1 (both 1/61, dense-first), "b" 1/62.
	if res.Hits[0].ChunkID != "a" || res.Hits[2].ChunkID != "b" {
		t.Fatalf("rerank-off must serve RRF order, got %s", orderOf(res))
	}
}

// ---------------------------------------------------------------------------
// 8. R6 (#136): the graph expansion arm (behind GraphArm, default off)
// ---------------------------------------------------------------------------

type fakeGraph struct {
	cands []repo.KGChunkCandidate
	err   error
	seeds []string
	minM  int
}

func (f *fakeGraph) GraphCandidates(ctx context.Context, seedChunkIDs []string, minMentions, limit int) ([]repo.KGChunkCandidate, error) {
	f.seeds, f.minM = seedChunkIDs, minMentions
	if f.err != nil {
		return nil, f.err
	}
	return f.cands, nil
}

func kgCand(id, doc, text string) repo.KGChunkCandidate {
	return repo.KGChunkCandidate{ChunkID: id, DocumentID: doc, Text: text,
		Locator: map[string]any{"type": "page_span", "page_label_start": "9"}, EntityLinks: 3}
}

func TestSearch_GraphArmOffByDefault(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	fg := &fakeGraph{}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}}, fakeDocs{})
	svc.SetGraphSource(fg)
	// GraphArm deliberately NOT set (default false).
	if _, err := svc.Search(context.Background(), Request{Query: "q", TopN: 1}); err != nil {
		t.Fatal(err)
	}
	if fg.seeds != nil {
		t.Fatal("graph must not expand when the arm is off")
	}
}

func TestSearch_GraphArmExpandsCandidates(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	// "d" in TWO hybrid arms so it strictly outranks any graph-only
	// candidate — the RRF pin below must hold by score, not tie order.
	os.bm25Hits = []osHit{hit("d", "doc", "bm25"), hit("b", "doc", "bm25")}
	fg := &fakeGraph{cands: []repo.KGChunkCandidate{kgCand("g", "doc", "graph neighbor")}}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}}, fakeDocs{})
	svc.GraphArm = true
	svc.SetGraphSource(fg)
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Seeds are the hybrid top hits (up to top_n of them, RRF-ordered).
	if len(fg.seeds) != 2 || fg.seeds[0] != "d" {
		t.Fatalf("seeds must be the RRF-ordered hybrid hits, got %v", fg.seeds)
	}
	if fg.minM != graphMinMentions {
		t.Fatalf("stability floor must apply, got %d", fg.minM)
	}
	ids := map[string]bool{}
	for _, h := range res.Hits {
		ids[h.ChunkID] = true
	}
	if !ids["g"] {
		t.Fatalf("graph candidate must surface in the pool, hits: %v", ids)
	}
	// RRF pin: the 2-arm hybrid top hit stays rank 1; a graph-ONLY
	// candidate (1 arm) must never outrank it — graph expansion joins the
	// pool, it does not take it over.
	if res.Hits[0].ChunkID != "d" {
		t.Fatalf("hybrid top hit must remain rank 1 over the graph-only candidate, got %q", res.Hits[0].ChunkID)
	}
	// The graph candidate carries its locator into the hit view.
	for _, h := range res.Hits {
		if h.ChunkID == "g" && h.Locator.Kind != "page" {
			t.Fatalf("graph hit locator lost: %+v", h.Locator)
		}
	}
}

func TestSearch_GraphArmFailureDegrades(t *testing.T) {
	os := newOSServer(t)
	os.knnHits = []osHit{hit("d", "doc", "dense")}
	fg := &fakeGraph{err: errors.New("db down")}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1}}, fakeDocs{})
	svc.GraphArm = true
	svc.SetGraphSource(fg)
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ChunkID != "d" {
		t.Fatalf("graph failure must not break search: %+v", res.Hits)
	}
}

// #173: rendering by trust level — physical_only NEVER renders as a printed
// page ("PDF-S." prefix), folio_verified/pdf_label_sane keep "S.", the
// page_source field rides along for client-side citation gating.
func TestLocatorViewTrustLevels(t *testing.T) {
	ch := []string{"2 Grundlagen", "2.5 Stakeholder"}
	folio := locatorView(json.RawMessage(`{"type":"page_span","page_label_start":"47","page_source":"folio_verified"}`), ch)
	if folio.PageSource != "folio_verified" || !strings.Contains(folio.Label, "S. 47") || strings.Contains(folio.Label, "PDF-S.") {
		t.Fatalf("folio_verified renders as print page: %+v", folio)
	}
	phys := locatorView(json.RawMessage(`{"type":"page_span","page_label_start":"99","physical_page_start":4,"page_source":"physical_only"}`), ch)
	if !strings.Contains(phys.Label, "PDF-S. 5") || strings.Contains(phys.Label, "99") {
		t.Fatalf("physical_only must show the physical index, never the untrusted label: %+v", phys)
	}
	if phys.PageSource != "physical_only" {
		t.Fatalf("page_source must ride along: %+v", phys)
	}
	epub := locatorView(json.RawMessage(`{"type":"epub_cfi","cfi_start":"/6/4!/x"}`), ch)
	if epub.PageSource != "none" || epub.Kind != "epub_cfi" {
		t.Fatalf("epub_cfi carries none: %+v", epub)
	}
	legacy := locatorView(json.RawMessage(`{"type":"page_span","page_label_start":"12"}`), ch)
	if legacy.PageSource != "" || !strings.Contains(legacy.Label, "S. 12") {
		t.Fatalf("legacy locators carry NO guessed trust level (blank stays blank): %+v", legacy)
	}
	// physical_only with NO physical index: there is nothing trustworthy to
	// display — bare chapter, never the untrusted label as "PDF-S. 99".
	nophys := locatorView(json.RawMessage(`{"type":"page_span","page_label_start":"99","page_source":"physical_only"}`), ch)
	if strings.Contains(nophys.Label, "99") || strings.Contains(nophys.Label, "PDF-S.") {
		t.Fatalf("physical_only without a physical index must suppress the untrusted label: %+v", nophys)
	}
	if nophys.Label != "2.5 Stakeholder" || nophys.PageSource != "physical_only" {
		t.Fatalf("bare chapter is the only honest rendering: %+v", nophys)
	}
}

func TestLocatorViewEpubCFITrustLevels(t *testing.T) {
	// #229: enriched epub_cfi locators render their folio — one case per
	// trust level; absent fields stay absent (none = no pages, no fields).
	ch := 7
	span := func(pageSource string, extra string) LocatorView {
		raw := fmt.Sprintf(`{"type":"epub_cfi","cfi_start":"epubcfi(/6/16!/4/132)","page_source":%q,"page_start":175,"page_end":176,"chapter":%d%s}`,
			pageSource, ch, extra)
		return locatorView(json.RawMessage(raw), []string{"Value Chains"})
	}
	for _, lvl := range []string{"print_verified", "derived_from_sibling", "print_unverified"} {
		v := span(lvl, "")
		if v.Kind != "epub_cfi" || v.PageSource != lvl {
			t.Fatalf("%s: page_source passthrough wrong: %+v", lvl, v)
		}
		if v.Label != "Kap. 7, S. 175-176" {
			t.Fatalf("%s: label = %q, want Kap. 7, S. 175-176", lvl, v.Label)
		}
		if v.PageStart == nil || *v.PageStart != 175 || v.PageEnd == nil || *v.PageEnd != 176 {
			t.Fatalf("%s: page span fields wrong: %+v", lvl, v)
		}
		if v.ChapterNumber == nil || *v.ChapterNumber != 7 {
			t.Fatalf("%s: chapter_number missing: %+v", lvl, v)
		}
	}
	// paragraph_pages passthrough (char-exact boundaries for hit positions)
	v := locatorView(json.RawMessage(`{"type":"epub_cfi","cfi_start":"epubcfi(/6/16!/4/132)","page_source":"derived_from_sibling","page_start":175,"page_end":176,"chapter":7,"paragraph_pages":[["0","175"],["1603","176"]]}`),
		[]string{"Value Chains"})
	if len(v.ParagraphPages) != 2 || v.ParagraphPages[1][1] != "176" {
		t.Fatalf("paragraph_pages passthrough wrong: %+v", v.ParagraphPages)
	}
	// none: no pages — no page fields, no fabricated label
	bare := locatorView(json.RawMessage(`{"type":"epub_cfi","cfi_start":"epubcfi(/6/4!/4/2)"}`), []string{"Kapitel 2"})
	if bare.PageSource != "none" || bare.PageStart != nil || bare.PageEnd != nil ||
		bare.Label != "Kapitel 2" || bare.ParagraphPages != nil {
		t.Fatalf("bare epub_cfi must carry none and no page fields: %+v", bare)
	}
	// single page (start == end): no range dash
	single := locatorView(json.RawMessage(`{"type":"epub_cfi","cfi_start":"x","page_source":"print_verified","page_start":176,"page_end":176,"chapter":7}`), nil)
	if single.Label != "Kap. 7, S. 176" {
		t.Fatalf("single page label = %q", single.Label)
	}
	// pages without chapter ordinal and with section title
	noCh := locatorView(json.RawMessage(`{"type":"epub_cfi","cfi_start":"x","page_source":"print_unverified","page_start":12}`), []string{"Grundlagen"})
	if noCh.Label != "Grundlagen · S. 12" {
		t.Fatalf("no-ordinal label = %q", noCh.Label)
	}
}

// #239 regression: a filtered sparse arm must carry minimum_should_match:1.
// OpenSearch treats bool-should as optional when a filter clause exists, so
// without it a filtered document matching ZERO sparse tokens still enters
// the arm and pollutes the RRF merge. Unfiltered requests keep the plain
// bool-should shape (no filter sibling — should is already required there).
func TestSearch_SparseArmFilterMinimumShouldMatch(t *testing.T) {
	os := newOSServer(t)
	os.sparseHits = []osHit{hit("s", "doc", "sparse")}
	svc := newService(os.URL, &fakeProcessor{embedVec: []float32{1},
		embedSparse: map[string]float64{"1": 1.0}}, fakeDocs{})
	if _, err := svc.Search(context.Background(), Request{Query: "q", TopN: 1,
		Filters: &Filters{DocumentIDs: []string{"doc"}}}); err != nil {
		t.Fatal(err)
	}
	if os.lastSparseBody == nil {
		t.Fatal("sparse never queried")
	}
	raw, _ := json.Marshal(os.lastSparseBody["query"])
	if !strings.Contains(string(raw), `"minimum_should_match":1`) {
		t.Fatalf("filtered sparse arm must set minimum_should_match:1 (else zero-token docs ride the filter into RRF): %s", raw)
	}

	// passenger shape check: the filter must still wrap via the keyword
	// subfield (the #748 contract), not be replaced by the tightening.
	if !strings.Contains(string(raw), `"terms":{"document_id.keyword":["doc"]}`) {
		t.Fatalf("document filter must stay: %s", raw)
	}

	// unfiltered: no minimum_should_match key is forced (should-only bool
	// is already mandatory by default).
	os.lastSparseBody = nil
	if _, err := svc.Search(context.Background(), Request{Query: "q", TopN: 1}); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(os.lastSparseBody["query"])
	if strings.Contains(string(raw), "minimum_should_match") {
		t.Fatalf("unfiltered sparse arm must not set minimum_should_match: %s", raw)
	}
}
