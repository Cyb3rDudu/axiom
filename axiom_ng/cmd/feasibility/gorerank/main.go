// Block 3 rerank-parity tool: bge-reranker-v2-m3 cross-encoder scores in Go
// via onnxruntime_go (CPU) on the pinned tggo/goSentencePiece tokenizer.
//
// XLM-R pair form (matches HF / FlagReranker): [CLS] q [SEP] p [SEP]
//   = <s>(0) [tok(q)] </s>(2) [tok(p)] </s>(2), with the HF +1 reindex.
// Score = sigmoid(logits[0]) (the runner's compute_score(normalize=True)).
//
// Usage: gorerank <dylib> <reranker.model.onnx> <sp.model> <pairs.json> <out.json>
// out.json = [{"logit":x,"score":s,...}, ...] aligned with pairs.json order.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

type pair struct {
	Query   string `json:"query"`
	Doc     string `json:"doc"`
	Passage string `json:"passage"`
}

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: gorerank <dylib> <model.onnx> <sp.model> <pairs.json> <out.json>")
		os.Exit(2)
	}
	lib, model, spm := os.Args[1], os.Args[2], os.Args[3]
	pairsPath, outPath := os.Args[4], os.Args[5]

	raw, err := os.ReadFile(pairsPath)
	if err != nil { fatal("%v", err) }
	var pairs []pair
	if err := json.Unmarshal(raw, &pairs); err != nil { fatal("%v", err) }

	tok, err := sentencepiece.NewTokenizer(spm)
	if err != nil { fatal("tokenizer: %v", err) }

	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil { fatal("env: %v", err) }
	defer ort.DestroyEnvironment()

	sess, err := ort.NewDynamicAdvancedSession(model,
		[]string{"input_ids", "attention_mask"},
		[]string{"logits"}, nil)
	if err != nil { fatal("session: %v", err) }
	defer sess.Destroy()

	// deterministic 2x determinism for the whole batch
	var run1, run2 []float64
	run := func() []float64 {
		res := make([]float64, 0, len(pairs))
		for i, p := range pairs {
			q := encodeNoSpecials(tok, p.Query)
			pa := encodeNoSpecials(tok, p.Passage)
			ids := buildPair(q, pa) // [0]+q+[2]+pa+[2], reindexed
			// truncate combined to 512 (bge-reranker max_length)
			logit, err := runLogits(sess, ids[:min(512, len(ids))])
			if err != nil { fatal("run #%d: %v", i, err) }
			score := sigmoid(logit)
			res = append(res, score)
		}
		return res
	}
	run1 = run()
	run2 = run()

	det := true
	for i := range run1 {
		if bitsEq(run1[i], run2[i]) { /* byte-equal f64 via bits */ } else { det = false }
	}
	fmt.Printf("DETERMINISM (run1 vs run2): %v (n=%d)\n", det, len(run1))
	if !det { /* still write */ }

	out := make([]map[string]any, len(pairs))
	for i, p := range pairs {
		out[i] = map[string]any{"query": p.Query, "doc": p.Doc, "score": run1[i]}
	}
	b, _ := json.MarshalIndent(out, "", " ")
	if err := os.WriteFile(outPath, b, 0o644); err != nil { fatal("%v", err) }
	fmt.Printf("wrote %d rerank scores to %s\n", len(out), outPath)
	_ = time.Now
}

func encodeNoSpecials(tok *sentencepiece.Tokenizer, text string) []int {
	enc := tok.EncodeWithOptions(text, false)
	return enc.IDs
}

func buildPair(q, p []int) []int {
	ids := []int{0} // <s>
	for _, id := range q { ids = append(ids, shifttok(id)) }
	ids = append(ids, 2)
	for _, id := range p { ids = append(ids, shifttok(id)) }
	ids = append(ids, 2)
	return ids
}

func shifttok(id int) int {
	if id <= 2 { return id }
	return id + 1
}

func runLogits(sess *ort.DynamicAdvancedSession, ids []int) (float32, error) {
	n := int64(len(ids))
	shape := ort.NewShape(1, n)
	ids64 := make([]int64, n)
	for i, id := range ids { ids64[i] = int64(id) }
	inIDs, err := ort.NewTensor(shape, ids64)
	if err != nil { return 0, err }
	defer inIDs.Destroy()
	mask := make([]int64, n)
	for i := range mask { mask[i] = 1 }
	inMask, err := ort.NewTensor(shape, mask)
	if err != nil { return 0, err }
	defer inMask.Destroy()
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1))
	if err != nil { return 0, err }
	defer out.Destroy()
	if err := sess.Run([]ort.Value{inIDs, inMask}, []ort.Value{out}); err != nil { return 0, err }
	return out.GetData()[0], nil
}

func sigmoid(x float32) float64 { return 1.0 / (1.0 + math.Exp(float64(-x))) }

func bitsEq(a, b float64) bool { return math.Float64bits(a) == math.Float64bits(b) }

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", a...); os.Exit(1)
}
