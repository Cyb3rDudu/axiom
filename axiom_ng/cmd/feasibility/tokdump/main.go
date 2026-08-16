// tokdump: write Go's truncated token IDs (the exact encoder input) for each
// chunk, to verify token-window parity vs Python independently of the ONNX
// numerics. Usage: tokdump <sp.model> <chunks.json> <out.json5>
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tggo/goSentencePiece"
)

type chunk struct {
	Doc  string `json:"doc"`
	Text string `json:"text"`
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: tokdump <sp.model> <chunks.json> <out.json5>")
		os.Exit(2)
	}
	spModel, chunksPath, out := os.Args[1], os.Args[2], os.Args[3]
	raw, err := os.ReadFile(chunksPath)
	if err != nil { fatal("%v", err) }
	var chunks []chunk
	if err := json.Unmarshal(raw, &chunks); err != nil { fatal("%v", err) }
	tok, err := sentencepiece.NewTokenizer(spModel)
	if err != nil { fatal("%v", err) }
	tok.WithPostProcessor(sentencepiece.BertStylePostProcessor(0, 2))

	outRecs := make([]map[string]any, len(chunks))
	for i, c := range chunks {
		enc := tok.EncodeWithOptions(c.Text, true)
		ids := shift(enc.IDs)
		if len(ids) > 512 { ids = ids[:512] }
		outRecs[i] = map[string]any{"i": i, "doc": c.Doc, "n": len(ids), "ids": ids}
	}
	b, _ := json.MarshalIndent(outRecs, "", " ")
	if err := os.WriteFile(out, b, 0o644); err != nil { fatal("%v", err) }
	fmt.Printf("wrote %d id-records to %s\n", len(outRecs), out)
}

func shift(ids []int) []int {
	out := append([]int(nil), ids...)
	for i, id := range out {
		if id <= 2 { continue }
		out[i] = id + 1
	}
	return out
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", a...); os.Exit(1)
}
