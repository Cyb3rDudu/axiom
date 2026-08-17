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
// Usage: mrebelgo <dylib> <modeldir> <chunks.json> <chunk_idx> <out.json>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
	RawSequences []string `json:"raw_sequences"`
	Triples      []triple `json:"triples"`
}

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: mrebelgo <dylib> <modeldir> <chunks.json> <chunk_idx> <out.json>")
		os.Exit(2)
	}
	lib, mdir, chunksPath := os.Args[1], os.Args[2], os.Args[3]
	var ci int
	fmt.Sscanf(os.Args[4], "%d", &ci)
	outPath := os.Args[5]

	raw, _ := os.ReadFile(chunksPath)
	var chunks []chunk
	json.Unmarshal(raw, &chunks)
	tok, err := sentencepiece.NewTokenizer(mdir + "/sentencepiece.bpe.model")
	if err != nil { fatal("tok: %v", err) }
	loadAddedTokens(mdir)

	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil { fatal("ort: %v", err) }
	defer ort.DestroyEnvironment()
	opts := sessionOpts()

	encSess := newDyn(mdir+"/encoder_model.onnx",
		[]string{"input_ids", "attention_mask"}, []string{"last_hidden_state"}, opts)
	// decoder_model (with-past-task graph): enc_mask, input_ids, enc_hidden -> logits + present
	decSess := newDyn(mdir+"/decoder_model.onnx",
		[]string{"encoder_attention_mask", "input_ids", "encoder_hidden_states"},
		decOutputs(), opts)

	// encode chunk
	c := chunks[ci]
	inputText := truncateRunes(c.Text, 1500) // Python slices by code points, NOT bytes
	goIDs, _ := tok.Encode(inputText)
	encIDs := []int64{enXX}
	for _, g := range goIDs { encIDs = append(encIDs, int64(g)+1) }
	encIDs = append(encIDs, eosID)
	if len(encIDs) > maxEnc { encIDs = encIDs[:maxEnc] }
	mask := make([]int64, len(encIDs))
	for i := range mask { mask[i] = 1 }
	encHidden := runEncoder(encSess, encIDs, mask)
	if os.Getenv("MRBEL_DUMP")=="1" {
		d, _ := json.Marshal(map[string]any{"enc_len": len(encIDs), "enc_ids": encIDs, "enc_hidden": encHidden, "text": inputText, "go_ids_n": len(goIDs)})
		os.WriteFile("/tmp/go_enc.json", d, 0o644)
		fmt.Fprintln(os.Stderr, "dumped /tmp/go_enc.json")
	}

	// beam search + decode + parse
	seqs := beamSearch(tok, decSess, encHidden, mask)
	cr := chunkResult{}
	for _, s := range seqs {
		cr.RawSequences = append(cr.RawSequences, s.text)
		for _, t := range parseTriples(s.text) { cr.Triples = append(cr.Triples, t) }
	}
	cr.Triples = dedupTriples(cr.Triples)
	b, _ := json.MarshalIndent(cr, "", " ")
	os.WriteFile(outPath, b, 0o644)
	fmt.Printf("chunk %d -> %d raw seqs, %d triples\n", ci, len(cr.RawSequences), len(cr.Triples))
	for _, s := range cr.RawSequences { fmt.Println("SEQ:", s) }
}

func decOutputs() []string { // logits + per-layer dec.k,dec.v,enc.k,enc.v (we only read logits)
	n := []string{"logits"}
	for l := 0; l < nLayers; l++ {
		n = append(n, fmt.Sprintf("present.%d.decoder.key", l), fmt.Sprintf("present.%d.decoder.value", l))
		n = append(n, fmt.Sprintf("present.%d.encoder.key", l), fmt.Sprintf("present.%d.encoder.value", l))
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
	n := int64(len(ids))
	tids, _ := ort.NewTensor(ort.NewShape(1, n), ids)
	defer tids.Destroy()
	tmask, _ := ort.NewTensor(ort.NewShape(1, n), mask)
	defer tmask.Destroy()
	o, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, n, 1024))
	defer o.Destroy()
	if err := s.Run([]ort.Value{tids, tmask}, []ort.Value{o}); err != nil { fatal("enc: %v", err) }
	return o.GetData()
}

// decodeStep feeds the full decoder token sequence to decoder_model.onnx and returns
// the log-probs at the LAST position (the next-token distribution).
func decodeStep(dec *ort.DynamicAdvancedSession, ids []int64, encHidden []float32, encMask []int64) ([]float64, error) {
	L := int64(len(ids))
	encLen := int64(len(encMask))
	tm, _ := ort.NewTensor(ort.NewShape(1, encLen), encMask)
	defer tm.Destroy()
	tid, _ := ort.NewTensor(ort.NewShape(1, L), ids)
	defer tid.Destroy()
	th, _ := ort.NewTensor(ort.NewShape(1, encLen, 1024), encHidden)
	defer th.Destroy()
	logits, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, L, vocab))
	// we only need logits; present outputs must still be provided (49 total). Allocate minimal.
	outs := decOutputs()
	outsV := make([]ort.Value, len(outs))
	outsV[0] = logits
	for i := 1; i < len(outs); i++ {
		var Lp []int64
		if strings.Contains(outs[i], "decoder") { Lp = []int64{1, 16, L, headDim} } else { Lp = []int64{1, 16, encLen, headDim} }
		t, _ := ort.NewEmptyTensor[float32](ort.NewShape(Lp...))
		outsV[i] = t
	}
	if err := dec.Run([]ort.Value{tm, tid, th}, outsV); err != nil { return nil, err }
	// last position logits -> log-softmax
	base := logits.GetData()
	last := base[(L-1)*vocab : L*vocab]
	return logSoftmax(last), nil
}

// truncateRunes keeps the first n UTF-8 runes (matches Python text[:n] code-point slicing).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n { return s }
	return string(r[:n])
}
