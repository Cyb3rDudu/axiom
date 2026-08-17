package main

import (
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

// hyp is one beam hypothesis (token sequence + cumulative log-prob score).
type hyp struct {
	ids   []int64
	score float64
}

// textSeq holds one decoded beam.
type seq struct {
	ids  []int64
	text string
	// score stored for beam ordering (not needed after selection)
}

// beamSearch replicates transformers beam search (num_beams=3, num_return=3,
// length_penalty=0, do_sample=false, decoder_start=tp_XX) using the no-cache
// re-decode path. Each hypothesis is its own growing token sequence; a hypothesis
// finishing with EOS moves to `done`. Returns the top-3 complete sequences.
func beamSearch(tok *sentencepiece.Tokenizer, dec *ort.DynamicAdvancedSession, cd *constDec, encHidden []float32, encMask []int64, oneOut bool) []seq {
	beams := []hyp{{ids: []int64{tpXX}, score: 0}}
	done := []hyp{} // finished hypotheses, capped at numBeams, best-by-score retained (BeamHypotheses semantics)
	decLen := 0
	for !beamDone(done, beams) && len(beams) > 0 && decLen < maxDec {
		all := make([]hyp, 0, len(beams)*(2*numBeams))
		db := []dumpBeam{}
		for _, b := range beams {
			logP, err := decodeStep(dec, cd, b.ids, oneOut)
			if err != nil { fatal("decodeStep: %v", err) }
			t6 := topKIndices(logP, 2*numBeams)
			de := dumpBeam{Ids: append([]int64{}, b.ids...), Score: b.score}
			for _, c := range t6 { de.Top6 = append(de.Top6, [2]any{c.tok, c.logp}) }
			db = append(db, de)
			for _, c := range t6 {
				all = append(all, hyp{
					ids:   append(append([]int64{}, b.ids...), c.tok),
					score: b.score + c.logp,
				})
			}
		}
		dumpStep("nocache", decLen, db)
		sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
		// select the top numBeams candidates; EOS candidates -> done (with eviction),
		// non-EOS -> open beams for the next round.
		next := []hyp{}
		for _, h := range all {
			if len(next) >= numBeams { break } // only numBeams open beams survive
			if h.ids[len(h.ids)-1] == eosID {
				addHyp(&done, h)
				continue
			}
			next = append(next, h)
		}
		beams = next
		decLen++
	}
	// promote truncated (never-finished) best beams if fewer than numReturn finished
	for _, b := range beams {
		if len(done) >= numReturn { break }
		addHyp(&done, b)
	}
	sort.SliceStable(done, func(i, j int) bool { return done[i].score > done[j].score })
	if len(done) > numReturn { done = done[:numReturn] }
	if os.Getenv("MRBEL_DEBUG") == "1" {
		for i, d := range done { fmt.Printf("DONE[%d] len=%d ids=%v\n", i, len(d.ids), d.ids) }
	}

	seqs := make([]seq, 0, len(done))
	for _, d := range done {
		cut := d.ids
		for i, id := range cut { if id == eosID { cut = cut[:i+1]; break } }
		seqs = append(seqs, seq{ids: cut, text: decodeSeq(tok, cut)})
	}
	return seqs
}

func logSoftmax(logits []float32) []float64 {
	mx := logits[0]
	for _, v := range logits { if v > mx { mx = v } }
	sum := 0.0
	for _, v := range logits { sum += math.Exp(float64(v) - float64(mx)) }
	lse := float64(mx) + math.Log(sum)
	out := make([]float64, len(logits))
	for i, v := range logits { out[i] = float64(v) - lse }
	return out
}

type cand struct{ tok int64; logp float64 }

func topKIndices(logP []float64, k int) []cand {
	if k > len(logP) { k = len(logP) }
	idx := make([]int, len(logP))
	for i := range idx { idx[i] = i }
	sort.SliceStable(idx, func(i, j int) bool { return logP[idx[i]] > logP[idx[j]] })
	out := make([]cand, k)
	for i := 0; i < k; i++ { out[i] = cand{tok: int64(idx[i]), logp: logP[idx[i]]} }
	return out
}

// addHyp inserts h into *done keeping at most numBeams entries, always retaining the
// best-by-score (transformers BeamHypotheses semantics: worse finished hyps are evicted).
func addHyp(done *[]hyp, h hyp) {
	*done = append(*done, h)
	sort.SliceStable(*done, func(i, j int) bool { return (*done)[i].score > (*done)[j].score })
	if len(*done) > numBeams { *done = (*done)[:numBeams] }
}

// beamDone mirrors transformers BeamHypotheses.is_done (early_stopping=False):
// generation is done once >= numBeams hypotheses are finished AND the best open beam
// score cannot beat the worst finished hypothesis.
func beamDone(done, beams []hyp) bool {
	if len(done) < numBeams { return false }
	worst := done[len(done)-1].score // done sorted desc, so last = worst
	bestOpen := math.Inf(-1)
	for _, b := range beams { if b.score > bestOpen { bestOpen = b.score } }
	return worst >= bestOpen
}
