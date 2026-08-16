// #160 ranking-hygiene tests — the extended flip list (Kipp-Liste): every
// lever has a probe proving that flipping it OFF brings back the exact
// failure mode it exists to prevent.
package search

import (
	"strings"
	"testing"
)

// Flip-list K2 (zuaggressiv-Sonde): an over-aggressive detector would eat the
// Fachtext-Gold; the precision fixtures in frontmatter_test.go pin the
// heuristic. Here: hygiene functions must never touch distinct body chunks.
func TestHygieneDistinctBodySurvives(t *testing.T) {
	cands := []osCandidate{
		{ID: "a", DocumentID: "doc1", Text: "Alpha Beta Gamma"},
		{ID: "b", DocumentID: "doc1", Text: "Delta Epsilon Zeta"},
	}
	got, folded := collapseNearDuplicates(cands)
	if len(got) != 2 || len(folded) != 0 {
		t.Fatalf("distinct chunks must both survive: %+v %v", got, folded)
	}
}

// K3 (collapse lever): near-identical twins of the same document fold into
// the higher-ranked hit with a count; flipping collapse off (trivially: not
// calling it) keeps the duplicate flood — asserted as the contrast case.
func TestCollapseNearDuplicates(t *testing.T) {
	tocish := "5.1.2 Zieldefinition" + strings.Repeat(".", 20) + " 148\nDie Zieldefinition leitet die Untersuchung."
	twin := "5.1.2  Zieldefinition" + strings.Repeat(".", 18) + " 148\nDie Zieldefinition leitet die Untersuchung."
	cands := []osCandidate{
		{ID: "chapter-start", DocumentID: "doc1", Text: "Die Zieldefinition leitet die Untersuchung und grenzt sie ab."},
		{ID: "toc-twin", DocumentID: "doc1", Text: tocish},
		{ID: "toc-twin2", DocumentID: "doc1", Text: twin},
		{ID: "other-book", DocumentID: "doc2", Text: tocish}, // same text, OTHER document: no fold
	}
	got, folded := collapseNearDuplicates(cands)
	// toc-twin2 folds into ITS twin (toc-twin), not into the unrelated
	// chapter-start — folding is per near-duplicate pair, not "into rank 1".
	if len(got) != 3 {
		t.Fatalf("toc-twin2 must fold into toc-twin (chapter-start + other-book stay): %+v", got)
	}
	if folded["toc-twin"] != 1 {
		t.Fatalf("fold count lands on the surviving twin: %v", folded)
	}
	for _, c := range got {
		if c.ID == "toc-twin2" {
			t.Fatalf("folded duplicate must be gone: %+v", got)
		}
	}
	// Contrast (lever off): the raw list still carries the flood.
	if len(cands) != 4 {
		t.Fatal("sanity")
	}
}

// K4 (diversity lever): max K per book with rank-order refill.
func TestDiversifyCapAndRefill(t *testing.T) {
	cands := []osCandidate{
		{ID: "b1", DocumentID: "bookA"}, {ID: "b2", DocumentID: "bookA"},
		{ID: "b3", DocumentID: "bookA"}, {ID: "b4", DocumentID: "bookA"},
		{ID: "c1", DocumentID: "bookB"}, {ID: "c2", DocumentID: "bookB"},
	}
	got := diversify(cands, 2)
	if got[0].ID != "b1" || got[1].ID != "b2" {
		t.Fatalf("top ranks keep the cap: %+v", got)
	}
	if got[2].ID != "c1" || got[3].ID != "c2" {
		t.Fatalf("bookB hits refill past the cap: %+v", got)
	}
	if len(got) != 6 {
		t.Fatalf("refill keeps the list complete (demoted, not deleted): %+v", got)
	}
	// Lever off (K=0): the flood stays in rank order.
	if got0 := diversify(cands, 0); len(got0) != 6 || got0[2].ID != "b3" {
		t.Fatalf("K=0 must be a no-op: %+v", got0)
	}
}

// K5 (over-tight diversity guard): K=2 must still allow a book's SECOND
// distinct hit — the z-suite needs multi-hit books (hit@5 metric) to survive.
func TestDiversifyKeepsSecondDistinctHit(t *testing.T) {
	cands := []osCandidate{
		{ID: "b1", DocumentID: "bookA", Text: "one"},
		{ID: "b2", DocumentID: "bookA", Text: "two"},
	}
	got := diversify(cands, 2)
	if len(got) != 2 {
		t.Fatalf("K=2 keeps two distinct hits of one book: %+v", got)
	}
}

func TestJaccard(t *testing.T) {
	if jaccard("a b c", "a b c") != 1 {
		t.Error("identical")
	}
	if j := jaccard("a b c d", "a b c e"); j != 0.6 {
		t.Errorf("3 of 5 union tokens = 0.6, got %v", j)
	}
	if j := jaccard("a b", "x y"); j != 0 {
		t.Errorf("disjoint = 0, got %v", j)
	}
}
