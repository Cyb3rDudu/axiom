package search

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

// coarseChunk builds a chunk > minRerankSplitChars whose SECOND half carries a
// distinguishable "answer" sentence — the coarse-chunk shape #200 targets: the
// whole text is multi-topic, so a whole-chunk rerank buries the answer.
func coarseChunk(firstHalf, secondHalf string) string {
	// Pad the first half so the total comfortably exceeds the split threshold
	// and the halves land far apart, mirroring real 4000+ char chunks.
	filler := strings.Repeat("filler wort thema inhalts neutral satz beispiel weiterer context. ", 60)
	return firstHalf + filler + secondHalf
}

// TestSearch_SpanMaxRerankSurfacesCoarseGold pins the #200 fix: when a coarse
// chunk's answer sentence lives in one half, scoring the chunk WHOLE buries it
// (z2 rank 9, z7 outside top-10), but span-max reranking — max over the two
// window-halves — surfaces it to rank 1. The test is discriminating: if the
// span-splitting is ever removed, the whole-chunk score for the gold is low,
// so the gold must fall out of rank 1 and the length assertion fails too.
func TestSearch_SpanMaxRerankSurfacesCoarseGold(t *testing.T) {
	os := newOSServer(t)
	gold := "coarse-gold"
	// a, c, d: short single-topic distractors (1 span, whole text).
	// b (gold): >1100 chars, second half carries the answer.
	os.knnHits = []osHit{
		hit("a", "d1", "kurzer distraktor passage"),
		hit(gold, "d2", coarseChunk(
			"Viele Unternehmen verfassen Nachhaltigkeitsberichte aus externem Druck und fuer ihre Stakeholder.",
			"Doch das Nachhaltigkeitsreporting bildet auch intern eine Grundlage fuer bessere Entscheidungen.",
		)),
		hit("c", "d3", "ein anderer kurzer distraktor"),
		hit("d", "d4", "weiterhin ein dritter distraktor text"),
	}
	fp := &fakeProcessor{embedVec: []float32{1}}
	// Span order: a(0), gold-lo(1), gold-hi(2), c(3), d(4). The gold's whole
	// chunk would score like its low half; its answer half scores highest.
	fp.rerankRes = []processor.RerankScore{
		{Index: 0, Score: 0.4}, // a
		{Index: 1, Score: 0.1}, // gold first half (off-topic) — whole-chunk baseline
		{Index: 2, Score: 0.9}, // gold SECOND half = the answer
		{Index: 3, Score: 0.3}, // c
		{Index: 4, Score: 0.2}, // d
	}
	svc := newService(os.URL, fp, fakeDocs{})
	res, err := svc.Search(context.Background(), Request{Query: "q", TopN: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reranked {
		t.Fatal("valid span scores must set reranked=true")
	}
	// The coarse gold got split into 2 spans (short distractors stayed whole),
	// so the reranker saw 5 texts, not the 4 whole chunks.
	if fp.lastRerank == nil {
		t.Fatal("rerank must have run")
	}
	if len(fp.lastRerank.Texts) != 5 {
		t.Fatalf("span-max must split the coarse gold into 2 spans + 3 whole distractors, got %d texts", len(fp.lastRerank.Texts))
	}
	// And the answer half must be among the reranked texts (the whole chunk's
	// later half is what spanWindow sends).
	if !strings.Contains(strings.Join(fp.lastRerank.Texts, "\n"), "auch intern eine Grundlage") {
		t.Fatal("rerank must receive the answer-bearing span, not only the off-topic whole")
	}
	if len(res.Hits) == 0 || res.Hits[0].ChunkID != gold {
		// Without span-max the gold's whole-chunk score is 0.1 -> it ranks
		// below a/c/d. Surfacing to rank 1 is the regression pin.
		t.Fatalf("span-max must surface the coarse gold to rank 1, got %s", orderOf(res))
	}
}

// TestSearch_SpanMaxSplitSurvivesLargeFetchN pins the #200-cliff fix: spanWindow
// must split the top coarse candidates REGARDLESS of how many candidates there
// are. The early k = maxCandidates/n arithmetic silently collapsed the split to
// whole once fetchN exceeded 32 (top_n >= 11), turning the fix off outside the
// bench config. Now the split is decoupled from n; only the 64-text runner cap
// may bind, and then by trimming the weakest RRF tail, never the top-split.
func TestSearch_SpanMaxSplitSurvivesLargeFetchN(t *testing.T) {
	for _, n := range []int{60, 64} { // fetchN>32: top_n>=20
		cands := make([]osCandidate, 0, n)
		for i := 0; i < n; i++ {
			text := "short whole candidate"
			if i < maxSplitCandidates { // top-4 coarse: the #200 target shape
				text = strings.Repeat("gross incoherent chunk viele themen filler satz ", 170)
			}
			cands = append(cands, osCandidate{ID: fmt.Sprintf("c%d", i), Text: text})
		}
		spans, owners, rep := spanWindow(cands)
		if len(spans) > maxCandidates {
			t.Fatalf("n=%d: %d spans exceed rerank cap %d", n, len(spans), maxCandidates)
		}
		if len(spans) < maxSplitCandidates*2 {
			t.Fatalf("n=%d: split vanished (got %d spans); the top-%d coarse candidates must stay split at large fetchN",
				n, len(spans), maxSplitCandidates)
		}
		// Discriminating: with the top-%d coarse split, spans = n + %d (capped
		// at maxCandidates), and nothing may be trimmed below n=61. A
		// reintroduced cliff (all whole) would yield n spans and rep=n instead.
		expSpans := min(n+maxSplitCandidates, maxCandidates)
		expRep := n
		if n > maxCandidates-maxSplitCandidates {
			expRep = maxCandidates - maxSplitCandidates
		}
		if len(spans) != expSpans || rep != expRep {
			t.Fatalf("n=%d: spans=%d rep=%d, want spans=%d rep=%d (fixed allocation must hold)",
				n, len(spans), rep, expSpans, expRep)
		}
		if len(spans) != len(owners) {
			t.Fatalf("n=%d: spans/owners length mismatch", n)
		}
		if n > 60 && rep < maxSplitCandidates {
			t.Fatalf("n=%d: represented=%d dropped a split candidate", n, rep)
		}
	}
	// Cap enforcement: with n=64 all coarse-top split, the tail (weakest whole
	// candidates) is trimmed, the split survives, and spans == maxCandidates.
	cands := make([]osCandidate, 0, 64)
	for i := 0; i < 64; i++ {
		text := strings.Repeat("kurz ganzer kandidat ", 3)
		if i < maxSplitCandidates {
			text = strings.Repeat("gross incoherent chunk viele themen filler ", 180)
		}
		cands = append(cands, osCandidate{ID: fmt.Sprintf("d%d", i), Text: text})
	}
	spans, owners, rep := spanWindow(cands)
	if len(spans) != maxCandidates {
		t.Fatalf("cap: got %d spans, want exactly %d (split preserved, tail trimmed)", len(spans), maxCandidates)
	}
	if rep != maxCandidates-maxSplitCandidates {
		t.Fatalf("cap: represented=%d, want %d (weakest whole candidates dropped)", rep, maxCandidates-maxSplitCandidates)
	}
	if len(spans) != len(owners) {
		t.Fatalf("cap: spans/owners mismatch")
	}
}
