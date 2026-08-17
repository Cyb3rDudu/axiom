package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

func hitBook(b string) search.Hit { return search.Hit{Source: repo.SourceView{Title: b}} }

func hitChunk(c string) search.Hit { return search.Hit{ChunkID: c} }

func chunks(n int) []search.Hit {
	hs := make([]search.Hit, n)
	for i := range hs {
		hs[i] = hitChunk(fmt.Sprintf("c%d", i+1))
	}
	return hs
}

func TestPassageAt(t *testing.T) {
	ten := chunks(10)
	for _, tc := range []struct {
		name string
		k    int
		gold []string
		want float64
	}{
		{"P@1 gold at rank 1", 1, []string{"c1"}, 1},
		{"P@1 gold at rank 2", 1, []string{"c2"}, 0},
		{"hit@5 gold at rank 5 (boundary)", 5, []string{"c5"}, 1},
		{"hit@5 gold at rank 6 misses", 5, []string{"c6"}, 0},
		{"any of multiple gold chunks counts", 5, []string{"zz", "c3"}, 1},
		{"k beyond hits", 10, []string{"c2"}, 1},
	} {
		if got := passageAt(tc.k, ten, tc.gold); got != tc.want {
			t.Fatalf("%s: passageAt(%d) = %v, want %v", tc.name, tc.k, got, tc.want)
		}
	}
	// k beyond hits with gold outside the shorter slice must not panic
	// or match.
	if got := passageAt(10, chunks(2), []string{"c3"}); got != 0 {
		t.Fatalf("passageAt(10, 2 hits, gold c3) = %v, want 0", got)
	}
}

func TestPassageMRR(t *testing.T) {
	ten := chunks(10)
	for _, tc := range []struct {
		name string
		gold []string
		want float64
	}{
		{"rank 1", []string{"c1"}, 1},
		{"rank 3", []string{"c3"}, 1.0 / 3.0},
		{"absent", []string{"zz"}, 0},
		{"first-hit rule: gold at 2 and 4 → 1/2", []string{"c4", "c2"}, 0.5},
	} {
		if got := passageMRR(ten, tc.gold); got != tc.want {
			t.Fatalf("%s: passageMRR = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSuiteMode(t *testing.T) {
	// Book-only suite: not passage, all counted book-level.
	p, bookN, chunkN := suiteMode(goldSuite{Queries: []goldQuery{
		{ID: "a", Gold: []string{"x"}}, {ID: "b", Gold: []string{"y"}},
	}})
	if p || bookN != 2 || chunkN != 0 {
		t.Fatalf("book-only suite: passage=%v book=%d chunks=%d", p, bookN, chunkN)
	}
	// Mixed suite must be visible to main's guard (both counts > 0).
	p, bookN, chunkN = suiteMode(goldSuite{Queries: []goldQuery{
		{ID: "a", Gold: []string{"x"}}, {ID: "b", GoldChunks: []string{"c"}},
	}})
	if !p || bookN != 1 || chunkN != 1 {
		t.Fatalf("mixed suite: passage=%v book=%d chunks=%d", p, bookN, chunkN)
	}
	// Scope-only entries stay book-level: scoping ≠ passage scoring.
	p, bookN, chunkN = suiteMode(goldSuite{Queries: []goldQuery{
		{ID: "a", Gold: []string{"x"}, Scope: []string{"d1"}},
	}})
	if p || bookN != 1 || chunkN != 0 {
		t.Fatalf("scope-only suite: passage=%v book=%d chunks=%d", p, bookN, chunkN)
	}
}

func TestPrecisionAt(t *testing.T) {
	hits := []search.Hit{hitBook("CSR und Reporting"), hitBook("Anderes Buch"), hitBook("CSR und Finance"), hitBook("X"), hitBook("Y")}
	got := precisionAt(5, hits, []string{"csr und reporting", "CSR UND FINANCE"})
	if got != 0.4 {
		t.Fatalf("P@5 = %v, want 0.4 (2 gold of 5)", got)
	}
	if p := precisionAt(3, hits[:3], []string{"Anderes Buch"}); p != 1.0/3.0 {
		t.Fatalf("P@5 partial = %v", p)
	}
}

func TestMRR(t *testing.T) {
	if m := mrr([]search.Hit{hitBook("x"), hitBook("G")}, []string{"g"}); m != 0.5 {
		t.Fatalf("MRR = %v, want 0.5", m)
	}
	if m := mrr([]search.Hit{hitBook("x")}, []string{"g"}); m != 0 {
		t.Fatal("MRR without gold hit must be 0")
	}
	if m := mrr([]search.Hit{hitBook("G")}, []string{"g"}); m != 1 {
		t.Fatal("MRR rank 1 must be 1")
	}
}

func TestRecallAt(t *testing.T) {
	hits := []search.Hit{hitBook("A"), hitBook("B"), hitBook("C")}
	if r := recallAt(10, hits, []string{"A", "B", "Z"}); r != 2.0/3.0 {
		t.Fatalf("R@10 = %v, want 2/3", r)
	}
}

func TestNormStripsPunctuationAndCase(t *testing.T) {
	if norm("CSR und Reporting – Mythen!") != norm("csr und reporting  mythen") {
		t.Fatalf("norm mismatch: %q vs %q", norm("CSR und Reporting – Mythen!"), norm("csr und reporting  mythen"))
	}
	if norm("Environmental, Social and Governance (ESG)") != norm("environmental social and governance esg") {
		t.Fatal("parens/comma normalization broken")
	}
	// Long titles differing only after char 12 must stay distinct: pins
	// that norm neither truncates nor over-collapses prefixes.
	a := norm("CSR und Reporting: Standards")
	b := norm("CSR und Reporting: Mythen")
	if a == b {
		t.Fatalf("norm collided distinct titles: %q", a)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	xs := make([]time.Duration, 100)
	for i := range xs {
		xs[i] = time.Duration(i) * time.Millisecond
	}
	// Nearest-rank: k = ceil(p/100*n); xs[i] = i ms -> the k-th smallest
	// is xs[k-1]. n=100: p95 -> 95th value = 94ms; p50 -> 50th = 49ms.
	if p := percentile(xs, 95); p != 94*time.Millisecond {
		t.Fatalf("p95 = %v, want 94ms", p)
	}
	if p := percentile(xs, 50); p != 49*time.Millisecond {
		t.Fatalf("p50 = %v, want 49ms", p)
	}
	// Same multiset, reversed: pins that percentile sorts rather than
	// trusting input order (index 94 of the raw reversed slice is 5ms).
	rev := make([]time.Duration, len(xs))
	for i, v := range xs {
		rev[len(xs)-1-i] = v
	}
	if p := percentile(rev, 95); p != 94*time.Millisecond {
		t.Fatalf("p95(unsorted) = %v, want 94ms (sort is pinned)", p)
	}
	if p := percentile(rev, 50); p != 49*time.Millisecond {
		t.Fatalf("p50(unsorted) = %v, want 49ms (sort is pinned)", p)
	}
}

func TestGoldSuiteLoads(t *testing.T) {
	suiteData, err := os.ReadFile("gold_suite.json")
	if err != nil {
		t.Fatal(err)
	}
	var s goldSuite
	if err := json.Unmarshal(suiteData, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Queries) < 20 {
		t.Fatalf("suite too small: %d", len(s.Queries))
	}
	ids := map[string]bool{}
	for _, q := range s.Queries {
		if q.ID == "" || q.Q == "" || len(q.Gold) == 0 {
			t.Fatalf("incomplete query entry %+v", q)
		}
		if ids[q.ID] {
			t.Fatalf("duplicate id %s", q.ID)
		}
		ids[q.ID] = true
	}
}
