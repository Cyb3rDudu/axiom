// Command retrieval-bench runs the R7 (#137) quality benchmark: the gold
// suite against the real stack (OpenSearch + query runner + Postgres), one
// pass per configuration in the matrix, reporting Precision@5, MRR,
// Recall@10 and latency p50/p95 per configuration.
//
// Env: AXIOM_OPENSEARCH_URL, AXIOM_PROCESSOR_URL (query runner with §7a),
// AXIOM_TEST_DATABASE_URL (source hydration + graph expansion).
//
//	go run ./cmd/retrieval-bench            # human-readable table
//	go run ./cmd/retrieval-bench -md out.md # additionally write markdown
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

type goldQuery struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Q    string `json:"q"`
	// Gold = zotero titles of the books a correct retrieval must surface.
	Gold []string `json:"gold"`
	// Confirmed: dudu has reviewed/approved this query's gold (0 in the
	// provisional suite; flip per query after review).
	Confirmed bool `json:"confirmed"`
	// Origin: "quality-assessment" = carried over from the QA start stock;
	// absent = implementor-derived from the library's titles.
	Origin string `json:"origin"`
}

type goldSuite struct {
	Note    string      `json:"note"`
	Queries []goldQuery `json:"queries"`
}

type config struct {
	name                string
	dense, bm25, sparse bool
	rerank, graph       bool
}

type result struct {
	name     string
	p5, mrr  float64
	r10      float64
	p50, p95 time.Duration
	qerr     int
}

func main() {
	mdPath := flag.String("md", "", "write a markdown report to this path")
	suitePath := flag.String("suite", "cmd/retrieval-bench/gold_suite.json", "gold suite file")
	flag.Parse()
	lg := log.New(os.Stderr, "bench: ", 0)

	suiteData, err := os.ReadFile(*suitePath)
	if err != nil {
		fatal("suite: %v", err)
	}
	var suite goldSuite
	if err := json.Unmarshal(suiteData, &suite); err != nil {
		fatal("suite decode: %v", err)
	}
	fmt.Printf("gold suite: %d queries (%d confirmed by dudu)\n\n",
		len(suite.Queries), countConfirmed(suite))

	osURL := envOr("AXIOM_OPENSEARCH_URL", "http://127.0.0.1:9200")
	procURL := envOr("AXIOM_PROCESSOR_URL", "http://127.0.0.1:8012")
	dbURL := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dbURL == "" {
		fatal("AXIOM_TEST_DATABASE_URL required (hydration + graph arm)")
	}
	ctx := context.Background()
	database, err := db.Open(ctx, dbURL)
	if err != nil {
		fatal("postgres: %v", err)
	}
	defer database.Close()
	pc, err := processor.New(processor.Options{BaseURL: procURL})
	if err != nil {
		fatal("runner client: %v", err)
	}
	rep := repo.New(database.Pool())

	base := search.New(osURL, "", "", pc, rep, lg)
	base.SetGraphSource(rep) // available; per-config toggle decides use

	matrix := []config{
		{name: "dense-only", dense: true},
		{name: "hybrid (dense+bm25)", dense: true, bm25: true},
		{name: "hybrid+rerank", dense: true, bm25: true, rerank: true},
		{name: "hybrid+rerank+sparse", dense: true, bm25: true, sparse: true, rerank: true},
		{name: "hybrid+rerank+sparse+graph", dense: true, bm25: true, sparse: true, rerank: true, graph: true},
	}

	// Warm the runner models once (cold loads would poison the first
	// config's latencies).
	if _, err := base.Search(ctx, search.Request{Query: "Nachhaltigkeit", TopN: 3}); err != nil {
		lg.Printf("warmup: %v", err)
	}

	var results []result
	for _, cfg := range matrix {
		results = append(results, runConfig(ctx, base, cfg, suite))
	}
	printTable(results)
	if *mdPath != "" {
		if err := writeMD(*mdPath, suite, results); err != nil {
			fatal("md: %v", err)
		}
		fmt.Printf("\nmarkdown report written to %s\n", *mdPath)
	}
}

func runConfig(ctx context.Context, base *search.Service, cfg config, suite goldSuite) result {
	base.DenseArm, base.BM25Arm, base.SparseArm = cfg.dense, cfg.bm25, cfg.sparse
	base.Rerank, base.GraphArm = cfg.rerank, cfg.graph

	var p5s, mrrs, r10s []float64
	var lat []time.Duration
	qerr := 0
	for _, gq := range suite.Queries {
		t0 := time.Now()
		res, err := base.Search(ctx, search.Request{Query: gq.Q, TopN: 10})
		lat = append(lat, time.Since(t0))
		if err != nil {
			qerr++
			p5s, mrrs, r10s = append(p5s, 0), append(mrrs, 0), append(r10s, 0)
			continue
		}
		p5s = append(p5s, precisionAt(5, res.Hits, gq.Gold))
		mrrs = append(mrrs, mrr(res.Hits, gq.Gold))
		r10s = append(r10s, recallAt(10, res.Hits, gq.Gold))
	}
	return result{
		name: cfg.name, p5: mean(p5s), mrr: mean(mrrs), r10: mean(r10s),
		p50: percentile(lat, 50), p95: percentile(lat, 95), qerr: qerr,
	}
}

// --- metrics (Hivemind re-computes these from the definitions) -----------

func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r >= 'ä' && r <= 'ü':
			b.WriteRune(r)
		case r == 'ö' || r == 'ä' || r == 'ü' || r == 'ß':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isGold(book string, gold []string) bool {
	nb := norm(book)
	for _, g := range gold {
		if nb == norm(g) {
			return true
		}
	}
	return false
}

// precisionAt: fraction of the top-k hits whose source book is gold.
func precisionAt(k int, hits []search.Hit, gold []string) float64 {
	if len(hits) == 0 {
		return 0
	}
	n := min(k, len(hits))
	good := 0
	for _, h := range hits[:n] {
		if isGold(h.Source.Book, gold) {
			good++
		}
	}
	return float64(good) / float64(n)
}

// mrr: reciprocal rank of the first gold hit.
func mrr(hits []search.Hit, gold []string) float64 {
	for i, h := range hits {
		if isGold(h.Source.Book, gold) {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// recallAt: fraction of gold books present in the top-k hits.
func recallAt(k int, hits []search.Hit, gold []string) float64 {
	if len(gold) == 0 {
		return 0
	}
	n := min(k, len(hits))
	found := map[string]bool{}
	for _, h := range hits[:n] {
		for _, g := range gold {
			if norm(h.Source.Book) == norm(g) {
				found[g] = true
			}
		}
	}
	return float64(len(found)) / float64(len(gold))
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func percentile(xs []time.Duration, p int) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	idx := int(math.Ceil(float64(p)/100*float64(len(xs)))) - 1
	if idx < 0 {
		idx = 0
	}
	return xs[idx]
}

// --- output ----------------------------------------------------------------

func printTable(rs []result) {
	fmt.Printf("%-32s %8s %8s %8s %10s %10s %5s\n", "configuration", "P@5", "MRR", "R@10", "p50", "p95", "errs")
	for _, r := range rs {
		fmt.Printf("%-32s %8.3f %8.3f %8.3f %10s %10s %5d\n",
			r.name, r.p5, r.mrr, r.r10, r.p50.Round(time.Millisecond), r.p95.Round(time.Millisecond), r.qerr)
	}
}

func writeMD(path string, suite goldSuite, rs []result) error {
	var b strings.Builder
	b.WriteString("# Retrieval Benchmark (R7, #137)\n\n")
	if countConfirmed(suite) == 0 {
		fmt.Fprintf(&b, "> **PROVISIONAL GOLD** — 0 of %d queries confirmed by dudu; verdicts may change after confirmation.\n\n", len(suite.Queries))
	}
	b.WriteString(fmt.Sprintf("Gold suite: %d queries (DE+EN; concept/fact/norm/author), %d confirmed by dudu.\n\n",
		len(suite.Queries), countConfirmed(suite)))
	b.WriteString("| configuration | P@5 | MRR | R@10 | p50 | p95 | errors |\n|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "| %s | %.3f | %.3f | %.3f | %s | %s | %d |\n",
			r.name, r.p5, r.mrr, r.r10, r.p50.Round(time.Millisecond), r.p95.Round(time.Millisecond), r.qerr)
	}
	b.WriteString("\nDefinitions: P@5 = gold-book hits / top-5; MRR = 1/rank of first gold hit; R@10 = distinct gold books found / |gold|.\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func countConfirmed(s goldSuite) int {
	n := 0
	for _, q := range s.Queries {
		if q.Confirmed {
			n++
		}
	}
	return n
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, "retrieval-bench: "+f+"\n", args...)
	os.Exit(1)
}
