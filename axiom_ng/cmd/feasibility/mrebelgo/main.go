// mrebelgo — Go mREBEL Seq2Seq decoding loop (Restpunkt 6).
//
// BART encoder + autoregressive decoder loop with KV-cache + BEAM-SEARCH natively :
// in Go via onnxruntime_go (CUDA on the carrier), replicating the Python oracle
// (relation_extractor.extract_relations_from_chunks):
//   num_beams=3, num_return_sequences=3, max_length=256, length_penalty=0,
//   decoder_start_token_id=tp_XX, input max_length=512.
//
// Graph topology (verified from the optimum 1.21 export on the carrier):
//   encoder_model.onnx          : input_ids, attention_mask -> last_hidden_state
//   decoder_model.onnx (step1)  : enc_mask, input_ids, enc_hidden -> logits + 48 present
//   decoder_with_past (stepN)   : enc_mask, input_ids, 48 past -> logits + 24 present.decoder
//   Cache threading: step1 present.encoder caches are the CONSTANT encoder past for stepN;
//                    stepN re-emits only the decoder present (24) which feeds the next step.
//
// Input tokenization (verified off-by-one): goSentencePiece raw ids +1, prefix en_XX(250004),
// suffix </s>(2), truncate to 512 — byte-identical to MBart50TokenizerFast.
//
// Usage (greedy single chunk, warm-up): mrebelgo <dylib> <modeldir> <chunks.json> <idx> <out.txt>
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

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: mrebelgo <dylib> <modeldir> <chunks.json> <chunk_idx> <out.txt>")
		os.Exit(2)
	}
	lib, mdir, chunksPath, outPath := os.Args[1], os.Args[2], os.Args[3], os.Args[5]
	var ci int
	fmt.Sscanf(os.Args[4], "%d", &ci)

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
		[]string{"input_ids", "attention_mask"},
		[]string{"last_hidden_state"}, opts)
	dec1Sess := newDyn(mdir+"/decoder_model.onnx",
		dec1Inputs(), dec1Outputs(), opts)
	decKSess := newDyn(mdir+"/decoder_with_past_model.onnx",
		decKInputs(), decKOutputs(), opts)

	// encode chunk
	c := chunks[ci]
	inputText := c.Text
	if len(inputText) > 1500 { inputText = inputText[:1500] }
	goIDs, _ := tok.Encode(inputText)
	encIDs := []int64{enXX}
	for _, g := range goIDs { encIDs = append(encIDs, int64(g)+1) }
	encIDs = append(encIDs, eosID)
	if len(encIDs) > maxEnc { encIDs = encIDs[:maxEnc] }
	mask := make([]int64, len(encIDs))
	for i := range mask { mask[i] = 1 }
	encHidden := runEncoder(encSess, encIDs, mask)

	ids, seqs := beamSearchOutput(tok, dec1Sess, decKSess, encHidden, mask)
	_ = ids
	for i, s := range seqs {
		fmt.Printf("chunk %d seq[%d] ids=%v\n%s\n", ci, i, ids[i], s)
	}
	os.WriteFile(outPath, []byte(strings.Join(seqs, "\n========================\n")+"\n"), 0o644)
}

// --- graph name lists (exact ONNX order, verified) ---

func encInputs() []string { return []string{"input_ids", "attention_mask"} }

func dec1Inputs() []string { // encoder_attention_mask, input_ids, encoder_hidden_states
	return []string{"encoder_attention_mask", "input_ids", "encoder_hidden_states"}
}

func dec1Outputs() []string { // logits + per-layer dec.k,dec.v,enc.k,enc.v
	n := []string{"logits"}
	for l := 0; l < nLayers; l++ {
		n = append(n, fmt.Sprintf("present.%d.decoder.key", l))
		n = append(n, fmt.Sprintf("present.%d.decoder.value", l))
		n = append(n, fmt.Sprintf("present.%d.encoder.key", l))
		n = append(n, fmt.Sprintf("present.%d.encoder.value", l))
	}
	return n
}

func decKInputs() []string { // enc_mask, input_ids, then per-layer dec.k,dec.v,enc.k,enc.v
	n := []string{"encoder_attention_mask", "input_ids"}
	for l := 0; l < nLayers; l++ {
		n = append(n, fmt.Sprintf("past_key_values.%d.decoder.key", l))
		n = append(n, fmt.Sprintf("past_key_values.%d.decoder.value", l))
		n = append(n, fmt.Sprintf("past_key_values.%d.encoder.key", l))
		n = append(n, fmt.Sprintf("past_key_values.%d.encoder.value", l))
	}
	return n
}

func decKOutputs() []string { // logits + per-layer present.decoder.key,value
	n := []string{"logits"}
	for l := 0; l < nLayers; l++ {
		n = append(n, fmt.Sprintf("present.%d.decoder.key", l))
		n = append(n, fmt.Sprintf("present.%d.decoder.value", l))
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
	sh := ort.NewShape(1, n)
	tids, _ := ort.NewTensor(sh, ids)
	defer tids.Destroy()
	tmask, _ := ort.NewTensor(sh, mask)
	defer tmask.Destroy()
	o, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, n, 1024))
	defer o.Destroy()
	if err := s.Run([]ort.Value{tids, tmask}, []ort.Value{o}); err != nil { fatal("enc run: %v", err) }
	return o.GetData()
}

// --- decoder steps ---

// step1 runs decoder_model.onnx with the (single) decoder_start token.
// Returns logits [1,1,vocab] and the 48 present tensors in dec1Outputs order.
func step1(s *ort.DynamicAdvancedSession, encHidden []float32, encMask []int64, encLen int64) ([]float32, []*ort.Tensor[float32]) {
	// feed order = dec1Inputs: enc_mask, input_ids, enc_hidden
	msh := ort.NewShape(1, encLen)
	tm, _ := ort.NewTensor(msh, encMask)
	defer tm.Destroy()
	sh := ort.NewShape(1, 1)
	tid, _ := ort.NewTensor(sh, []int64{tpXX})
	defer tid.Destroy()
	esh := ort.NewShape(1, encLen, 1024)
	th, _ := ort.NewTensor(esh, encHidden)
	defer th.Destroy()

	names := dec1Outputs()
	outT := make([]*ort.Tensor[float32], len(names))
	outs := make([]ort.Value, len(names))
	logits, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1, vocab))
	if err != nil { fatal("step1 logits: %v", err) }
	outT[0] = logits; outs[0] = logits
	for i := 1; i < len(names); i++ {
		kind := names[i]
		var L int64
		if strings.Contains(kind, "decoder") { L = 1 } else { L = encLen }
		t, err := ort.NewEmptyTensor[float32](ort.NewShape(1, heads, L, headDim))
		if err != nil { fatal("step1 out: %v", err) }
		outT[i] = t; outs[i] = t
	}
	if err := s.Run([]ort.Value{tm, tid, th}, outs); err != nil { fatal("step1: %v", err) }
	return logits.GetData(), outT
}

// stepN runs decoder_with_past with the single new token + current decoder cache + constant encoder cache.
// pastDec: [dec.k0,dec.v0,enc.k0,enc.v0,...]? No — pastDec = decoder caches (24) in dec-k/v per-layer order;
// pastEnc = encoder caches (24, constant from step1) in enc-k/v per-layer order.
func stepN(s *ort.DynamicAdvancedSession, tokID int64,
	pastDec []*ort.Tensor[float32], pastEnc []*ort.Tensor[float32],
	encMask []int64, encLen, decLen int64) ([]float32, []*ort.Tensor[float32]) {
	// ---- godec1-identical implementation ----
	tm, _ := ort.NewTensor(ort.NewShape(1, encLen), encMask)
	tid, _ := ort.NewTensor(ort.NewShape(1, 1), []int64{tokID})
	feeds := []ort.Value{tm, tid}
	for l := 0; l < 12; l++ {
		feeds = append(feeds, pastDec[2*l], pastDec[2*l+1], pastEnc[2*l], pastEnc[2*l+1])
	}
	logits, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, 1, 250071))
	outsV := []ort.Value{logits}
	for l := 0; l < 12; l++ {
		for k := 0; k < 2; k++ {
			t, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, 16, int64(decLen)+1, 64))
			outsV = append(outsV, t)
		}
	}
	if err := s.Run(feeds, outsV); err != nil { fatal("stepN: %v", err) }
	tm.Destroy(); tid.Destroy()
	// repack outputs: logits + 24 present (per layer dec.k, dec.v)
	outT := make([]*ort.Tensor[float32], 0, 25)
	outT = append(outT, logits)
	for l := 0; l < 12; l++ {
		outT = append(outT, outsV[1+2*l].(*ort.Tensor[float32]), outsV[2+2*l].(*ort.Tensor[float32]))
	}
	return logits.GetData(), outT
}
