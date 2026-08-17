package main

import (
	"math"
	"sort"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

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
func beamSearch(tok *sentencepiece.Tokenizer, dec *ort.DynamicAdvancedSession, encHidden []float32, encMask []int64) []seq {
	// initial hypothesis starts with tp_XX (score 0)
	type hyp struct {
		ids   []int64
		score float64
	}
	beams := []hyp{{ids: []int64{tpXX}, score: 0}}
	done := []hyp{}
	decLen := 0 // number of generation steps taken
	for len(done) < numBeams && len(beams) > 0 && decLen < maxDec {
		// expand every live beam
		all := make([]hyp, 0, len(beams)*(2*numBeams))
		for _, b := range beams {
			logP, err := decodeStep(dec, b.ids, encHidden, encMask)
			if err != nil { fatal("decodeStep: %v", err) }
			for _, c := range topKIndices(logP, 2*numBeams) {
				all = append(all, hyp{
					ids:   append(append([]int64{}, b.ids...), c.tok),
					score: b.score + c.logp,
				})
			}
		}
		// sort by score desc
		sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
		// select numBeams; EOS -> done
		next := []hyp{}
		for _, h := range all {
			if len(next)+len(done) >= numBeams { break }
			if h.ids[len(h.ids)-1] == eosID {
				done = append(done, h)
				continue
			}
			next = append(next, h)
		}
		beams = next
		decLen++
	}
	// promote truncated beams if needed
	for _, b := range beams {
		if len(done) >= numReturn { break }
		done = append(done, b)
	}
	sort.SliceStable(done, func(i, j int) bool { return done[i].score > done[j].score })
	if len(done) > numReturn { done = done[:numReturn] }

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


