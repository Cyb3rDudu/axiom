// Block 3 sparse-parity tool (BGE-M3 lexical weights in Go), v2.
//
// Uses the exported sparse_head.onnx (input_ids, attention_mask ->
// relu(sparse_linear(last_hidden)) per-token scalar weights [1,seq]), then
// max-scatters by input_id (special 0,1,2,3 zeroed). This avoids reading the
// large token_embeddings tensor directly; the sparse_head.onnx was validated in
// Python at overlap 1.0 / cosine 1.0 vs FlagEmbedding.
//
// Usage: gosparse <dylib> <sparse_head.onnx> <sp.model> <chunks.json> <out.json>
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

type chunk struct {
	Doc  string `json:"doc"`
	Text string `json:"text"`
}

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: gosparse <dylib> <sparse_head.onnx> <sp.model> <chunks.json> <out.json>")
		os.Exit(2)
	}
	lib, model, spm, chunksPath, outPath := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]

	raw, err := os.ReadFile(chunksPath)
	if err != nil { fatal("%v", err) }
	var chunks []chunk
	if err := json.Unmarshal(raw, &chunks); err != nil { fatal("%v", err) }

	tok, err := sentencepiece.NewTokenizer(spm)
	if err != nil { fatal("tok: %v", err) }
	// Auto-review fix: godense applies the BertStyle post-processor (adds <s></s>,
	// XLM-R specials) but gosparse did NOT — so every chunk fed sparse_head.onnx
	// ids WITHOUT specials while the Python reference used add_special_tokens=True.
	// Required for the next matched Go-vs-Python sparse re-run; the committed
	// sparse_go8192_cuda.json was produced WITHOUT this line (see 03-dense-parity.md).
	tok.WithPostProcessor(sentencepiece.BertStylePostProcessor(0, 2))

	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil { fatal("env: %v", err) }
	defer ort.DestroyEnvironment()
	sess, err := ort.NewDynamicAdvancedSession(model,
		[]string{"input_ids", "attention_mask"},
		[]string{"token_weights"}, sessionOpts())
	if err != nil { fatal("session: %v", err) }
	defer sess.Destroy()

	run := func() []map[string]any {
		results := make([]map[string]any, 0, len(chunks))
		for i, c := range chunks {
			enc := tok.EncodeWithOptions(c.Text, true)
			ids := shift(enc.IDs)
			if len(ids) > 8192 { ids = ids[:8192] }
			weights, err := tokenWeights(sess, ids)
			if err != nil { fatal("run #%d: %v", i, err) }
			sp := scatter(ids, weights)
			results = append(results, map[string]any{"i": i, "doc": c.Doc, "n_tok": len(sp), "sparse": sp})
		}
		return results
	}
	a := run()
	b2 := run()
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b2)
	det := string(ja) == string(jb)
	fmt.Printf("DETERMINISM (run1 vs run2 byte-equal): %v\n", det)
	if err := os.WriteFile(outPath, ja, 0o644); err != nil { fatal("%v", err) }
	fmt.Printf("wrote sparse for %d chunks -> %s\n", len(a), outPath)
}

func tokenWeights(sess *ort.DynamicAdvancedSession, ids []int) ([]float32, error) {
	n := int64(len(ids))
	shape := ort.NewShape(1, n)
	ids64 := make([]int64, n)
	for i, id := range ids { ids64[i] = int64(id) }
	inIDs, err := ort.NewTensor(shape, ids64)
	if err != nil { return nil, err }
	defer inIDs.Destroy()
	mask := make([]int64, n)
	for i := range mask { mask[i] = 1 }
	inMask, err := ort.NewTensor(shape, mask)
	if err != nil { return nil, err }
	defer inMask.Destroy()
	out, err := ort.NewEmptyTensor[float32](shape)
	if err != nil { return nil, err }
	defer out.Destroy()
	if err := sess.Run([]ort.Value{inIDs, inMask}, []ort.Value{out}); err != nil { return nil, err }
	return out.GetData(), nil
}

func scatter(ids []int, weights []float32) map[string]float64 {
	best := make(map[int]float32)
	for p, vid := range ids {
		if vid <= 3 { continue }
		w := weights[p]
		if w > best[vid] { best[vid] = w }
	}
	out := make(map[string]float64, len(best))
	for vid, v := range best {
		out[itoa(vid)] = float64(v)
	}
	return out
}

func itoa(v int) string {
	if v == 0 { return "0" }
	neg := v < 0
	if neg { v = -v }
	var b [12]byte
	i := len(b)
	for v > 0 { i--; b[i] = byte('0' + v%10); v /= 10 }
	if neg { i--; b[i] = '-' }
	return string(b[i:])
}

func shift(ids []int) []int {
	out := append([]int(nil), ids...)
	for i, id := range out {
		if id <= 2 { continue }
		out[i] = id + 1
	}
	return out
}

// sessionOpts: CUDA EP when ORT_CUDA=1 (device ORT_CUDA_DEVICE), else CPU.
func sessionOpts() *ort.SessionOptions {
	opts, err := ort.NewSessionOptions()
	if err != nil { fatal("session opts: %v", err) }
	if os.Getenv("ORT_CUDA") != "1" { return opts }
	cuda, err := ort.NewCUDAProviderOptions()
	if err != nil { fatal("cuda opts: %v", err) }
	dev := os.Getenv("ORT_CUDA_DEVICE")
	if dev == "" { dev = "0" }
	if err := cuda.Update(map[string]string{"device_id": dev}); err != nil { fatal("cuda update: %v", err) }
	defer cuda.Destroy()
	if err := opts.AppendExecutionProviderCUDA(cuda); err != nil { fatal("cuda ep: %v", err) }
	fmt.Fprintf(os.Stderr, "[gosparse] using CUDA EP device %s\n", dev)
	return opts
}


func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", a...); os.Exit(1)
}
