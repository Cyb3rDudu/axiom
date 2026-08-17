// mrebelgo — Go mREBEL Seq2Seq decoding loop (Restpunkt 6).
//
// BART encoder + autoregressive decoder with BEAM-SEARCH natively in Go via
// onnxruntime_go (CUDA on the carrier), replicating the Python oracle:
//   num_beams=3, num_return_sequences=3, max_length=256, length_penalty=0,
//   decoder_start_token_id=tp_XX, input max_length=512.
//
// Decoding uses the optimum-exported `decoder_model.onnx` (the with-past variant's
// first-step graph) for EVERY autoregressive step: each step feeds the beam's full
// decoder_input_ids so far plus encoder_hidden_states/encoder_attention_mask and takes
// the logits at the last position. This is the documented "volle Re-Encodierung pro
// Schritt" path — correct (validated: argmax matches torch at lengths 1-4) and simpler
// than threading the KV-cache, at the cost of O(L^2) per beam. The with_past model was
// exported and validated (decoder + decoder_with_past present on the carrier) but the
// Go cache-thread hit an onnxruntime_go dynamic-rank quirk; the no-cache path is the
// correctness-isolated fallback the task explicitly allowed (§4.2). Performance of this
// path is measured and reported honestly.
//
// Input tokenization (verified): goSentencePiece raw ids +1, prefix en_XX(250004),
// suffix </s>(2), truncate to 512 — byte-identical to MBart50TokenizerFast.
//
// Usage: mrebelgo <dylib> <modeldir> <chunks.json> <chunks.idx.json> <out.json>
//   chunks.idx.json = JSON array of chunk indices to process (matching pymrebel_ref schema:
//   each result has idx, raw_sequences, triples, deduped first-seen across beams).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

const (
	vocab   = 250071
	heads   = 16
	headDim = 64
	nLayers = 12
	eosID   = 2
	enXX    = 250004 // en_XX language prefix (mbart default src_lang)
	tpXX    = 250058 // tp_XX decoder_start_token_id
	maxEnc  = 512
	maxDec  = 256
	numBeams  = 3
	numReturn = 3
)

var addedTokens map[int32]string

type chunk struct {
	Doc  string `json:"doc"`
	Text string `json:"text"`
}
type triple struct {
	Head     string `json:"head"`
	HeadType string `json:"head_type"`
	Tail     string `json:"tail"`
	TailType string `json:"tail_type"`
	Relation string `json:"relation"`
}
type chunkResult struct {
	Idx          int      `json:"idx"`
	RawSequences []string `json:"raw_sequences"`
	Triples      []triple `json:"triples"`
}

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: mrebelgo <dylib> <modeldir> <chunks.json> <chunks.idx.json> <out.json>")
		os.Exit(2)
	}
	lib, mdir, chunksPath := os.Args[1], os.Args[2], os.Args[3]
	idxPath, outPath := os.Args[4], os.Args[5]

	raw, _ := os.ReadFile(chunksPath)
	var chunks []chunk
	json.Unmarshal(raw, &chunks)
	idxRaw, _ := os.ReadFile(idxPath)
	var idxs []int
	json.Unmarshal(idxRaw, &idxs)
	tok, err := sentencepiece.NewTokenizer(mdir + "/sentencepiece.bpe.model")
	if err != nil { fatal("tok: %v", err) }
	loadAddedTokens(mdir)
	traceInit()

	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil { fatal("ort: %v", err) }
	defer ort.DestroyEnvironment()
	opts := sessionOpts()

	encSess := newDyn(mdir+"/encoder_model.onnx",
		[]string{"input_ids", "attention_mask"}, []string{"last_hidden_state"}, opts)
	// Prefer the logits-only graph (onnx.utils trim: drops 48 unused present outputs,
	// which cost per-step tensor allocs + memory bandwidth in the no-cache loop).
	decModel := mdir + "/decoder_model.onnx"
	decIn := []string{"encoder_attention_mask", "input_ids", "encoder_hidden_states"}
	var decOut []string
	if fileExists(mdir + "/decoder_logits.onnx") {
		decModel = mdir + "/decoder_logits.onnx"
		decOut = []string{"logits"}
	} else {
		decOut = decOutputs()
	}
	decSess := newDyn(decModel, decIn, decOut, opts)
	var decKSess *ort.DynamicAdvancedSession
	if os.Getenv("MRBEL_CACHE") == "1" {
		decKSess = newDyn(mdir+"/decoder_with_past_model.onnx",
			[]string{"encoder_attention_mask", "input_ids",
				"past_key_values.0.decoder.key", "past_key_values.0.decoder.value", "past_key_values.0.encoder.key", "past_key_values.0.encoder.value",
				"past_key_values.1.decoder.key", "past_key_values.1.decoder.value", "past_key_values.1.encoder.key", "past_key_values.1.encoder.value",
				"past_key_values.2.decoder.key", "past_key_values.2.decoder.value", "past_key_values.2.encoder.key", "past_key_values.2.encoder.value",
				"past_key_values.3.decoder.key", "past_key_values.3.decoder.value", "past_key_values.3.encoder.key", "past_key_values.3.encoder.value",
				"past_key_values.4.decoder.key", "past_key_values.4.decoder.value", "past_key_values.4.encoder.key", "past_key_values.4.encoder.value",
				"past_key_values.5.decoder.key", "past_key_values.5.decoder.value", "past_key_values.5.encoder.key", "past_key_values.5.encoder.value",
				"past_key_values.6.decoder.key", "past_key_values.6.decoder.value", "past_key_values.6.encoder.key", "past_key_values.6.encoder.value",
				"past_key_values.7.decoder.key", "past_key_values.7.decoder.value", "past_key_values.7.encoder.key", "past_key_values.7.encoder.value",
				"past_key_values.8.decoder.key", "past_key_values.8.decoder.value", "past_key_values.8.encoder.key", "past_key_values.8.encoder.value",
				"past_key_values.9.decoder.key", "past_key_values.9.decoder.value", "past_key_values.9.encoder.key", "past_key_values.9.encoder.value",
				"past_key_values.10.decoder.key", "past_key_values.10.decoder.value", "past_key_values.10.encoder.key", "past_key_values.10.encoder.value",
				"past_key_values.11.decoder.key", "past_key_values.11.decoder.value", "past_key_values.11.encoder.key", "past_key_values.11.encoder.value"},
			decKOutputs(), opts)
	}

	results := make([]chunkResult, 0, len(idxs))
	for _, ci := range idxs {
		c := chunks[ci]
		if len(truncateRunes(c.Text, 1)) == 0 { continue }
		curChunk = ci
		start := time.Now()
		inputText := truncateRunes(c.Text, 1500) // Python slices by code points, NOT bytes
		goIDs, _ := tok.Encode(inputText)
		encIDs := []int64{enXX}
		for _, g := range goIDs { encIDs = append(encIDs, int64(g)+1) }
		encIDs = append(encIDs, eosID)
		if len(encIDs) > maxEnc { encIDs = encIDs[:maxEnc] }
		mask := make([]int64, len(encIDs))
		for i := range mask { mask[i] = 1 }
		encHidden := runEncoder(encSess, encIDs, mask)
		cd := &constDec{}
		cd.tm, _ = ort.NewTensor(ort.NewShape(1, int64(len(mask))), mask)
		cd.th, _ = ort.NewTensor(ort.NewShape(1, int64(len(mask)), 1024), encHidden)
		oneOut := len(decOut) == 1

		var seqs []seq
		if os.Getenv("MRBEL_CACHE") == "1" {
			seqs = beamSearchCached(tok, decSess, decKSess, encHidden, mask)
		} else {
			seqs = beamSearch(tok, decSess, cd, encHidden, mask, oneOut)
		}
		cd.Destroy()
		cr := chunkResult{Idx: ci}
		for _, s := range seqs {
			cr.RawSequences = append(cr.RawSequences, s.text)
			for _, t := range parseTriples(s.text) { cr.Triples = append(cr.Triples, t) }
		}
		cr.Triples = dedupTriples(cr.Triples)
		results = append(results, cr)
		fmt.Fprintf(os.Stderr, "chunk %d: %d seqs, %d triples, %s\n", ci, len(cr.RawSequences), len(cr.Triples), time.Since(start).Round(time.Millisecond))
	}
	b, _ := json.MarshalIndent(results, "", " ")
	os.WriteFile(outPath, b, 0o644)
	fmt.Printf("wrote %d chunk results -> %s\n", len(results), outPath)
}

func decOutputs() []string { // logits + per-layer dec.k,dec.v,enc.k,enc.v (we only read logits)
	n := []string{"logits"}
	for l := 0; l < nLayers; l++ {
		n = append(n, fmt.Sprintf("present.%d.decoder.key", l), fmt.Sprintf("present.%d.decoder.value", l))
		n = append(n, fmt.Sprintf("present.%d.encoder.key", l), fmt.Sprintf("present.%d.encoder.value", l))
	}
	return n
}

// decKOutputs: decoder_with_past outputs = logits + 24 present.decoder (per layer key,value).
func decKOutputs() []string {
	n := []string{"logits"}
	for l := 0; l < nLayers; l++ {
		n = append(n, fmt.Sprintf("present.%d.decoder.key", l), fmt.Sprintf("present.%d.decoder.value", l))
	}
	return n
}

func sessionOpts() *ort.SessionOptions {
	opts, err := ort.NewSessionOptions()
	if err != nil { fatal("opts: %v", err) }
	if os.Getenv("ORT_CUDA") != "1" { return opts }
	cuda, err := ort.NewCUDAProviderOptions()
	if err != nil { fatal("cuda: %v", err) }
	dev := os.Getenv("ORT_CUDA_DEVICE")
	if dev == "" { dev = "0" }
	if err := cuda.Update(map[string]string{"device_id": dev}); err != nil { fatal("cuda upd: %v", err) }
	defer cuda.Destroy()
	if err := opts.AppendExecutionProviderCUDA(cuda); err != nil { fatal("cuda ep: %v", err) }
	fmt.Fprintln(os.Stderr, "[mrebelgo] CUDA EP device", dev)
	return opts
}

func newDyn(path string, in, out []string, opts *ort.SessionOptions) *ort.DynamicAdvancedSession {
	s, err := ort.NewDynamicAdvancedSession(path, in, out, opts)
	if err != nil { fatal("session %s: %v", path, err) }
	return s
}

func runEncoder(s *ort.DynamicAdvancedSession, ids, mask []int64) []float32 {
	t0 := time.Now()
	n := int64(len(ids))
	tids, _ := ort.NewTensor(ort.NewShape(1, n), ids)
	defer tids.Destroy()
	tmask, _ := ort.NewTensor(ort.NewShape(1, n), mask)
	defer tmask.Destroy()
	o, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, n, 1024))
	defer o.Destroy()
	if err := s.Run([]ort.Value{tids, tmask}, []ort.Value{o}); err != nil { fatal("enc: %v", err) }
	traceT("enc", fmt.Sprintf("e%d", n), time.Since(t0))
	return o.GetData()
}

// constDec holds the per-chunk constant decoder inputs (enc_mask, enc_hidden tensors)
// so the no-cache loop reuses them instead of re-allocating per step.
type constDec struct {
	tm *ort.Tensor[int64]
	th *ort.Tensor[float32]
}

func (c *constDec) Destroy() {
	if c.tm != nil { c.tm.Destroy() }
	if c.th != nil { c.th.Destroy() }
}

// decodeStep feeds the full decoder token sequence to the (logits-only) decoder graph and
// returns the log-probs at the LAST position. Only tid + the logits output are allocated
// per call (enc_mask/enc_hidden tensors are reused via constDec).
func decodeStep(dec *ort.DynamicAdvancedSession, cd *constDec, ids []int64, oneOutput bool) ([]float32, error) {
	t0 := time.Now()
	L := int64(len(ids))
	tid, _ := ort.NewTensor(ort.NewShape(1, L), ids)
	defer tid.Destroy()
	logits, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, L, vocab))
	var outsV []ort.Value
	if oneOutput {
		outsV = []ort.Value{logits}
	} else {
		outs := decOutputs()
		outsV = make([]ort.Value, len(outs))
		outsV[0] = logits
		encLen := int64(len(cd.tm.GetData()))
		for i := 1; i < len(outs); i++ {
			var Lp []int64
			if strings.Contains(outs[i], "decoder") { Lp = []int64{1, 16, L, headDim} } else { Lp = []int64{1, 16, encLen, headDim} }
			t, _ := ort.NewEmptyTensor[float32](ort.NewShape(Lp...))
			outsV[i] = t
		}
	}
	if err := dec.Run([]ort.Value{cd.tm, tid, cd.th}, outsV); err != nil { return nil, err }
	traceT("dec", fmt.Sprintf("L%d", L), time.Since(t0))
	base := logits.GetData()
	last := base[(L-1)*vocab : L*vocab]
	return last, nil // raw logits; caller does fused topK-log-softmax
}

// truncateRunes keeps the first n UTF-8 runes (matches Python text[:n] code-point slicing).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n { return s }
	return string(r[:n])
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
