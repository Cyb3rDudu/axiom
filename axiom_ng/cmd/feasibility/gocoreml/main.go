// Mac CoreML-EP probe (Restpunkt 5): verify onnxruntime_go CoreML EP runs the
// BGE-M3 encoder on the Mac GPU and produces the same sentence_embedding as
// the CUDA/CPU path (within fp variance).
//
// Usage: gocoreml <libonnxruntime.dylib> <model.onnx> <sp.model> <chunks.json>
// Encodes the first 3 chunks via the CoreML EP and prints L2 + head vs the
// committed CUDA reference (dense_cosine_cuda.csv chunk 0 cosine must be ~1.0).
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

type chunk struct {
	Doc  string `json:"doc"`
	Text string `json:"text"`
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: gocoreml <dylib> <model.onnx> <sp.model> <chunks.json>")
		os.Exit(2)
	}
	lib, model, spm, chunksPath := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	raw, err := os.ReadFile(chunksPath)
	if err != nil {
		fatal("read: %v", err)
	}
	var chunks []chunk
	if err := json.Unmarshal(raw, &chunks); err != nil {
		fatal("json: %v", err)
	}

	tok, err := sentencepiece.NewTokenizer(spm)
	if err != nil {
		fatal("tok: %v", err)
	}
	tok.WithPostProcessor(sentencepiece.BertStylePostProcessor(0, 2))

	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil {
		fatal("env: %v", err)
	}
	defer ort.DestroyEnvironment()

	opts, err := ort.NewSessionOptions()
	if err != nil {
		fatal("opts: %v", err)
	}
	defer opts.Destroy()
	if err := opts.AppendExecutionProviderCoreMLV2(map[string]string{}); err != nil {
		fatal("CoreML EP append err: %v", err)
	}
	fmt.Fprintln(os.Stderr, "gocoreml: CoreML EP appended")

	sess, err := ort.NewDynamicAdvancedSession(model,
		[]string{"input_ids", "attention_mask"},
		[]string{"token_embeddings", "sentence_embedding"}, opts)
	if err != nil {
		fatal("session: %v", err)
	}
	defer sess.Destroy()

	encodeOne := func(text string) ([]float32, error) {
		enc := tok.EncodeWithOptions(text, true)
		ids := shift(enc.IDs)
		if len(ids) > 8192 {
			ids = ids[:8192]
		}
		n := int64(len(ids))
		shape := ort.NewShape(1, n)
		ids64 := make([]int64, n)
		for i, id := range ids {
			ids64[i] = int64(id)
		}
		inIDs, er := ort.NewTensor(shape, ids64)
		if er != nil {
			return nil, er
		}
		defer inIDs.Destroy()
		mask := make([]int64, n)
		for i := range mask {
			mask[i] = 1
		}
		inMask, er := ort.NewTensor(shape, mask)
		if er != nil {
			return nil, er
		}
		defer inMask.Destroy()
		outSent, er := ort.NewEmptyTensor[float32](ort.NewShape(1, 1024))
		if er != nil {
			return nil, er
		}
		defer outSent.Destroy()
		outTok, er := ort.NewEmptyTensor[float32](ort.NewShape(1, n, 1024))
		if er != nil {
			return nil, er
		}
		defer outTok.Destroy()
		if er := sess.Run([]ort.Value{inIDs, inMask}, []ort.Value{outTok, outSent}); er != nil {
			return nil, er
		}
		return outSent.GetData(), nil
	}

	// Dense latency: warm + 20 short-query encodes (CoreML)
	short := "Was ist CSR-Reporting und welche Standards gibt es?"
	{
		en := tok.EncodeWithOptions(short, true)
		ids := shift(en.IDs)
		if len(ids) > 512 {
			ids = ids[:512]
		}
	}
	{
		_ = t0
	}
	// Run twice for determinism + encode first 3 chunks.
	lookup := func(i int) []float32 {
		t0 := time.Now()
		out, er := encodeOne(chunks[i].Text)
		if er != nil {
			fatal("encode: %v", er)
		}
		return out
		_ = t0
	}
	// dense latency: warmup + 10 short-query encodes
	warm := lookup(0)
	_ = warm
	q := "Was ist CSR-Reporting und welche Standards gibt es?"
	t0 := time.Now()
	for i := 0; i < 10; i++ {
		_ = lookup(0)
	}
	_ = t0
	run1 := lookup(0)
	run2, _ := encodeOne(chunks[0].Text)
	det := len(run1) == len(run2)
	for i := range run1 {
		if i >= len(run2) || math.Float32bits(run1[i]) != math.Float32bits(run2[i]) {
			det = false
			break
		}
	}
	fmt.Printf("DETERMINISM (coreml chunk0 run1 vs run2): %v\n", det)
	var s float64
	for _, x := range run1 {
		s += float64(x) * float64(x)
	}
	fmt.Printf("chunk0 sentence_embedding L2 norm=%.6f head=[%.5f %.5f %.5f]\n", math.Sqrt(s), run1[0], run1[1], run1[2])
	// write chunk 0-2 embeddings (big-endian f32) for cosine compare vs MPS ref
	for ci := 0; ci < 3; ci++ {
		emb := lookup(ci)
		out, _ := os.Create(fmt.Sprintf("/tmp/gd/gocoreml_chunk%d.bin", ci))
		var tmp [4]byte
		for _, x := range emb {
			binary.BigEndian.PutUint32(tmp[:], math.Float32bits(x))
			out.Write(tmp[:])
		}
		out.Close()
	}
}

func shift(ids []int) []int {
	out := append([]int(nil), ids...)
	for i, id := range out {
		if id <= 2 {
			continue
		}
		out[i] = id + 1
	}
	return out
}
func fatal(f string, a ...any) { fmt.Fprintf(os.Stderr, "FATAL: "+f+"\n", a...); os.Exit(1) }
