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
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// --- fakes -----------------------------------------------------------------

type fakeProcessor struct {
	embedErr   error
	embedVec   []float32
	rerankErr  error
	rerankRes  []processor.RerankScore
	lastRerank *RerankCapture
}

// RerankCapture records what the pipeline sent to the reranker.
type RerankCapture struct {
	Query string
	Texts []string
	TopN  int
}

func (f *fakeProcessor) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = f.embedVec
	}
	return out, nil
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
type osServer struct {
	*httptest.Server
	knnHits      []osHit
	bm25Hits     []osHit
	failKnn      bool
	failBM25     bool
	lastKnnBody  map[string]any
	lastBM25Body map[string]any
}

func newOSServer(t *testing.T) *osServer {
	t.Helper()
	s := &osServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/_search") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		q, _ := body["query"].(map[string]any)
		raw, _ := json.Marshal(q)
		isKnn := strings.Contains(string(raw), `"knn"`)
		var hits []osHit
		if isKnn {
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
	return New(osURL, "", "", p, d, log.New(io.Discard, "", 0))
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
	for i := 0; i < 20; i++ {
		os.knnHits = append(os.knnHits, hit(fmt.Sprintf("k%d", i), "d", "t"))
		os.bm25Hits = append(os.bm25Hits, hit(fmt.Sprintf("b%d", i), "d", "t"))
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
		if !strings.Contains(string(raw), `"terms"`) || !strings.Contains(string(raw), "d1") {
			t.Fatalf("%s arm query missing document filter: %s", name, raw)
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
			if h.Source.Book != "CSR Handbuch" || h.Source.Authors[0] != "Rene Schmidpeter" || *h.Source.Year != 2020 {
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
		if h.Source.Book != "" {
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
