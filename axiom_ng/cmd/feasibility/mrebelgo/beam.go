package main

import (
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

const (
	numBeams  = 3
	numReturn = 3
)

// beamSearch replicates transformers beam search:
//   num_beams=3, num_return_sequences=3, max_length=256, length_penalty=0
//   (score = sum of token log-probs, no length normalization), do_sample=false,
//   decoder_start_token_id=tp_XX.
// Each beam owns its decoder KV-cache (24 tensors); the encoder cache is constant.
// Survivor caches are deep-cloned so every beam is independent and temporaries free.
func beamSearchFull(tok *sentencepiece.Tokenizer, dec1, decK *ort.DynamicAdvancedSession,
	encHidden []float32, encMask []int64) ([][]int64, []string) {

	encLen := int64(len(encMask))
	_, present := step1(dec1, encHidden, encMask, encLen)
	pastDec0 := make([]*ort.Tensor[float32], 0, 24)
	pastEnc := make([]*ort.Tensor[float32], 0, 24)
	for l := 0; l < nLayers; l++ {
		base := 1 + 4*l
		pastDec0 = append(pastDec0, present[base], present[base+1])
		pastEnc = append(pastEnc, present[base+2], present[base+3])
	}
	// first-step expansion: step1 already decoded tp_XX; the cache holds tp_XX (len 1).
	// We need the first generated token: run the tp_XX logits from step1? step1 returned logits
	// for tp_XX only. Re-derive: we need logits for the first expanded position, so run stepN
	// once on tp_XX with the empty-ish decoder cache? Simpler: decLen starts at 1 and we treat
	// the beam's "last token" as tp_XX for the very first stepN. So initialize beams from [tp_XX]
	// with cache pastDec0 (len 1) and score 0; then expand tp_XX -> next token via stepN.
	if os.Getenv("MRBEL_DEBUG") == "1" {
		for i := 0; i < 4; i++ { fmt.Fprintf(os.Stderr, "  pastDec0[%d] shape=%v\n", i, pastDec0[i].GetShape()) }
		for i := 0; i < 4; i++ { fmt.Fprintf(os.Stderr, "  pastEnc[%d]  shape=%v\n", i, pastEnc[i].GetShape()) }
	}
	// EXPERIMENT: fresh zero tensors for initial cache (godec1-style) to isolate rank error
	zdec := func(L int64) []*ort.Tensor[float32] {
		out := make([]*ort.Tensor[float32], 24)
		for i := range out { out[i], _ = ort.NewTensor(ort.NewShape(1, heads, L, headDim), make([]float32, heads*int(L)*headDim)) }
		return out
	}
	pastDec0 = zdec(1)
	pastEnc = zdec(encLen)
	beams := []*beam{{ids: []int64{tpXX}, score: 0, past: pastDec0}}

	finished := make([]*beam, 0, numReturn)
	decLen := int64(1)
	step := 0
	for step = 0; step < maxDec && beams != nil; step++ {
		var live []*beam
		if step == 0 {
			live = []*beam{beams[0]}
		} else {
			live = beams
		}
		// expand live beams
		all := make([]*beam, 0, len(live)*(2*numBeams))
		for _, b := range live {
			lastTok := b.ids[len(b.ids)-1]
			newLog, newDec := stepN(decK, lastTok, b.past, pastEnc, encMask, encLen, decLen)
			logP := logSoftmax(newLog)
			for _, c := range topKIndices(logP, 2*numBeams) {
				// candidate shares b's newDec; deep-clone on survive.
				nb := &beam{ids: append(append([]int64{}, b.ids...), c.tok), score: b.score + c.logp, _cache: newDec}
				all = append(all, nb)
			}
		}
		// sort all by score desc
		sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
		// select numBeams survivors; EOS -> finished; deep-clone survivor caches
		next := make([]*beam, 0, numBeams)
		sel := 0
		for _, h := range all {
			if sel+len(finished) >= numBeams { break }
			if h.ids[len(h.ids)-1] == eosID {
				h.done = true
				finished = append(finished, h)
				sel++
				continue
			}
			h.past = deepCloneCache(h._cache)
			next = append(next, h)
			sel++
		}
		// free temporaries: the old beam caches + this step's newDec (unless cloned)
		for _, b := range live { freeCache(b.past) }
		freeCachesOf(all) // free _cache references not cloned (deepClone already copied; originals free)
		beams = next
		decLen++
		if len(finished) >= numBeams {
			// keep going to allow more complete beams? transformers stops when numBeams complete
			break
		}
	}
	// promote truncated best beams if needed
	for _, b := range beams {
		if len(finished) >= numReturn { break }
		finished = append(finished, b)
	}
	sort.SliceStable(finished, func(i, j int) bool { return finished[i].score > finished[j].score })
	if len(finished) > numReturn { finished = finished[:numReturn] }

	idsOut := make([][]int64, 0, len(finished))
	texts := make([]string, 0, len(finished))
	for _, f := range finished {
		cut := truncateAtEOS(f.ids)
		idsOut = append(idsOut, cut)
		texts = append(texts, decodeSeq(tok, cut))
	}
	return idsOut, texts
}

type beam struct {
	ids    []int64
	score  float64
	past   []*ort.Tensor[float32] // survivor-owned decoder cache
	_cache []*ort.Tensor[float32] // transient reference for a step
	done   bool
}

func truncateAtEOS(ids []int64) []int64 {
	for i, id := range ids { if id == eosID { return ids[:i+1] } }
	return ids
}

// beamSearchOutput runs beam search and returns the per-sequence id-lists + texts.
func beamSearchOutput(tok *sentencepiece.Tokenizer, dec1, decK *ort.DynamicAdvancedSession,
	encHidden []float32, encMask []int64) ([][]int64, []string) {
	return beamSearchFull(tok, dec1, decK, encHidden, encMask)
}

func beamSearch(tok *sentencepiece.Tokenizer, dec1, decK *ort.DynamicAdvancedSession,
	encHidden []float32, encMask []int64) []string {
	_, texts := beamSearchFull(tok, dec1, decK, encHidden, encMask)
	return texts
}

// deepCloneCache copies 24 tensors so a survivor owns an independent cache.
func deepCloneCache(src []*ort.Tensor[float32]) []*ort.Tensor[float32] {
	out := make([]*ort.Tensor[float32], len(src))
	for i, t := range src {
		if t == nil { continue }
		data := t.GetData()
		sh := t.GetShape()
		n, err := ort.NewTensor(sh, data)
		if err != nil { fatal("clone: %v", err) }
		out[i] = n
	}
	return out
}

func freeCache(c []*ort.Tensor[float32]) {
	for i := range c { if c[i] != nil { c[i].Destroy(); c[i] = nil } }
}

func freeCachesOf(bs []*beam) {
	seen := map[*ort.Tensor[float32]]bool{}
	for _, b := range bs {
		for _, t := range b._cache {
			if t != nil && !seen[t] { seen[t] = true }
		}
	}
	for t := range seen { t.Destroy() }
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

var _ = fmt.Sprintf
