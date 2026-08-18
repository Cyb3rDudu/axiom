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

// Batched KV-cached beam search (Opt-5b): ONE decoder_with_past call per round for all
// beams ([B,1] token step + batch cache [B,16,L,64]) — Python generate()'s structure.
// Cache re-parenting: after beam selection, survivor i's next-round cache row is its
// PARENT's row in the current round's present tensors (gather via host copies, ~2-3 MB
// per round — negligible).
//
// with_past graph I/O (verified from the optimum 1.21 export):
//   inputs : enc_mask [B,E], input_ids [B,1], past_key_values.{L}.{decoder,encoder}.{key,value}
//            (24 dec [B,16,L,64] + 24 enc [B,16,E,64], per-layer interleaved d.k,d.v,e.k,e.v)
//   outputs: logits [B,1,vocab], present.{L}.decoder.{key,value} (24, [B,16,L+1,64])

type hypB struct {
	ids        []int64
	score      float64
	parentSlot int // row of this beam's KV in the CURRENT round's batch cache
}

// step1CachedB runs the FULL graph once per chunk (B=1) -> first logits + initial caches.
func step1CachedB(dec1 *ort.DynamicAdvancedSession, encHidden []float32, encMask []int64, encLen int64) ([]float32, []*ort.Tensor[float32], []*ort.Tensor[float32]) {
	t0 := time.Now()
	tm, _ := ort.NewTensor(ort.NewShape(1, encLen), encMask)
	defer tm.Destroy()
	tid, _ := ort.NewTensor(ort.NewShape(1, 1), []int64{tpXX})
	defer tid.Destroy()
	th, _ := ort.NewTensor(ort.NewShape(1, encLen, 1024), encHidden)
	defer th.Destroy()
	names := decOutputs()
	outs := make([]ort.Value, len(names))
	logits, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, 1, vocab))
	outs[0] = logits
	for i := 1; i < len(names); i++ {
		var L int64 = 1
		if contains(names[i], "encoder") { L = encLen }
		t, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, heads, L, headDim))
		outs[i] = t
	}
	if err := dec1.Run([]ort.Value{tm, tid, th}, outs); err != nil { fatal("step1CachedB: %v", err) }
	traceT("step1", fmt.Sprintf("e%d", encLen), time.Since(t0))
	dec := make([]*ort.Tensor[float32], 0, 24)
	enc := make([]*ort.Tensor[float32], 0, 24)
	for l := 0; l < nLayers; l++ {
		base := 1 + 4*l
		dec = append(dec, outs[base].(*ort.Tensor[float32]), outs[base+1].(*ort.Tensor[float32]))
		enc = append(enc, outs[base+2].(*ort.Tensor[float32]), outs[base+3].(*ort.Tensor[float32]))
	}
	return logits.GetData(), dec, enc
}

// replicateBatch builds a [B,...] tensor whose rows all copy row 0 of src (B=1 tensor).
func replicateBatch(src *ort.Tensor[float32], B int64) *ort.Tensor[float32] {
	sh := src.GetShape()          // [1, d1, d2, ...]
	rowLen := sh[1] * sh[2] * sh[3]
	out, _ := ort.NewEmptyTensor[float32](ort.NewShape(B, sh[1], sh[2], sh[3]))
	data := out.GetData()
	base := src.GetData()
	var i int64
	for ; i < B; i++ {
		copy(data[i*rowLen:(i+1)*rowLen], base[:rowLen])
	}
	return out
}

// gatherRows builds dst [B,...] taking row srcRow[from] per output row.
func gatherRows(dst *ort.Tensor[float32], src *ort.Tensor[float32], from []int) {
	sh := dst.GetShape()
	rowLen := sh[1] * sh[2] * sh[3]
	sd := dst.GetData()
	bd := src.GetData()
	for i, r := range from {
		copy(sd[int64(i)*rowLen:int64(i+1)*rowLen], bd[int64(r)*rowLen:int64(r+1)*rowLen])
	}
}

func beamSearchCached(tok *sentencepiece.Tokenizer, dec1, decK *ort.DynamicAdvancedSession,
	encHidden []float32, encMask []int64) []seq {
	encLen := int64(len(encMask))
	logits1, decCache0, encCache1 := step1CachedB(dec1, encHidden, encMask, encLen)

	// First expansion (loop semantics): EOS -> done w/ eviction, top numBeams non-EOS live.
	t6 := topKLogSoftmax(logits1, 2*numBeams)
	beams := []hypB{}
	done := []hyp{}
	for _, c := range t6 {
		if c.tok == eosID {
			addHyp(&done, hyp{ids: []int64{tpXX, eosID}, score: c.logp})
			continue
		}
		if len(beams) >= numBeams { break }
		beams = append(beams, hypB{ids: []int64{tpXX, c.tok}, score: c.logp, parentSlot: 0})
	}
	if len(beams) == 0 { return finishSeqs(tok, done) }

	// Batch-3 constant encoder cache + mask (rebuilt if B changes).
	tmB := func(B int) *ort.Tensor[int64] {
		m := make([]int64, 0, B*len(encMask))
		for i := 0; i < B; i++ { m = append(m, encMask...) }
		t, _ := ort.NewTensor(ort.NewShape(int64(B), encLen), m)
		return t
	}
	encCacheB := func(B int) []*ort.Tensor[float32] {
		out := make([]*ort.Tensor[float32], len(encCache1))
		for i, t := range encCache1 { out[i] = replicateBatch(t, int64(B)) }
		return out
	}

	// prevCache: the previous round's present tensors [B_prev,16,decLen,64]; parents index into it.
	B0 := len(beams)
	prevCache := make([]*ort.Tensor[float32], 24)
	for i := range prevCache { prevCache[i] = replicateBatch(decCache0[i], int64(B0)) }
	var curTm *ort.Tensor[int64]
	var curEnc []*ort.Tensor[float32]
	curB := -1
	setB := func(B int) {
		if B == curB { return }
		if curTm != nil { curTm.Destroy() }
		for _, t := range curEnc { if t != nil { t.Destroy() } }
		curTm = tmB(B)
		curEnc = encCacheB(B)
		curB = B
	}
	defer func() { curTm.Destroy(); for _, t := range curEnc { t.Destroy() } }()

	decLen := int64(1)
	outNames := decKOutputs()
	for !beamDoneC(done, beamToHypB(beams)) && len(beams) > 0 && decLen < int64(maxDec) {
		B := len(beams)
		setB(B)

		// gather parent rows -> batch dec cache [B,16,decLen,64]
		parents := make([]int, B)
		for i, b := range beams { parents[i] = b.parentSlot }
		decBatch := make([]*ort.Tensor[float32], 24)
		for i := 0; i < 24; i++ {
			t, _ := ort.NewEmptyTensor[float32](ort.NewShape(int64(B), heads, decLen, headDim))
			gatherRows(t, prevCache[i], parents)
			decBatch[i] = t
		}

		// run with_past: enc_mask, ids, 24 dec, 24 enc (per-layer interleaved)
		ids := make([]int64, B)
		for i, b := range beams { ids[i] = b.ids[len(b.ids)-1] }
		tid, _ := ort.NewTensor(ort.NewShape(int64(B), 1), ids)
		t0 := time.Now()
		feeds := []ort.Value{curTm, tid}
		for l := 0; l < nLayers; l++ {
			feeds = append(feeds, decBatch[2*l], decBatch[2*l+1], curEnc[2*l], curEnc[2*l+1])
		}
		outs := make([]ort.Value, len(outNames))
		logits, _ := ort.NewEmptyTensor[float32](ort.NewShape(int64(B), 1, vocab))
		outs[0] = logits
		for i := 1; i < len(outNames); i++ {
			t, _ := ort.NewEmptyTensor[float32](ort.NewShape(int64(B), heads, decLen+1, headDim))
			outs[i] = t
		}
		if err := decK.Run(feeds, outs); err != nil { fatal("cachedB run: %v", err) }
		traceT("stepN", fmt.Sprintf("B%dd%d", B, decLen), time.Since(t0))
		tid.Destroy()
		for _, t := range decBatch { t.Destroy() }

		// per-beam topK in parallel
		base := logits.GetData()
		cands := make([][]cand, B)
		var wg sync.WaitGroup
		for i := 0; i < B; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				cands[i] = topKLogSoftmax(base[int64(i)*vocab:int64(i+1)*vocab], 2*numBeams)
			}(i)
		}
		wg.Wait()

		// collect + select (BeamHypotheses semantics)
		type candH struct { h hyp; parent int }
		all := make([]candH, 0, B*(2*numBeams))
		for i, b := range beams {
			for _, c := range cands[i] {
				all = append(all, candH{hyp{ids: append(append([]int64{}, b.ids...), c.tok), score: b.score + c.logp}, i})
			}
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].h.score > all[j].h.score })
		next := []hypB{}
		for _, ch := range all {
			if len(next) >= numBeams { break }
			if ch.h.ids[len(ch.h.ids)-1] == eosID {
				addHyp(&done, ch.h)
				continue
			}
			next = append(next, hypB{ids: ch.h.ids, score: ch.h.score, parentSlot: ch.parent})
		}
		beams = next
		// rotate cache: present rows ARE the current beams' rows (index i)
		for _, t := range prevCache { t.Destroy() }
		for i := 0; i < 24; i++ { prevCache[i] = outs[1+i].(*ort.Tensor[float32]) }
		decLen++
	}
	return finishSeqs(tok, done, beamToHypB(beams))
}

func finishSeqs(tok *sentencepiece.Tokenizer, done []hyp, beams ...[]hyp) []seq {
	for _, bs := range beams {
		for _, b := range bs {
			if len(done) >= numReturn { break }
			addHyp(&done, hyp{ids: b.ids, score: b.score})
		}
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

func beamToHypB(beams []hypB) []hyp {
	out := make([]hyp, len(beams))
	for i, b := range beams { out[i] = hyp{ids: b.ids, score: b.score} }
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub { return true }
	}
	return false
}

func beamDoneC(done, beams []hyp) bool {
	if len(done) < numBeams { return false }
	worst := done[len(done)-1].score
	bestOpen := math.Inf(-1)
	for _, b := range beams { if b.score > bestOpen { bestOpen = b.score } }
	return worst >= bestOpen
}

var _ = os.Getenv
