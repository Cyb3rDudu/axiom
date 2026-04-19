package retriever_test

import (
	"math"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/retriever"
)

func TestRRFWithEqualWeightsPutsMutualHitsOnTop(t *testing.T) {
	t.Parallel()
	fused := retriever.RRF([]retriever.FusionInput{
		{Name: "a", Weight: 1, Hits: []retriever.Ranked{{ChunkID: "x"}, {ChunkID: "y"}, {ChunkID: "z"}}},
		{Name: "b", Weight: 1, Hits: []retriever.Ranked{{ChunkID: "y"}, {ChunkID: "x"}, {ChunkID: "w"}}},
	}, 0)
	// y and x both appear in both channels; they should rank first.
	if fused[0].ChunkID != "y" && fused[0].ChunkID != "x" {
		t.Errorf("expected x or y first, got %q", fused[0].ChunkID)
	}
	if fused[1].ChunkID != "y" && fused[1].ChunkID != "x" {
		t.Errorf("expected x or y second, got %q", fused[1].ChunkID)
	}
}

func TestRRFAppliesWeights(t *testing.T) {
	t.Parallel()
	// Channel 'a' weighted heavily; its top hit must beat channel 'b'.
	fused := retriever.RRF([]retriever.FusionInput{
		{Name: "a", Weight: 9, Hits: []retriever.Ranked{{ChunkID: "heavy_top"}}},
		{Name: "b", Weight: 1, Hits: []retriever.Ranked{{ChunkID: "light_top"}}},
	}, 60)
	if fused[0].ChunkID != "heavy_top" {
		t.Errorf("weighted channel should win: %+v", fused)
	}
}

func TestRRFHandlesZeroWeightsByEqualising(t *testing.T) {
	t.Parallel()
	fused := retriever.RRF([]retriever.FusionInput{
		{Name: "a", Weight: 0, Hits: []retriever.Ranked{{ChunkID: "x"}}},
		{Name: "b", Weight: 0, Hits: []retriever.Ranked{{ChunkID: "y"}}},
	}, 60)
	if len(fused) != 2 {
		t.Fatalf("want 2 hits, got %d", len(fused))
	}
	diff := math.Abs(fused[0].CombinedScore - fused[1].CombinedScore)
	if diff > 1e-9 {
		t.Errorf("equal weights should produce equal scores, diff=%v", diff)
	}
}

func TestRRFPreservesChannelRanksAndScores(t *testing.T) {
	t.Parallel()
	fused := retriever.RRF([]retriever.FusionInput{
		{Name: "dense", Weight: 0.7, Hits: []retriever.Ranked{{ChunkID: "a", Score: 0.9}, {ChunkID: "b", Score: 0.8}}},
		{Name: "bm25", Weight: 0.3, Hits: []retriever.Ranked{{ChunkID: "b", Score: 3.1}}},
	}, 60)
	// Find chunk b — should have both channel scores populated.
	var bHit *retriever.FusedHit
	for i := range fused {
		if fused[i].ChunkID == "b" {
			bHit = &fused[i]
		}
	}
	if bHit == nil {
		t.Fatal("b missing from fused results")
	}
	if bHit.ChannelScores["dense"] != 0.8 {
		t.Errorf("dense score for b: %v", bHit.ChannelScores["dense"])
	}
	if bHit.ChannelScores["bm25"] != 3.1 {
		t.Errorf("bm25 score for b: %v", bHit.ChannelScores["bm25"])
	}
	if bHit.ChannelRanks["dense"] != 2 || bHit.ChannelRanks["bm25"] != 1 {
		t.Errorf("ranks for b: %+v", bHit.ChannelRanks)
	}
}

func TestRRFEmptyInputs(t *testing.T) {
	t.Parallel()
	out := retriever.RRF(nil, 0)
	if len(out) != 0 {
		t.Errorf("empty inputs: %d", len(out))
	}
}

func TestRRFNegativeWeightClampsToZero(t *testing.T) {
	t.Parallel()
	out := retriever.RRF([]retriever.FusionInput{
		{Name: "neg", Weight: -1, Hits: []retriever.Ranked{{ChunkID: "x"}}},
		{Name: "ok", Weight: 1, Hits: []retriever.Ranked{{ChunkID: "y"}}},
	}, 60)
	if out[0].ChunkID != "y" {
		t.Errorf("positive-weight channel should win: %+v", out)
	}
}

func TestSparseCosineMatchesPython(t *testing.T) {
	t.Parallel()
	a := retriever.SparseVector{"1": 2, "3": 4}
	b := retriever.SparseVector{"1": 1, "2": 5, "3": 0.5}
	// dot = 2*1 + 4*0.5 = 4
	// |a| = sqrt(4+16) = sqrt(20), |b| = sqrt(1+25+0.25) = sqrt(26.25)
	got := a.Cosine(b)
	want := 4.0 / (math.Sqrt(20) * math.Sqrt(26.25))
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cosine: got %v, want %v", got, want)
	}
}

func TestSparseCosineEmptySidesReturnZero(t *testing.T) {
	t.Parallel()
	var empty retriever.SparseVector
	if got := empty.Cosine(retriever.SparseVector{"1": 1}); got != 0 {
		t.Errorf("empty-lhs: %v", got)
	}
	if got := (retriever.SparseVector{"1": 1}).Cosine(empty); got != 0 {
		t.Errorf("empty-rhs: %v", got)
	}
	// Disjoint keys: dot=0 → cosine=0
	if got := (retriever.SparseVector{"1": 1}).Cosine(retriever.SparseVector{"2": 1}); got != 0 {
		t.Errorf("disjoint: %v", got)
	}
}

func TestDecodeSparseHandlesStringKeys(t *testing.T) {
	t.Parallel()
	out := retriever.DecodeSparse([]byte(`{"42": 0.5, "99": 0.1}`))
	if out["42"] != 0.5 || out["99"] != 0.1 {
		t.Errorf("decoded: %+v", out)
	}
}

func TestDecodeSparseFallsBackToIntKeys(t *testing.T) {
	t.Parallel()
	// Legacy migration stored int keys — try the fallback branch.
	out := retriever.DecodeSparse([]byte(`{"1": 0.9}`))
	if out["1"] != 0.9 {
		t.Errorf("decoded: %+v", out)
	}
}

func TestDecodeSparseHandlesInvalidJSON(t *testing.T) {
	t.Parallel()
	out := retriever.DecodeSparse([]byte(`not json`))
	if len(out) != 0 {
		t.Errorf("bad json should yield empty map, got %+v", out)
	}
}

func TestDecodeSparseEmptyInputReturnsEmpty(t *testing.T) {
	t.Parallel()
	out := retriever.DecodeSparse(nil)
	if len(out) != 0 {
		t.Errorf("nil input: %+v", out)
	}
}
