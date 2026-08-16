// onnxruntime_go probe — Block 2/3 foundation.
// Verifies yalue/onnxruntime_go can load a shared ORT dylib on this host and
// run the existing BGE-M3 model.onnx to produce a 1024-dim sentence embedding.
// Usage: go run . <libonnxruntime.dylib> <model.onnx> [input_ids...]
// If input_ids given, feeds them (plus an all-ones mask); else uses <s>..</s>.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: ortprobe <libonnxruntime.dylib> <model.onnx> [input_ids...]")
		os.Exit(2)
	}
	lib := os.Args[1]
	model := os.Args[2]
	ids := []int64{}
	args := os.Args[3:]
	for _, a := range args {
		if a == "|" {
			break
		}
		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad id:", a)
			os.Exit(2)
		}
		ids = append(ids, n)
	}
	if len(ids) == 0 {
		ids = []int64{0, 1, 2, 1, 2, 2} // toy: <s> a b </s> </s>
	}

	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil {
		fmt.Fprintf(os.Stderr, "InitEnvironment: %v\n", err)
		os.Exit(1)
	}
	defer ort.DestroyEnvironment()

	inputShape := ort.NewShape(int64(len(ids)), 1) // (seq, batch) -> transpose to (batch, seq)
	// model expects ['batch_size','sequence_length']; shape must be (1, seq).
	inputShape = ort.NewShape(1, int64(len(ids)))
	inIDs, err := ort.NewTensor(inputShape, ids)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tensor ids: %v\n", err)
		os.Exit(1)
	}
	defer inIDs.Destroy()
	mask := make([]int64, len(ids))
	for i := range mask {
		mask[i] = 1
	}
	inMask, err := ort.NewTensor(inputShape, mask)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tensor mask: %v\n", err)
		os.Exit(1)
	}
	defer inMask.Destroy()

	// token_embeddings output shape depends on seq len; use token_embeddings for generality
	tokShape := ort.NewShape(1, int64(len(ids)), 1024)
	outTok, err := ort.NewEmptyTensor[float32](tokShape)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tensor tok: %v\n", err)
		os.Exit(1)
	}
	defer outTok.Destroy()
	outSent, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1024))
	if err != nil {
		fmt.Fprintf(os.Stderr, "tensor sent: %v\n", err)
		os.Exit(1)
	}
	defer outSent.Destroy()

	t0 := time.Now()
	session, err := ort.NewAdvancedSession(model,
		[]string{"input_ids", "attention_mask"},
		[]string{"token_embeddings", "sentence_embedding"},
		[]ort.Value{inIDs, inMask},
		[]ort.Value{outTok, outSent},
		nil,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewSession: %v\n", err)
		os.Exit(1)
	}
	defer session.Destroy()
	loadTime := time.Since(t0)

	t0 = time.Now()
	if err := session.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Run: %v\n", err)
		os.Exit(1)
	}
	runTime := time.Since(t0)

	sent := outSent.GetData()
	fmt.Printf("load_time=%s\nrun_time=%s\n", loadTime, runTime)
	fmt.Printf("input_ids=%v\n", ids)
	fmt.Printf("token_embeddings shape=%v\n", outTok.GetShape())
	fmt.Printf("sentence_embedding dim=%d\n", len(sent))
	if len(sent) > 0 {
		fmt.Printf("sentence_embedding head=%v\n", sent[:6])
		fmt.Printf("sentence_embedding L2 norm=%.6f\n", l2(sent))
	}
}

func l2(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return sqrt(s)
}

func sqrt(x float64) float64 {
	// simple Newton
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 30; i++ {
		z = (z + x/z) / 2
	}
	return z
}
