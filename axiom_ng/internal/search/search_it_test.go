package search

// End-to-end integration proofs for POST /api/search (epic #130 R3, #133).
// NOT part of the hermetic suite: talks to the REAL OpenSearch index, the
// REAL runner (R1/R2 warm endpoints) and the REAL dev Postgres. No stubs.
//
// Run with:
//   AXIOM_SEARCH_IT=1 \
//   AXIOM_PROCESSOR_URL=http://127.0.0.1:8012 \
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user@127.0.0.1:5444/axiom_ng_test?sslmode=disable \
//   go test ./internal/search/ -run IT -v
//
// The runner must be up with AXIOM_PROCESSOR_COMPUTE=real.

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

func itEnv(t *testing.T) (osURL, procURL, dbURL string) {
	if os.Getenv("AXIOM_SEARCH_IT") != "1" {
		t.Skip("AXIOM_SEARCH_IT=1 required (real OS + real runner + real DB)")
	}
	osURL = os.Getenv("AXIOM_OPENSEARCH_URL")
	if osURL == "" {
		osURL = "http://127.0.0.1:9200"
	}
	procURL = os.Getenv("AXIOM_PROCESSOR_URL")
	if procURL == "" {
		procURL = "http://127.0.0.1:8012"
	}
	dbURL = os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL required for source hydration")
	}
	return
}

func itService(t *testing.T, osURL, procURL, dbURL string) *Service {
	ctx := context.Background()
	database, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	pc, err := processor.New(processor.Options{BaseURL: procURL})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(osURL, "", "", pc, repo.New(database.Pool()), log.New(os.Stderr, "it-search: ", 0))
	// The R5 capability proof asserts arms.sparse per query — independent
	// of the production default (benchmark-driven off since R7).
	svc.SparseArm = true
	return svc
}

// itQueries are realistic library questions (DE/EN mix, quality-assessment
// style topics from the corpus). Each carries explicit topic anchors: an
// answer is "on topic" when a top-3 hit contains >=2 anchors (substring,
// German compounds). Anchors name the TOPIC vocabulary, not the exact query
// words — semantic hits legitimately paraphrase ("Lieferkettengesetz" hits
// discuss Sorgfaltspflichten/Menschenrechte/Haftung).
var itQueries = []struct {
	q       string
	anchors string
}{
	{"Was ist Corporate Social Responsibility und welche Dimensionen hat es?", "corporate social responsibility csr gesellschaft verantwortung nachhaltigkeit"},
	{"Nachhaltigkeitsberichterstattung CSRD Anforderungen an Unternehmen", "nachhaltigkeitsbericht csrd reporting berichterstattung csr"},
	{"Wie funktioniert eine Ökobilanz Life Cycle Assessment?", "bilanz life cycle assessment lebenszyklus ökobilanz lca"},
	{"Lieferkettensorgfaltspflichtengesetz Anforderungen Unternehmen", "lieferkett sorgfaltspflicht menschenrecht haftung verantwortung"},
	{"ESG rating agencies influence on corporate sustainability reporting", "esg rating sustainability reporting nachhaltigkeit bewertung"},
}

func TestIT_SearchEndToEnd(t *testing.T) {
	osURL, procURL, dbURL := itEnv(t)
	svc := itService(t, osURL, procURL, dbURL)
	ctx := context.Background()

	// Warmup: pays the cold model loads (embedder + reranker) once.
	if _, err := svc.Search(ctx, Request{Query: "Nachhaltigkeit", TopN: 5}); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	lat := make([]float64, 0, len(itQueries))
	for _, qc := range itQueries {
		q := qc.q
		t0 := time.Now()
		res, err := svc.Search(ctx, Request{Query: q, TopN: 5})
		dt := time.Since(t0)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		lat = append(lat, dt.Seconds())
		if len(res.Hits) == 0 {
			t.Fatalf("query %q returned no hits", q)
		}
		if !res.Reranked {
			t.Fatalf("query %q not reranked (runner up expected)", q)
		}
		if !res.Arms.Dense || !res.Arms.BM25 {
			t.Fatalf("query %q arms degraded: %+v", q, res.Arms)
		}
		// R5 (#135): the learned-lexical arm must contribute live against
		// the backfilled rank_features index.
		if !res.Arms.Sparse {
			t.Fatalf("query %q sparse arm did not fire: %+v", q, res.Arms)
		}
		// On-topic smoke against the query's explicit topic anchors.
		if !onTopic(qc.anchors, res.Hits[:min(3, len(res.Hits))]) {
			t.Fatalf("query %q off-topic top-3:\n%s", q, dumpHits(res.Hits[:min(3, len(res.Hits))]))
		}
		// Every hit must carry a locator and a source doc id.
		for _, h := range res.Hits {
			if h.Locator.Kind == "none" || h.Locator.Label == "" {
				t.Fatalf("hit %s without usable locator: %+v", h.ChunkID, h.Locator)
			}
			if h.Source.DocID == "" {
				t.Fatalf("hit %s without document provenance", h.ChunkID)
			}
		}
		top := res.Hits[0]
		t.Logf("[IT] %0.1fs q=%q -> %q | %s | %s | %.3f", dt.Seconds(), q[:min(40, len(q))],
			truncateChars(top.Text, 60), top.Source.Book, top.Locator.Label, top.Score)
	}
	sort.Float64s(lat)
	p95 := lat[int(math.Ceil(0.95*float64(len(lat))))-1]
	t.Logf("[IT] search latency LOCAL (%s): p50=%.2fs p95=%.2fs max=%.2fs (n=%d, warm, top_n=5, 3x overfetch=15 rerank candidates)",
		envOr(os.Getenv("AXIOM_IT_DEVICE"), "MPS fp32"), lat[len(lat)/2], p95, lat[len(lat)-1], len(lat))
	// Issue Ziel 4 targets p95 < 2s; the budget assumes rerank top-30 at
	// 0.5-1s (CUDA/Carrier). On the local Mac the fp32 cross-encoder is
	// ~130ms/pair, so 15 candidates cost ~2s alone — a hardware bound, not
	// a pipeline regression. 2.5s is the hard sanity bound; the exact local
	// number is the DoD evidence and gets documented on the issue.
	if p95 > 2.5 {
		t.Fatalf("p95 %.2fs exceeds the 2.5s local sanity bound", p95)
	}
}

func TestIT_SearchRerankFallbackWithRunnerDown(t *testing.T) {
	// DoD: runner weg → results still served, flagged. A dead runner port
	// makes embed AND rerank fail: BM25-only recall + reranked=false.
	osURL, _, dbURL := itEnv(t)
	svc := itService(t, osURL, "http://127.0.0.1:1", dbURL) // nothing listens on port 1
	res, err := svc.Search(context.Background(), Request{Query: "Corporate Social Responsibility", TopN: 5})
	if err != nil {
		t.Fatalf("runner down must not fail search: %v", err)
	}
	if res.Reranked {
		t.Fatal("runner down must report reranked=false")
	}
	if res.Arms.Dense {
		t.Fatal("runner down must clear the dense arm")
	}
	if !res.Arms.BM25 || len(res.Hits) == 0 {
		t.Fatalf("BM25 fallback must serve hits, got %+v", res.Arms)
	}
	t.Logf("[IT] fallback: %d hits, reranked=%v, arms=%+v, top=%q",
		len(res.Hits), res.Reranked, res.Arms, truncateChars(res.Hits[0].Text, 60))
}

func onTopic(anchors string, hits []Hit) bool {
	// Substring containment over the hit's text + section + book title:
	// German compounds make substring the honest lexical signal.
	for _, h := range hits {
		lt := strings.ToLower(h.Text + " " + strings.Join(h.Section, " ") + " " + h.Source.Book)
		ov := 0
		for _, a := range strings.Fields(anchors) {
			if strings.Contains(lt, a) {
				ov++
			}
		}
		if ov >= 2 {
			return true
		}
	}
	return false
}

func dumpHits(hits []Hit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "  - %q loc=%s src=%q\n", truncateChars(h.Text, 80), h.Locator.Label, h.Source.Book)
	}
	return b.String()
}

func envOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// TestIT_SearchGraphArmOn proves the R6 (#136) graph arm runs live:
// same correct answers, graph candidates may join the pool. Quality-lift
// measurement is R7's job (flag stays default-off in prod).
func TestIT_SearchGraphArmOn(t *testing.T) {
	osURL, procURL, dbURL := itEnv(t)
	svc := itService(t, osURL, procURL, dbURL)
	database, _ := db.Open(context.Background(), dbURL)
	defer database.Close()
	svc.GraphArm = true
	svc.SetGraphSource(repo.New(database.Pool()))
	res, err := svc.Search(context.Background(), Request{
		Query: "Sustainable Development Goals der Vereinten Nationen", TopN: 5,
	})
	if err != nil {
		t.Fatalf("graph-arm search failed: %v", err)
	}
	if len(res.Hits) == 0 || !res.Arms.Dense || !res.Arms.BM25 || !res.Arms.Sparse {
		t.Fatalf("arms degraded under graph expansion: %+v", res.Arms)
	}
	if !onTopic("sdg sustainable development vereinten nationen", res.Hits[:min(3, len(res.Hits))]) {
		t.Fatalf("graph-arm run off-topic:\n%s", dumpHits(res.Hits[:min(3, len(res.Hits))]))
	}
	t.Logf("[IT] graph arm ON: top=%q | %s | %.3f (pool %d candidates, hybrid quality held)",
		truncateChars(res.Hits[0].Text, 50), res.Hits[0].Source.Book, res.Hits[0].Score, len(res.Hits))
}
