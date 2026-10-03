// Block 3 dense-parity tool: BGE-M3 dense embeddings in Go via
// onnxruntime_go (CPU) on the pinned tggo/goSentencePiece tokenizer.
//
//  1. Loads parser tokenizer (sentencepiece.bpe.model) + BGE-M3 onnx/model.onnx.
//  2. Encodes each input text -> sentence_embedding (1024 fp32), one at a time,
//     through a DynamicAdvancedSession (fresh tensors per call).
//  3. Runs the WHOLE batch twice and asserts byte-equality of run1 vs run2
//     (determinism precondition, #171 Ziel 10).
//  4. Writes go_run1.bin / go_run2.bin (one 1024*4-byte big-endian f32 row per
//     chunk, in input order) for the Python-parity cosine step.
//
// Usage:
//
//	godense <libonnxruntime.dylib> <model.onnx> <sentencepiece.model> \
//	        <chunks.json> <outdir>
package main

import (
	"bytes"
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
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: godense <dylib> <model.onnx> <sp.model> <chunks.json> <outdir>")
		os.Exit(2)
	}
	lib, model, spModel := os.Args[1], os.Args[2], os.Args[3]
	chunksPath, outdir := os.Args[4], os.Args[5]
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		fatal("%v", err)
	}

	raw, err := os.ReadFile(chunksPath)
	if err != nil {
		fatal("read chunks: %v", err)
	}
	var chunks []chunk
	if err := json.Unmarshal(raw, &chunks); err != nil {
		fatal("json chunks: %v", err)
	}

	tok, err := sentencepiece.NewTokenizer(spModel)
	if err != nil {
		fatal("tokenizer: %v", err)
	}
	tok.WithPostProcessor(sentencepiece.BertStylePostProcessor(0, 2))

	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil {
		fatal("ort env: %v", err)
	}
	defer ort.DestroyEnvironment()

	sess, err := ort.NewDynamicAdvancedSession(model,
		[]string{"input_ids", "attention_mask"},
		[]string{"token_embeddings", "sentence_embedding"}, sessionOpts())
	if err != nil {
		fatal("session: %v", err)
	}
	defer sess.Destroy()

	encodeOne := func(text string) ([]float32, error) {
		enc := tok.EncodeWithOptions(text, true)
		ids := shift(enc.IDs)
		// Truncate to the SAME max_length the Python reference uses. The final
		// parity pass ran at maxLen=8192 (ingest regime — no truncation for the
		// realistic study chunks, all < 8k sentencepiece tokens). The first pass's 0.966 avg was caused by a truncation mismatch (Go full length vs Python
		// 512) — any future comparison must keep both sides at the same max_length.
		// #171 Ziel 4.
		const maxLen = 8192
		if len(ids) > maxLen {
			ids = ids[:maxLen]
		}
		n := int64(len(ids))
		shape := ort.NewShape(1, n)

		ids64 := make([]int64, n)
		for i, id := range ids {
			ids64[i] = int64(id)
		}
		inIDs, err := ort.NewTensor(shape, ids64)
		if err != nil {
			return nil, err
		}
		defer inIDs.Destroy()
		mask := make([]int64, n)
		for i := range mask {
			mask[i] = 1
		}
		inMask, err := ort.NewTensor(shape, mask)
		if err != nil {
			return nil, err
		}
		defer inMask.Destroy()

		outTok, err := ort.NewEmptyTensor[float32](ort.NewShape(1, n, 1024))
		if err != nil {
			return nil, err
		}
		defer outTok.Destroy()
		outSent, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1024))
		if err != nil {
			return nil, err
		}
		defer outSent.Destroy()

		if err := sess.Run([]ort.Value{inIDs, inMask}, []ort.Value{outTok, outSent}); err != nil {
			return nil, err
		}
		return outSent.GetData(), nil
	}

	run := func(label string) ([][]float32, error) {
		res := make([][]float32, 0, len(chunks))
		for i, c := range chunks {
			emb, err := encodeOne(c.Text)
			if err != nil {
				return nil, fmt.Errorf("encode #%d: %w", i, err)
			}
			res = append(res, emb)
		}
		fmt.Printf("%s: encoded %d chunks\n", label, len(res))
		return res, nil
	}

	run1, err := run("run1")
	if err != nil {
		fatal("%v", err)
	}
	run2, err := run("run2")
	if err != nil {
		fatal("%v", err)
	}

	b1 := pack(run1)
	b2 := pack(run2)
	identical := bytes.Equal(b1, b2)
	fmt.Printf("DETERMINISM (run1 vs run2 byte-equal): %v\n", identical)
	if !identical {
		for i := 0; i < len(b1) && i < len(b2); i++ {
			if b1[i] != b2[i] {
				fmt.Printf("  first byte diff at offset %d (%d vs %d)\n", i, b1[i], b2[i])
				break
			}
		}
	}
	writeFile(outdir+"/go_run1.bin", b1)
	writeFile(outdir+"/go_run2.bin", b2)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(run1)))
	writeFile(outdir+"/count.bin", hdr[:])
	fmt.Printf("wrote go_run1/2.bin (%d chunks x 1024 f32 = %d bytes)\n", len(run1), len(b1))
	if !identical {
		os.Exit(3)
	}
}

// HF <pad>@1 reindex: every normal piece id +1; <s>=0 </s>=2 unchanged.
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

func pack(rows [][]float32) []byte {
	buf := bytes.NewBuffer(nil)
	buf.Grow(len(rows) * 1024 * 4)
	var tmp [4]byte
	for _, r := range rows {
		for _, v := range r {
			binary.BigEndian.PutUint32(tmp[:], math.Float32bits(v))
			buf.Write(tmp[:])
		}
	}
	return buf.Bytes()
}

func writeFile(path string, b []byte) {
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fatal("write %s: %v", path, err)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", a...)
	os.Exit(1)
}

// sessionOpts returns SessionOptions with the CUDA EP appended when ORT_CUDA=1
// (device from ORT_CUDA_DEVICE, default 0); CPU otherwise. Carrier parity runs
// set ORT_CUDA=1 so Go uses the same 3090 as the Python reference.
func sessionOpts() *ort.SessionOptions {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		fatal("session opts: %v", err)
	}
	if os.Getenv("ORT_CUDA") != "1" {
		return opts
	}
	cuda, err := ort.NewCUDAProviderOptions()
	if err != nil {
		fatal("cuda opts: %v", err)
	}
	dev := os.Getenv("ORT_CUDA_DEVICE")
	if dev == "" {
		dev = "0"
	}
	if err := cuda.Update(map[string]string{"device_id": dev}); err != nil {
		fatal("cuda update: %v", err)
	}
	defer cuda.Destroy()
	if err := opts.AppendExecutionProviderCUDA(cuda); err != nil {
		fatal("cuda ep: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[godense] using CUDA EP device %s\n", dev)
	return opts
}
