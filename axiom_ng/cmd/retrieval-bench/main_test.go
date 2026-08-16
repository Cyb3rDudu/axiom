package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

func hitBook(b string) search.Hit { return search.Hit{Source: search.Source{Book: b}} }

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
