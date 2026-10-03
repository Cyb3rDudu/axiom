// mrtok — verify the mREBEL input-encoding rule reproduces MBart50TokenizerFast.
//
// Python (runner oracle): tok(text, max_length=512, padding, truncation) yields
//
//	[250004(en_XX), <token ids...>, </s>=2]
//
// with every content token id = raw-sentencepiece_id + 1 (HF reserves <pad>=1 in
// the mbart vocab, off-by-one vs the raw piece id). tggo/goSentencePiece gives the
// RAW piece ids (no prefix, no +1). Rule to reproduce Python ids from Go:
//
//	input_ids = [250004] + [go_id + 1 for go_id in go_ids] + [2]
//
// (go_ids already includes the trailing <unk>/`.`. There is NO <s>=0 prefix and NO
// </s> appended by Go — the <s> does not appear; </s> is added manually as 2.)
// This tool asserts the built-up ids equal the hard-coded Python reference for a
// few short German sentences.
package main

import (
	"fmt"
	"os"

	"github.com/tggo/goSentencePiece"
)

func main() {
	sp := "/models/mrebel_onnx/sentencepiece.bpe.model"
	tk, err := sentencepiece.NewTokenizer(sp)
	if err != nil {
		fatal("tok: %v", err)
	}

	refs := map[string][]int{
		"Teilung.": {250004, 16046, 1619, 5, 2},
		"Zelle.":   {250004, 567, 2118, 5, 2},
		"A.":       {250004, 62, 5, 2},
	}
	if len(os.Args) > 1 {
		t := os.Args[1]
		ids, _ := tk.Encode(t)
		fmt.Printf("MRTOK_ARG %q -> %v\n", t, ids)
		os.Exit(0)
	}
	allOK := true
	for text, want := range refs {
		got, _ := tk.Encode(text) // raw Go ids
		built := []int{250004}    // en_XX language prefix (mbart default)
		for _, id := range got {
			built = append(built, id+1)
		}
		built = append(built, 2) // </s>
		ok := equal(built, want)
		if !ok {
			allOK = false
		}
		fmt.Printf("%-12q\n  got   =%v\n  built =%v\n  want  =%v  MATCH=%v\n",
			text, got, built, want, ok)
	}
	if allOK {
		fmt.Println("MREBEL_INPUT_ENCODING_MATCH: ALL OK")
	} else {
		fmt.Println("MREBEL_INPUT_ENCODING_MATCH: MISMATCH")
		os.Exit(1)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func fatal(f string, a ...any) { fmt.Fprintf(os.Stderr, "FATAL: "+f+"\n", a...); os.Exit(1) }
