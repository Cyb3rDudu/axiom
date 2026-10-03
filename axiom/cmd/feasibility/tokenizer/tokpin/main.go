// tokenizer-pin PoC (#171, Block 2) via tggo/goSentencePiece (pure Go,
// Unigram+Viterbi, byte-identical to reference C++/Python sentencepiece).
// Loads the BGE-M3 XLM-R sentencepiece.bpe.model and reproduces the HF
// XLMRobertaTokenizerFast token IDs on a German sample incl. umlauts + NFKC
// (NFC/NFD normalization case).
//
// XLM-R wrap: BERT-style post-processor with cls=<s>=0 sep=</s>=2, matching the
// Fast tokenizer's template <s> TEXT </s>.
// Usage: go run . <sentencepiece.bpe.model> TEXT...
package main

import (
	"fmt"
	"os"
	"strings"

	sp "github.com/tggo/goSentencePiece"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: toktool3 <sentencepiece.bpe.model> TEXT...")
		os.Exit(2)
	}
	model := os.Args[1]
	text := strings.Join(os.Args[2:], " ")

	tok, err := sp.NewTokenizer(model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load model: %v\n", err)
		os.Exit(1)
	}
	// XLM-R specials: <s>=0, </s>=2 (BOS/EOS).
	tok.WithPostProcessor(sp.BertStylePostProcessor(0, 2))

	enc := tok.EncodeWithOptions(text, true)
	// HF XLMRobertaTokenizerFast reindexes: <s>=0,<pad>=1,</s>=2, every normal
	// sentencepiece piece is +1 relative to the raw sentencepiece id (the fast
	// tokenizer inserts <pad> at 1, shifting the native piece index up by one).
	// Special (<s>,</s>) stay 0/2; <pad> is not emitted by encoding.
	ids := append([]int(nil), enc.IDs...)
	for i, id := range ids {
		if id <= 2 {
			continue // native specials <s>=0,</s>=2 stay
		}
		ids[i] = id + 1
	}
	fmt.Printf("input=%s\n", fmt.Sprintf("%q", text))
	fmt.Printf("ids=[%s]\n", joinInts(ids))
	fmt.Printf("pieces=[%s]\n", joinStrings(enc.Tokens))
	fmt.Printf("vocab=%d\n", tok.VocabSize())
}

func joinInts(ids []int) string {
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
