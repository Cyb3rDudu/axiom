// Tokenizer-pin PoC (#171, Block 2): reproduce BGE-M3 / XLM-R token IDs in Go
// via gomlx/tokenizers (Rust huggingface/tokenizers FFI — the same engine HF
// Fast tokenizers use), against the Python XLMRobertaTokenizerFast reference.
//
// The pin MUST hold on a German sample incl. umlauts + NFC/NFD before any
// model-comparison numbers are meaningful. It loads the HF `tokenizer.json`
// (serialized Rust tokenizer) directly, so tokenization is engine-identical to
// the Fast reference by construction; the measure is that we call it correctly
// and the German normalization (NFKC, case, umlaut BPE) survives.
//
// Usage: go run . <tokenizer.json> TEXT...
// Prints input_ids (with special tokens) + decoded pieces.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gomlx/tokenizers"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: toktool <tokenizer.json> TEXT...")
		os.Exit(2)
	}
	tokPath := os.Args[1]
	text := strings.Join(os.Args[2:], " ")

	t, err := tokenizers.FromFile(tokPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load tokenizer: %v\n", err)
		os.Exit(1)
	}
	defer t.Finalize()

	// For the pin we want the model input = single sequence with the
	// template special tokens (<s> ... </s>) as the post_processor defines.
	t.AddSpecialTokens(true)
	t.ReturnTokens(true)

	e, err := t.Encode(text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("input=%s\n", str(text))
	fmt.Printf("ids=[%s]\n", joinUint32(e.GetIDs()))
	fmt.Printf("tokens=[%s]\n", joinStrings(e.GetTokens()))
	fmt.Printf("vocab=%d n_ids=%d\n", t.VocabSize(), len(e.GetIDs()))
}

func joinUint32(ids []uint32) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = fmt.Sprint(id)
	}
	return strings.Join(s, ", ")
}

func joinStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

func str(s string) string { return fmt.Sprintf("%q", s) }
