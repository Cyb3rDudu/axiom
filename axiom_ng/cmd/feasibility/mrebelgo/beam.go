package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"

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
	// Batched beam expansion (Opt-3): all live beams share the same length per round,
	// so ONE decoder call [B, L] covers them all — Python generate()'s structure.
	// cdBatch3/1 hold the encoder tensors replicated to the batch rows.
	cd1 := cd
	cd3 := constDecBatch(encMask, encHidden, 3)
	defer cd3.Destroy()
	beams := []hyp{{ids: []int64{tpXX}, score: 0}}
	done := []hyp{} // finished hypotheses, capped at numBeams, best-by-score retained (BeamHypotheses semantics)
	decLen := 0
	for !beamDone(done, beams) && len(beams) > 0 && decLen < maxDec {
		all := make([]hyp, 0, len(beams)*(2*numBeams))
		db := []dumpBeam{}
		{
			seqs := make([][]int64, len(beams))
			for i, b := range beams { seqs[i] = b.ids }
			var useCD *constDec
			switch len(beams) {
			case 1:
				useCD = cd1
			case 3:
				useCD = cd3
			default: // defensive: rebuild for odd batch sizes
				useCD = constDecBatch(encMask, encHidden, len(beams))
			}
			t0 := time.Now()
			rows, err := decodeStepB(dec, useCD, seqs, oneOut)
			if err != nil { fatal("decodeStepB: %v", err) }
			traceT("dec", fmt.Sprintf("B%dL%d", len(beams), len(seqs[0])), time.Since(t0))
			// Opt-5a: the 3 per-beam 250k scans run in parallel (indexed writes -> deterministic,
			// identical arithmetic to the serial version).
			cands := make([][]cand, len(beams))
			var wg sync.WaitGroup
			for i := range beams {
				wg.Add(1)
				go func(i int) { defer wg.Done(); cands[i] = topKLogSoftmax(rows[i], 2*numBeams) }(i)
			}
			wg.Wait()
			for i, b := range beams {
				t6 := cands[i]
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

type cand struct{ tok int64; logp float64 }

// topKLogSoftmax returns the k highest-probability candidates with exact log-softmax
// scores — O(n) single scan for selection (raw logits are monotone in logprob, no sort!)
// plus one O(n) pass for the log-sum-exp. Replaces the old full sort of 250k indices,
// which was the actual per-chunk bottleneck (44 calls x ~60ms sort = ~2.6s/chunk).
// Ties keep the lower token index (strict > comparisons) — same order the previous
// stable sort produced.
func topKLogSoftmax(logits []float32, k int) []cand {
	const maxK = 8
	if k > maxK { k = maxK }
	var toks [maxK]int64
	var vals [maxK]float32
	n := 0
	mx := logits[0]
	for i, v := range logits {
		if v > mx { mx = v }
		if n < k {
			// insertion into descending buffer position i
			j := n
			for j > 0 && vals[j-1] < v {
				toks[j], vals[j] = toks[j-1], vals[j-1]
				j--
			}
			toks[j], vals[j] = int64(i), v
			n++
		} else if v > vals[k-1] {
			j := k - 1
			for j > 0 && vals[j-1] < v {
				toks[j], vals[j] = toks[j-1], vals[j-1]
				j--
			}
			toks[j], vals[j] = int64(i), v
		}
	}
	sum := 0.0
	for _, v := range logits { sum += math.Exp(float64(v) - float64(mx)) }
	lse := float64(mx) + math.Log(sum)
	out := make([]cand, n)
	for i := 0; i < n; i++ { out[i] = cand{tok: toks[i], logp: float64(vals[i]) - lse} }
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
