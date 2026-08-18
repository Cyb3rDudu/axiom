// Block 5 R7-E2E Mini-Go Runner (#171, PFLICHT).
//
// A minimal processor exposing /v1/embed (dense) + /v1/rerank over the
// contract §7a JSON, backended by onnxruntime_go (BGE-M3 dense encoder +
// bge-reranker-v2-m3 cross-encoder), so cmd/retrieval-bench can be pointed at
// it via AXIOM_PROCESSOR_URL. Sparse is NOT served (Go sparse output
// extraction is blocked; Block 3) — the ±sparse bench rows are dropped.
//
// Usage: minirunner <dylib> <bge-m3-model.onnx> <sp.model> <reranker.onnx> <port>
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"

	"github.com/tggo/goSentencePiece"
	ort "github.com/yalue/onnxruntime_go"
)

const contractVersion = "1.0"

type embedReq struct {
	ContractVersion string   `json:"contract_version"`
	Texts           []string `json:"texts"`
	IncludeSparse   bool     `json:"include_sparse,omitempty"`
}
type embedResp struct {
	ContractVersion string        `json:"contract_version"`
	Model           string        `json:"model"`
	Dimensions      int           `json:"dimensions"`
	Embeddings      [][]float32   `json:"embeddings"`
	Sparse          []map[string]float64 `json:"sparse,omitempty"`
}
type rerankReq struct {
	ContractVersion string   `json:"contract_version"`
	Query           string   `json:"query"`
	Texts           []string `json:"texts"`
	TopN            int      `json:"top_n"`
}
type scoreEntry struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}
type rerankResp struct {
	ContractVersion string       `json:"contract_version"`
	Model           string       `json:"model"`
	Scores          []scoreEntry `json:"scores"`
}

type runner struct {
	tok      *sentencepiece.Tokenizer
	denseS   *ort.DynamicAdvancedSession
	rrkS     *ort.DynamicAdvancedSession
	denseMOD string
	rrkMOD   string
}

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: minirunner <dylib> <bge-m3.onnx> <sp.model> <reranker.onnx> <port>")
		os.Exit(2)
	}
	lib, denseModel, spm, rrkModel, port := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]

	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil { fatal("ort env: %v", err) }
	defer ort.DestroyEnvironment()

	tok, err := sentencepiece.NewTokenizer(spm)
	if err != nil { fatal("tok: %v", err) }
	// CRITICAL: must match godense — without the BertStyle post-processor,
	// EncodeWithOptions(..., true) adds NO <s></s>, so query ids lack the
	// special-token wrapper Python uses -> query embeddings diverge (cos~0.5)
	// and R7 dense retrieval collapses (Go P@5 0.080 vs Python 0.536).
	tok.WithPostProcessor(sentencepiece.BertStylePostProcessor(0, 2))

	so := sessionOpts()
	denseS, err := ort.NewDynamicAdvancedSession(denseModel,
		[]string{"input_ids", "attention_mask"},
		[]string{"token_embeddings", "sentence_embedding"}, so)
	if err != nil { fatal("dense session: %v", err) }
	defer denseS.Destroy()

	rrkS, err := ort.NewDynamicAdvancedSession(rrkModel,
		[]string{"input_ids", "attention_mask"},
		[]string{"logits"}, so)
	if err != nil { fatal("rerank session: %v", err) }
	defer rrkS.Destroy()

	r := &runner{tok: tok, denseS: denseS, rrkS: rrkS, denseMOD: "BAAI/bge-m3", rrkMOD: "BAAI/bge-reranker-v2-m3"}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embed", r.handleEmbed)
	mux.HandleFunc("/v1/rerank", r.handleRerank)
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	bind := os.Getenv("MINI_RUNNER_BIND")
	if bind == "" { bind = "127.0.0.1" }
	addr := bind + ":" + port
	log.Printf("mini-go-runner listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// dense-only; sparse would need the (blocked) Go sparse extraction.
func (r *runner) handleEmbed(w http.ResponseWriter, req *http.Request) {
	var body embedReq
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"code":"BAD_JSON"}`, 422); return
	}
	if len(body.Texts) == 0 {
		http.Error(w, `{"code":"QUERY_TEXTS_EMPTY"}`, 422); return
	}
	out := embedResp{ContractVersion: contractVersion, Model: r.denseMOD, Dimensions: 1024}
	for _, t := range body.Texts {
		emb, err := r.encodeDense(t)
		if err != nil {
			http.Error(w, `{"code":"EMBEDDING_SHAPE_MISMATCH","message":"`+err.Error()+`"}`, 500); return
		}
		out.Embeddings = append(out.Embeddings, emb)
	}
	if body.IncludeSparse {
		// Not supported in the mini runner (Block 3 extraction blocker).
		http.Error(w, `{"code":"SPARSE_UNSUPPORTED","message":"mini runner has no sparse arm"}`, 422); return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (r *runner) handleRerank(w http.ResponseWriter, req *http.Request) {
	var body rerankReq
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"code":"BAD_JSON"}`, 422); return
	}
	if body.Query == "" || len(body.Texts) == 0 {
		http.Error(w, `{"code":"RERANK_QUERY_EMPTY"}`, 422); return
	}
	topN := body.TopN
	if topN <= 0 { topN = len(body.Texts) }
	scores := make([]float64, len(body.Texts))
	for i := range body.Texts {
		logit, err := r.rerankPair(body.Query, body.Texts[i])
		if err != nil {
			http.Error(w, `{"code":"RERANK_SHAPE_MISMATCH","message":"`+err.Error()+`"}`, 500); return
		}
		scores[i] = 1.0 / (1.0 + math.Exp(-float64(logit)))
	}
		entries := make([]scoreEntry, len(scores))
	for i, s := range scores { entries[i] = scoreEntry{Index: i, Score: s} }
	sort.SliceStable(entries, func(a, b int) bool { return entries[a].Score > entries[b].Score })
	if topN < len(entries) { entries = entries[:topN] }
	resp := rerankResp{ContractVersion: contractVersion, Model: r.rrkMOD, Scores: entries}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// --- dense encode (identical to godense) ---
func (r *runner) encodeDense(text string) ([]float32, error) {
	enc := r.tok.EncodeWithOptions(text, true)
	ids := shift(enc.IDs)
	if len(ids) > 512 { ids = ids[:512] }
	n := int64(len(ids))
	shape := ort.NewShape(1, n)
	ids64 := make([]int64, n)
	for i, id := range ids { ids64[i] = int64(id) }
	inIDs, err := ort.NewTensor(shape, ids64); if err != nil { return nil, err }
	defer inIDs.Destroy()
	mask := make([]int64, n)
	for i := range mask { mask[i] = 1 }
	inMask, err := ort.NewTensor(shape, mask); if err != nil { return nil, err }
	defer inMask.Destroy()
	outSent, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1024)); if err != nil { return nil, err }
	defer outSent.Destroy()
	outTok, err := ort.NewEmptyTensor[float32](ort.NewShape(1, n, 1024)); if err != nil { return nil, err }
	defer outTok.Destroy()
	if err := r.denseS.Run([]ort.Value{inIDs, inMask}, []ort.Value{outTok, outSent}); err != nil { return nil, err }
	return outSent.GetData(), nil
}

func (r *runner) rerankPair(query, passage string) (float32, error) {
	q := r.tok.EncodeWithOptions(query, false).IDs
	p := r.tok.EncodeWithOptions(passage, false).IDs
	ids := []int{0}
	for _, id := range q { ids = append(ids, shifttok(id)) }
	ids = append(ids, 2, 2) // </s></s> — HF XLM-R pair separator
	for _, id := range p { ids = append(ids, shifttok(id)) }
	ids = append(ids, 2)
	if len(ids) > 512 { ids = ids[:512] }
	n := int64(len(ids))
	shape := ort.NewShape(1, n)
	ids64 := make([]int64, n)
	for i, id := range ids { ids64[i] = int64(id) }
	inIDs, err := ort.NewTensor(shape, ids64); if err != nil { return 0, err }
	defer inIDs.Destroy()
	mask := make([]int64, n)
	for i := range mask { mask[i] = 1 }
	inMask, err := ort.NewTensor(shape, mask); if err != nil { return 0, err }
	defer inMask.Destroy()
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1)); if err != nil { return 0, err }
	defer out.Destroy()
	if err := r.rrkS.Run([]ort.Value{inIDs, inMask}, []ort.Value{out}); err != nil { return 0, err }
	return out.GetData()[0], nil
}

func shift(ids []int) []int {
	out := append([]int(nil), ids...)
	for i, id := range out { if id <= 2 { continue }; out[i] = id + 1 }
	return out
}
func shifttok(id int) int { if id <= 2 { return id }; return id + 1 }

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
	log.Printf("mini-runner using CUDA EP device %s", dev)
	return opts
}


func fatal(format string, a ...any) {
	log.Printf("FATAL: "+format, a...); os.Exit(1)
}
