// GLiNER-ONNX Go forward via onnxruntime_go (CUDA EP) — #171 Nachzug point 4.
//
// Reads gliner_inputs.json (shapes/dtypes/offsets) + gliner_inputs.bin (raw
// int64/bool tensor data) produced by the Python side from gliner's
// _build_dummy_batch, runs the GLiNER ONNX `logits` output through
// onnxruntime_go (CUDA when ORT_CUDA=1), writes gliner_logits_go.bin
// (big-endian f32). A compare step reports Go-vs-Python logits parity.
//
// Usage: gogliner <dylib> <model.onnx> <inputs.json> <inputs.bin> <out.bin>
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"

	ort "github.com/yalue/onnxruntime_go"
)

type metaT struct {
	Names  []string         `json:"names"`
	Shapes map[string][]int `json:"shapes"`
	IsBool map[string]bool  `json:"bool"`
	Offset map[string]int   `json:"offset"`
}

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: gogliner <dylib> <model.onnx> <inputs.json> <inputs.bin> <out.bin>")
		os.Exit(2)
	}
	lib, model, metaPath, binPath, outBin := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]

	jraw, err := os.ReadFile(metaPath)
	if err != nil {
		fatal("read meta: %v", err)
	}
	var m metaT
	if err := json.Unmarshal(jraw, &m); err != nil {
		fatal("meta: %v", err)
	}
	raw, err := os.ReadFile(binPath)
	if err != nil {
		fatal("read bin: %v", err)
	}

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
	if os.Getenv("ORT_CUDA") == "1" {
		cuda, err := ort.NewCUDAProviderOptions()
		if err != nil {
			fatal("cudaopts: %v", err)
		}
		dev := os.Getenv("ORT_CUDA_DEVICE")
		if dev == "" {
			dev = "0"
		}
		_ = cuda.Update(map[string]string{"device_id": dev})
		defer cuda.Destroy()
		if err := opts.AppendExecutionProviderCUDA(cuda); err != nil {
			fatal("cuda ep: %v", err)
		}
		fmt.Fprintln(os.Stderr, "gogliner: using CUDA EP device", dev)
	}

	var inputs []ort.Value
	for _, name := range m.Names {
		shape := m.Shapes[name]
		n := total(shape)
		off := m.Offset[name]
		if m.IsBool[name] {
			bb := make([]bool, n)
			for i := 0; i < n; i++ {
				bb[i] = raw[off+i] != 0
			}
			t, er := ort.NewTensor(ort.NewShape(i64s(shape)...), bb)
			if er != nil {
				fatal("bool tensor %s: %v", name, er)
			}
			inputs = append(inputs, t)
			defer t.Destroy()
		} else {
			ids := make([]int64, n)
			for i := 0; i < n; i++ {
				ids[i] = int64(int64fromLE(raw[off+i*8:]))
			}
			t, er := ort.NewTensor(ort.NewShape(i64s(shape)...), ids)
			if er != nil {
				fatal("i64 tensor %s: %v", name, er)
			}
			inputs = append(inputs, t)
			defer t.Destroy()
		}
	}

	out, er := ort.NewEmptyTensor[float32](ort.NewShape(1, 5, 12, 3))
	if er != nil {
		fatal("out: %v", er)
	}
	defer out.Destroy()
	session, er := ort.NewAdvancedSession(model, m.Names, []string{"logits"}, inputs, []ort.Value{out}, opts)
	if er != nil {
		fatal("session: %v", er)
	}
	defer session.Destroy()
	if er := session.Run(); er != nil {
		fatal("run: %v", er)
	}

	d := out.GetData()
	f, _ := os.Create(outBin)
	defer f.Close()
	var tmp [4]byte
	for _, v := range d {
		binary.BigEndian.PutUint32(tmp[:], math.Float32bits(v))
		f.Write(tmp[:])
	}
	fmt.Printf("wrote %d logits to %s\n", len(d), outBin)
}

func int64fromLE(b []byte) int64 {
	return int64(uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56)
}
func i64s(s []int) []int64 {
	out := make([]int64, len(s))
	for i, x := range s {
		out[i] = int64(x)
	}
	return out
}
func total(s []int) int {
	n := 1
	for _, x := range s {
		n *= x
	}
	return n
}
func fatal(f string, a ...any) { fmt.Fprintf(os.Stderr, "FATAL: "+f+"\n", a...); os.Exit(1) }
