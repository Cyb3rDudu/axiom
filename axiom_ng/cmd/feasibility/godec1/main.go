// godec1 — isolated Go test: run decoder_with_past_model.onnx with a single
// token + a 1-token decoder cache + matching encoder cache, on zero inputs,
// to verify onnxruntime_go can execute the with_past graph (Restpunkt 6).
// If ORT rejects rank-4 past tensors, this reproduces it in isolation.
//
// Usage: godec1 <libonnxruntime.so> <mrebel_onnx_dir> <encLen>
package main

import (
	"fmt"
	"os"

	ort "github.com/yalue/onnxruntime_go"
)

func main() {
	lib, mdir := os.Args[1], os.Args[2]
	var encLen int64 = 10
	if len(os.Args) > 3 { fmt.Sscanf(os.Args[3], "%d", &encLen) }
	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil { fatal("ort: %v", err) }
	defer ort.DestroyEnvironment()
	opts, _ := ort.NewSessionOptions()

	// input names in exact model order
	ins := []string{"encoder_attention_mask", "input_ids"}
	for l := 0; l < 12; l++ {
		ins = append(ins, fmt.Sprintf("past_key_values.%d.decoder.key", l))
		ins = append(ins, fmt.Sprintf("past_key_values.%d.decoder.value", l))
		ins = append(ins, fmt.Sprintf("past_key_values.%d.encoder.key", l))
		ins = append(ins, fmt.Sprintf("past_key_values.%d.encoder.value", l))
	}
	outs := []string{"logits"}
	for l := 0; l < 12; l++ {
		outs = append(outs, fmt.Sprintf("present.%d.decoder.key", l))
		outs = append(outs, fmt.Sprintf("present.%d.decoder.value", l))
	}
	// mirror mrebelgo: also open encoder + decoder_model sessions first
	encSess, err := ort.NewDynamicAdvancedSession(mdir+"/encoder_model.onnx",
		[]string{"input_ids", "attention_mask"}, []string{"last_hidden_state"}, opts)
	if err != nil { fatal("enc session: %v", err) }
	defer encSess.Destroy()
	dec1Sess, err := ort.NewDynamicAdvancedSession(mdir+"/decoder_model.onnx",
		[]string{"encoder_attention_mask", "input_ids", "encoder_hidden_states"},
		[]string{"logits"}, opts) // NOTE: only logits here
	if err != nil { fatal("dec1 session: %v", err) }
	defer dec1Sess.Destroy()
	// === actually run encoder + decoder_model (step1) first, mirroring mrebelgo ===
	_ = encLen
	encMask := ones(encLen)
	tem, _ := ort.NewTensor(ort.NewShape(1, encLen), encMask)
	defer tem.Destroy()
	tin, _ := ort.NewTensor(ort.NewShape(1, 1), []int64{250058})
	defer tin.Destroy()
	eh, err := ort.NewEmptyTensor[float32](ort.NewShape(1, encLen, 1024))
	if err != nil { fatal("eh: %v", err) }
	defer eh.Destroy()
	// encoder needs input_ids (not 250058, that's just a token) — use a fixed small id array
	tids, _ := ort.NewTensor(ort.NewShape(1, encLen), ones(encLen))
	defer tids.Destroy()
	if err := encSess.Run([]ort.Value{tids, tem}, []ort.Value{eh}); err != nil { fatal("enc run: %v", err) }
	// step1 decoder_model
	d1s := dec1Sess
	d1in0, _ := ort.NewTensor(ort.NewShape(1, encLen), encMask) // enc_mask
	defer d1in0.Destroy()
	d1in1, _ := ort.NewTensor(ort.NewShape(1, 1), []int64{250058}) // input_ids
	defer d1in1.Destroy()
	d1in2 := eh // enc_hidden
	logits1, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, 1, 250071))
	// decoder_model (step1) in THIS export outputs only logits, but the assembly might differ; try logits-only
	if err := dec1Sess.Run([]ort.Value{d1in0, d1in1, d1in2}, []ort.Value{logits1}); err != nil {
		fmt.Println("dec1 note (logits-only) err:", err)
	}
	_ = d1s
	sess, err := ort.NewDynamicAdvancedSession(mdir+"/decoder_with_past_model.onnx", ins, outs, opts)
	if err != nil { fatal("session: %v", err) }
	defer sess.Destroy()

	// feeds: enc_mask [1,encLen] int64 ones; input_ids [1,1] int64 {123};
	// past decoder k/v [1,16,1,64] zeros; past encoder k/v [1,16,encLen,64] zeros
	if _, err := ort.NewTensor[float32](ort.NewShape(1, 16, 1, 64), make([]float32, 16*1*64)); err != nil {
		fatal("tensor probe: %v", err)
	}
	tm, _ := ort.NewTensor(ort.NewShape(1, encLen), ones(encLen))
	defer tm.Destroy()
	tid, _ := ort.NewTensor(ort.NewShape(1, 1), []int64{250058})
	defer tid.Destroy()
	feeds := []ort.Value{tm, tid}
	for l := 0; l < 12; l++ {
		kdec, _ := ort.NewTensor(ort.NewShape(1, 16, 1, 64), make([]float32, 16*64))
		defer kdec.Destroy()
		vdec, _ := ort.NewTensor(ort.NewShape(1, 16, 1, 64), make([]float32, 16*64))
		defer vdec.Destroy()
		kenc, _ := ort.NewTensor(ort.NewShape(1, 16, encLen, 64), make([]float32, 16*int(encLen)*64))
		defer kenc.Destroy()
		venc, _ := ort.NewTensor(ort.NewShape(1, 16, encLen, 64), make([]float32, 16*int(encLen)*64))
		defer venc.Destroy()
		feeds = append(feeds, kdec, vdec, kenc, venc)
	}
	fmt.Println("feeds count:", len(feeds), "expected 50")
	logits, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, 1, 250071))
	outsV := []ort.Value{logits}
	for l := 0; l < 12; l++ {
		for k := 0; k < 2; k++ {
			t, _ := ort.NewEmptyTensor[float32](ort.NewShape(1, 16, 2, 64))
			outsV = append(outsV, t)
		}
	}
	fmt.Println("running with_past graph with rank-4 past tensors...")
	if err := sess.Run(feeds, outsV); err != nil {
		fatal("RUN ERROR: %v", err)
	}
	fmt.Println("RUN OK; logits[:3]=", logits.GetData()[:3])
}
func ones(n int64) []int64 { o := make([]int64, n); for i := range o { o[i] = 1 }; return o }
func fatal(f string, a ...any) { fmt.Fprintf(os.Stderr, "FATAL: "+f+"\n", a...); os.Exit(1) }
