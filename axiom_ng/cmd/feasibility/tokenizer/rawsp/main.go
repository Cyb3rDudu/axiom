// Tokenizer-pin attempt via lwch/sentencepiece (pure-Go port) — Block 2.
// Loads the BGE-M3 XLM-R SentencePiece .model and encodes a German sample.
// Usage: go run . <sentencepiece.model> TEXT...
package main

import (
	"fmt"
	"os"
	"strings"

	sp "github.com/lwch/sentencepiece"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: toktool2 <sentencepiece.model> TEXT...")
		os.Exit(2)
	}
	modelPath := os.Args[1]
	text := strings.Join(os.Args[2:], " ")

	m, err := sp.Load(modelPath)
	if err != nil {
		// Load takes a dir; try LoadFrom with reader.
		f, ferr := os.Open(modelPath)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "open model: %v\n", ferr)
			os.Exit(1)
		}
		defer f.Close()
		m, err = sp.LoadFrom(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load model: %v\n", err)
			os.Exit(1)
		}
	}

	// XLMRobertaTokenizer body = SentencePiece on the text followed by wrap.
	// For the pin, also expose bos-dropped / raw.
	idsBOS := m.Encode(text, true, true)
	idsRaw := m.Encode(text, false, false)
	fmt.Printf("input=%s\n", fmt.Sprintf("%q", text))
	fmt.Printf("ids_with_bos_eos=[%s]\n", joinU64(idsBOS))
	fmt.Printf("ids_raw=[%s]\n", joinU64(idsRaw))
	fmt.Printf("bos=%d eos=%d count=%d\n", m.Bos(), m.Eos(), m.Count())
}

func joinU64(ids []uint64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = fmt.Sprint(id)
	}
	return strings.Join(s, ", ")
}
