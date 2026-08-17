package main

import (
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

// Cached (KV-cache) decoding path using decoder_with_past_model.onnx.
// Graph topology (verified from the optimum 1.21 export):
//   decoder_model.onnx         : enc_mask, input_ids, enc_hidden -> logits + 48 present
//                                (present.{L}.{decoder,encoder}.{key,value}, per layer dec.k,dec.v,enc.k,enc.v)
//   decoder_with_past_model.onnx: enc_mask, input_ids, 48 past -> logits + 24 present.decoder
//   Cache threading: step1 present.encoder caches are the CONSTANT encoder past for stepN;
//                    stepN re-emits only decoder present (24) which feeds the next step.
//
// Returns the same [][]*beam as the no-cache path for the beam loop.
type hypC struct {
	ids   []int64
	score float64
	past  []*ort.Tensor[float32] // decoder cache (24 tensors), nil for the initial tp_XX beam
}

// step1Cached runs decoder_model.onnx for the first decoder token (tp_XX).
// Returns logits [1,1,vocab], 24 decoder-cache tensors, 24 constant encoder-cache tensors.
func step1Cached(dec1 *ort.DynamicAdvancedSession, encHidden []float32, encMask []int64, encLen int64) ([]float32, []*ort.Tensor[float32], []*ort.Tensor[float32]) {
	tm, _ := ort.NewTensor(ort.NewShape(1, encLen), encMask)
	defer tm.Destroy()
	tid, _ := ort.NewTensor(ort.NewShape(1, 1), []int64{tpXX})
	defer tid.Destroy()
	th, _ := ort.NewTensor(ort.NewShape(1, encLen, 1024), encHidden)
	defer th.Destroy()

	names := decOutputs() // logits + 48 present
	outs := make([]ort.Value, len(names))
	logits, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, 1, vocab))
	outs[0] = logits
	for i := 1; i < len(names); i++ {
		var L int64 = 1
		if contains(names[i], "encoder") { L = encLen }
		t, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, heads, L, headDim))
		outs[i] = t
	}
	if err := dec1.Run([]ort.Value{tm, tid, th}, outs); err != nil { fatal("step1Cached: %v", err) }
	decCache := make([]*ort.Tensor[float32], 0, 24)
	encCache := make([]*ort.Tensor[float32], 0, 24)
	for l := 0; l < nLayers; l++ {
		base := 1 + 4*l
		decCache = append(decCache, outs[base].(*ort.Tensor[float32]), outs[base+1].(*ort.Tensor[float32]))
		encCache = append(encCache, outs[base+2].(*ort.Tensor[float32]), outs[base+3].(*ort.Tensor[float32]))
	}
	return logits.GetData(), decCache, encCache
}

// stepNCached runs decoder_with_past_model.onnx for one new token.
// tm and encCache are pre-created (constant across steps); only tid is new per call.
func stepNCached(decK *ort.DynamicAdvancedSession, tokID int64,
	decCache, encCache []*ort.Tensor[float32], tm *ort.Tensor[int64], encLen, decLen int64) ([]float32, []*ort.Tensor[float32]) {
	tid, _ := ort.NewTensor(ort.NewShape(1, 1), []int64{tokID})
	feeds := []ort.Value{tm, tid}
	for l := 0; l < nLayers; l++ {
		feeds = append(feeds, decCache[2*l], decCache[2*l+1], encCache[2*l], encCache[2*l+1])
	}
	outNames := decKOutputs() // logits + 24 present.decoder
	outs := make([]ort.Value, len(outNames))
	logits, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, 1, vocab))
	outs[0] = logits
	for i := 1; i < len(outNames); i++ {
		t, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, heads, decLen+1, headDim))
		outs[i] = t
	}
	if err := decK.Run(feeds, outs); err != nil { fatal("stepNCached: %v", err) }
	tid.Destroy()
	newDec := make([]*ort.Tensor[float32], 0, 24)
	for l := 0; l < nLayers; l++ {
		newDec = append(newDec, outs[1+2*l].(*ort.Tensor[float32]), outs[2+2*l].(*ort.Tensor[float32]))
	}
	return logits.GetData(), newDec
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub { return true }
		}
		return false
	})()
}

// beamSearchCached: same beam semantics as beamSearch but using the KV-cache path.
// Cache invariant: a beam's `past` holds the decoder KV-cache for ids[:len-1] (all but the
// newest token); the newest token is the next stepN input. The first expansion uses step1's
// logits directly (the cache after step1 = KV of tp_XX, which is ids[:0] of the candidates).
func beamSearchCached(tok *sentencepiece.Tokenizer, dec1, decK *ort.DynamicAdvancedSession,
	encHidden []float32, encMask []int64) []seq {
	encLen := int64(len(encMask))
	logits1, decCache0, encCache := step1Cached(dec1, encHidden, encMask, encLen)
	tm, _ := ort.NewTensor(ort.NewShape(1, encLen), encMask) // constant across steps
	defer tm.Destroy()
	logP1 := logSoftmax(logits1)
	beams := []hypC{}
	for _, c := range topKIndices(logP1, 2*numBeams) {
		beams = append(beams, hypC{ids: []int64{tpXX, c.tok}, score: c.logp, past: decCache0})
	}
	done := []hyp{}
	decLen := int64(1) // cache length after step1 (KV of tp_XX)
	for !beamDoneC(done, beamToHypC(beams)) && len(beams) > 0 && decLen < int64(maxDec) {
		all := make([]hypC, 0, len(beams)*(2*numBeams))
		for _, b := range beams {
			lastTok := b.ids[len(b.ids)-1]
			rawLog, newCache := stepNCached(decK, lastTok, b.past, encCache, tm, encLen, decLen)
			logP := logSoftmax(rawLog)
			for _, c := range topKIndices(logP, 2*numBeams) {
				all = append(all, hypC{
					ids:   append(append([]int64{}, b.ids...), c.tok),
					score: b.score + c.logp,
					past:  newCache, // KV for all tokens of this beam now (incl. lastTok)
				})
			}
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
		next := []hypC{}
		for _, h := range all {
			if len(next) >= numBeams { break }
			if h.ids[len(h.ids)-1] == eosID {
				done = append(done, hyp{ids: h.ids, score: h.score})
				continue
			}
			next = append(next, h)
		}
		beams = next
		decLen++
	}
	for _, b := range beams {
		if len(done) >= numReturn { break }
		done = append(done, hyp{ids: b.ids, score: b.score})
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

func beamToHyp(done []hyp) []hyp { return done }
func beamToHypC(beams []hypC) []hyp {
	out := make([]hyp, len(beams))
	for i, b := range beams { out[i] = hyp{ids: b.ids, score: b.score} }
	return out
}
func beamDoneC(done, beams []hyp) bool {
	if len(done) < numBeams { return false }
	worst := done[len(done)-1].score
	bestOpen := math.Inf(-1)
	for _, b := range beams { if b.score > bestOpen { bestOpen = b.score } }
	return worst >= bestOpen
}

var _ = os.Getenv
var _ = fmt.Sprintf
