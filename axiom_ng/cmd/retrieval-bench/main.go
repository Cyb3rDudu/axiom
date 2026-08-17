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
	"path/filepath"
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
	// Confirmed: dudu has reviewed/approved this query's gold — all 25
	// confirmed since Schritt 0 (2026-08-16, „alles grün").
	Confirmed bool `json:"confirmed"`
	// Origin: "quality-assessment" = carried over from the QA start stock;
	// absent = implementor-derived from the library's titles.
	Origin string `json:"origin"`
	// v2 (#155): Scope = document ids dudu works against (his chosen books,
	// 1-3 per query); GoldChunks = the expected passages inside the scope.
	// Entries with GoldChunks are scored PASSAGE-level — P@1/hit@5/hit@10 =
	// 1 iff a gold chunk sits within the top-k, MRR = 1/rank of the first
	// gold chunk — with the query scoped via filters.document_ids.
	Scope      []string `json:"scope_document_ids,omitempty"`
	GoldChunks []string `json:"gold_chunk_ids,omitempty"`
}

type goldSuite struct {
	Note    string      `json:"note"`
	Queries []goldQuery `json:"queries"`
}

type config struct {
	name                string
	dense, bm25, sparse bool
	rerank, graph       bool
	hygiene             bool // #160: frontmatter filter + collapse + diversity
}

type result struct {
	name     string
	p1, p5   float64
	mrr, r10 float64
	p50, p95 time.Duration
	qerr     int
	scoped   int // queries actually run with filters.document_ids
}

func main() {
	mdPath := flag.String("md", "", "write a markdown report to this path")
	perqPath := flag.String("perq", "", "write per-query records (JSON lines) to this path — #160 evidence: per-query P@1 + frontmatter-in-top5 flags")
	suitePath := flag.String("suite", "cmd/retrieval-bench/gold_suite.json", "gold suite file")
	propose := flag.Bool("propose", false, "v2 proposal mode: scope each query to its confirmed gold books, run scoped retrieval, emit passage proposals (JSON + yes/no list)")
	materialize21 := flag.Bool("materialize21", false, "v2.1: extend gold_suite_v2 with trace-verified VWL/ORG_HA entries -> gold_suite_v21.json")
	_ = materialize21
	materialize := flag.Bool("materialize", false, "v2 materialization: apply dudu's decisions to the proposals + anchor-resolve z1-z7, write gold_suite_v2.json")
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

	// A suite mixing book-level and passage-level entries would silently
	// blend two metric semantics into one row — refuse it outright.
	passageMode, bookN, chunkN := suiteMode(suite)
	if passageMode && bookN > 0 {
		fatal("suite mixes %d book-level and %d passage-level entries — one suite, one mode", bookN, chunkN)
	}

	if *propose {
		runPropose(context.Background(), suite, *suitePath)
		return
	}
	if *materialize21 {
		dbURL := os.Getenv("AXIOM_TEST_DATABASE_URL")
		if dbURL == "" {
			fatal("AXIOM_TEST_DATABASE_URL required")
		}
		database, err := db.Open(context.Background(), dbURL)
		if err != nil {
			fatal("postgres: %v", err)
		}
		defer database.Close()
		if err := materializeV21(context.Background(), database, filepath.Dir(*suitePath)); err != nil {
			fatal("materialize21: %v", err)
		}
		return
	}
	if *materialize {
		dbURL := os.Getenv("AXIOM_TEST_DATABASE_URL")
		if dbURL == "" {
			fatal("AXIOM_TEST_DATABASE_URL required (z-anchor resolution)")
		}
		database, err := db.Open(context.Background(), dbURL)
		if err != nil {
			fatal("postgres: %v", err)
		}
		defer database.Close()
		if err := materializeV2(context.Background(), database, filepath.Dir(*suitePath)); err != nil {
			fatal("materialize: %v", err)
		}
		fmt.Println("gold_suite_v2.json materialisiert")
		return
	}

	ctx := context.Background()
	base, database := openStack(ctx, lg)
	defer database.Close()

	matrix := []config{
		{name: "dense-only", dense: true},
		{name: "hybrid (dense+bm25)", dense: true, bm25: true},
		{name: "hybrid+rerank", dense: true, bm25: true, rerank: true},
		{name: "hybrid+rerank+sparse", dense: true, bm25: true, sparse: true, rerank: true},
		{name: "hybrid+rerank+sparse+graph", dense: true, bm25: true, sparse: true, rerank: true, graph: true},
		// #160 after-state: the v2.1 production config plus frontmatter/
		// TOC hygiene (filter + near-dup collapse + per-book diversity).
		{name: "hybrid+rerank+hygiene", dense: true, bm25: true, rerank: true, hygiene: true},
	}

	// Warm the runner models once (cold loads would poison the first
	// config's latencies).
	if _, err := base.Search(ctx, search.Request{Query: "Nachhaltigkeit", TopN: 3}); err != nil {
		lg.Printf("warmup: %v", err)
	}

	var results []result
	var perqAll []perQueryRecord
	for _, cfg := range matrix {
		r, pq := runConfig(ctx, base, cfg, suite)
		results = append(results, r)
		perqAll = append(perqAll, pq...)
	}
	printTable(results, passageMode)
	if len(results) > 0 && results[0].scoped > 0 {
		mode := "book"
		if passageMode {
			mode = "passage"
		}
		fmt.Printf("\n%d/%d queries scoped via filters.document_ids (%s-level metrics)\n",
			results[0].scoped, len(suite.Queries), mode)
	}
	if *mdPath != "" {
		if err := writeMD(*mdPath, suite, results, passageMode); err != nil {
			fatal("md: %v", err)
		}
		fmt.Printf("\nmarkdown report written to %s\n", *mdPath)
	}
	if *perqPath != "" {
		f, err := os.Create(*perqPath)
		if err != nil {
			fatal("perq: %v", err)
		}
		enc := json.NewEncoder(f)
		for _, pq := range perqAll {
			if err := enc.Encode(pq); err != nil {
				fatal("perq encode: %v", err)
			}
		}
		f.Close()
		fmt.Printf("per-query records written to %s (%d)\n", *perqPath, len(perqAll))
	}
}

// openStack wires the shared benchmark stack (env → Postgres → runner
// client → search service); the caller owns closing the database.
func openStack(ctx context.Context, lg *log.Logger) (*search.Service, *db.DB) {
	dbURL := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dbURL == "" {
		fatal("AXIOM_TEST_DATABASE_URL required (hydration + graph arm)")
	}
	database, err := db.Open(ctx, dbURL)
	if err != nil {
		fatal("postgres: %v", err)
	}
	pc, err := processor.New(processor.Options{BaseURL: envOr("AXIOM_PROCESSOR_URL", "http://127.0.0.1:8012")})
	if err != nil {
		fatal("runner client: %v", err)
	}
	rep := repo.New(database.Pool())
	svc := search.New(envOr("AXIOM_OPENSEARCH_URL", "http://127.0.0.1:9200"), "", "", pc, rep, lg)
	svc.SetGraphSource(rep) // available; per-config toggle decides use
	return svc, database
}

// suiteMode: a suite is passage-level iff any entry carries gold chunks.
// Callers refuse mixes (blended semantics); a Scope-only entry stays
// book-level — scoping and passage scoring are independent.
func suiteMode(s goldSuite) (passage bool, bookOnly, withChunks int) {
	for _, q := range s.Queries {
		if len(q.GoldChunks) > 0 {
			withChunks++
		} else {
			bookOnly++
		}
	}
	return withChunks > 0, bookOnly, withChunks
}

func countScoped(s goldSuite) int {
	n := 0
	for _, q := range s.Queries {
		if len(q.Scope) > 0 {
			n++
		}
	}
	return n
}

func runConfig(ctx context.Context, base *search.Service, cfg config, suite goldSuite) (result, []perQueryRecord) {
	base.DenseArm, base.BM25Arm, base.SparseArm = cfg.dense, cfg.bm25, cfg.sparse
	base.Rerank, base.GraphArm = cfg.rerank, cfg.graph
	// #160 hygiene levers: explicit per config so the matrix rows stay
	// comparable (existing rows = v2.1 before-state with hygiene OFF).
	base.FrontmatterFilter, base.MaxPerBook = cfg.hygiene, 0
	if cfg.hygiene {
		base.MaxPerBook = 2
	}

	var p1s, p5s, mrrs, r10s []float64
	var lat []time.Duration
	var perq []perQueryRecord
	qerr, scoped := 0, 0
	for _, gq := range suite.Queries {
		req := search.Request{Query: gq.Q, TopN: 10}
		if len(gq.Scope) > 0 {
			req.Filters = &search.Filters{DocumentIDs: gq.Scope}
			scoped++
		}
		t0 := time.Now()
		res, err := base.Search(ctx, req)
		lat = append(lat, time.Since(t0))
		pq := perQueryRecord{Config: cfg.name, ID: gq.ID, Type: gq.Type, Scoped: len(gq.Scope) > 0}
		if err != nil {
			qerr++
			pq.Error = true
			perq = append(perq, pq)
			p1s, p5s, mrrs, r10s = append(p1s, 0), append(p5s, 0), append(mrrs, 0), append(r10s, 0)
			continue
		}
		for i, h := range res.Hits {
			if i < 10 {
				pq.Top10 = append(pq.Top10, h.ChunkID)
			}
			if i < 5 && search.IsFrontmatter(h.Text) {
				pq.FrontmatterInTop5 = true
			}
		}
		if len(gq.GoldChunks) > 0 {
			// v2: passage-level scoring (the real-workflow criterion).
			p1s = append(p1s, passageAt(1, res.Hits, gq.GoldChunks))
			p5s = append(p5s, passageAt(5, res.Hits, gq.GoldChunks))
			mrrs = append(mrrs, passageMRR(res.Hits, gq.GoldChunks))
			r10s = append(r10s, passageAt(10, res.Hits, gq.GoldChunks))
		} else {
			p1s = append(p1s, precisionAt(1, res.Hits, gq.Gold))
			p5s = append(p5s, precisionAt(5, res.Hits, gq.Gold))
			mrrs = append(mrrs, mrr(res.Hits, gq.Gold))
			r10s = append(r10s, recallAt(10, res.Hits, gq.Gold))
		}
		// Desync-proof: the per-query P@1 mirrors exactly what the metric
		// aggregation above recorded for this query (gq.Gold holds book
		// titles, not chunk ids — deriving P@1 from hits again would diverge).
		pq.P1 = p1s[len(p1s)-1] > 0
		perq = append(perq, pq)
	}
	return result{
		name: cfg.name, p1: mean(p1s), p5: mean(p5s), mrr: mean(mrrs), r10: mean(r10s),
		p50: percentile(lat, 50), p95: percentile(lat, 95), qerr: qerr, scoped: scoped,
	}, perq
}

// perQueryRecord: #160 evidence line — per-query P@1, top-10 chunk ids, and
// whether any top-5 hit is frontmatter (TOC/preface/references) material.
type perQueryRecord struct {
	Config            string   `json:"config"`
	ID                string   `json:"id"`
	Type              string   `json:"type"`
	Scoped            bool     `json:"scoped"`
	P1                bool     `json:"p1"`
	FrontmatterInTop5 bool     `json:"frontmatter_in_top5"`
	Error             bool     `json:"error"`
	Top10             []string `json:"top10"`
}

// passageAt: 1 iff a gold chunk sits within the top-k (P@1 via k=1).
func passageAt(k int, hits []search.Hit, goldChunks []string) float64 {
	n := min(k, len(hits))
	for _, h := range hits[:n] {
		for _, g := range goldChunks {
			if h.ChunkID == g {
				return 1
			}
		}
	}
	return 0
}

// passageMRR: reciprocal rank of the first gold chunk.
func passageMRR(hits []search.Hit, goldChunks []string) float64 {
	for i, h := range hits {
		for _, g := range goldChunks {
			if h.ChunkID == g {
				return 1.0 / float64(i+1)
			}
		}
	}
	return 0
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
		if isGold(h.Source.Title, gold) {
			good++
		}
	}
	return float64(good) / float64(n)
}

// mrr: reciprocal rank of the first gold hit.
func mrr(hits []search.Hit, gold []string) float64 {
	for i, h := range hits {
		if isGold(h.Source.Title, gold) {
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
			if norm(h.Source.Title) == norm(g) {
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

func printTable(rs []result, passageMode bool) {
	p5l, r10l := "P@5", "R@10"
	if passageMode {
		p5l, r10l = "hit@5", "hit@10"
	}
	fmt.Printf("%-32s %8s %8s %8s %8s %10s %10s %5s\n", "configuration", "P@1", p5l, "MRR", r10l, "p50", "p95", "errs")
	for _, r := range rs {
		fmt.Printf("%-32s %8.3f %8.3f %8.3f %8.3f %10s %10s %5d\n",
			r.name, r.p1, r.p5, r.mrr, r.r10, r.p50.Round(time.Millisecond), r.p95.Round(time.Millisecond), r.qerr)
	}
}

func writeMD(path string, suite goldSuite, rs []result, passageMode bool) error {
	var b strings.Builder
	b.WriteString("# Retrieval Benchmark (R7, #137)\n\n")
	if countConfirmed(suite) == 0 {
		fmt.Fprintf(&b, "> **PROVISIONAL GOLD** — 0 of %d queries confirmed by dudu; verdicts may change after confirmation.\n\n", len(suite.Queries))
	}
	mode := "book-level (gold titles)"
	if passageMode {
		mode = "passage-level (gold chunks, scoped)"
	}
	b.WriteString(fmt.Sprintf("Gold suite: %d queries (DE+EN; concept/fact/norm/author), %d confirmed by dudu. Metrics: %s.\n\n",
		len(suite.Queries), countConfirmed(suite), mode))
	p5l, r10l := "P@5", "R@10"
	if passageMode {
		p5l, r10l = "hit@5", "hit@10"
	}
	fmt.Fprintf(&b, "| configuration | P@1 | %s | MRR | %s | p50 | p95 | errors |\n|---|---|---|---|---|---|---|---|\n", p5l, r10l)
	for _, r := range rs {
		fmt.Fprintf(&b, "| %s | %.3f | %.3f | %.3f | %.3f | %s | %s | %d |\n",
			r.name, r.p1, r.p5, r.mrr, r.r10, r.p50.Round(time.Millisecond), r.p95.Round(time.Millisecond), r.qerr)
	}
	if n := countScoped(suite); n > 0 {
		fmt.Fprintf(&b, "\n%d of %d queries scoped via filters.document_ids.\n", n, len(suite.Queries))
	}
	if passageMode {
		b.WriteString("\nDefinitions: P@1/hit@5 = 1 iff a gold chunk sits within the top-k; MRR = 1/rank of the first gold chunk; hit@10 = 1 iff a gold chunk sits within the top-10.\n")
	} else {
		b.WriteString("\nDefinitions: P@1/P@5 = gold-book hits / top-k; MRR = 1/rank of first gold hit; R@10 = distinct gold books found / |gold|.\n")
	}
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
