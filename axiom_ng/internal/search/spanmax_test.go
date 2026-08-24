package search

import (
	"context"
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
