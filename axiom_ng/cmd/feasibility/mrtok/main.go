// mrtok — verify tggo/goSentencePiece reproduces the MBart50TokenizerFast
// input_ids for mREBEL (en_XX prefix + </s> suffix + subword ids), Restpunkt 6.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/tggo/goSentencePiece"
)

func main() {
	sp := "/models/mrebel_onnx/sentencepiece.bpe.model"
	var texts = []string{
		"Virchow entdeckte 1821 die Zelle und ihre Teilung.",
		"CSR-Bericht umfasst die Offenlegung nichtfinanzieller Informationen.",
	}
	for _, t := range texts {
		for _, wrap := range []string{"none", "mbart"} {
			tk, err := sentencepiece.NewTokenizer(sp)
			if err != nil { fatal("tok: %v", err) }
			var ids []int
			if wrap == "mbart" {
				// mbart: prepend language token en_XX id 250004, append </s> id 2? 
				// We must discover the actual en_XX id. Try "▁en_XX" encode then wrap.
				ids, _ = tk.Encode(t)
				// append </s>=2 (mbart eos), and the lang token handled separately
			} else {
				ids, _ = tk.Encode(t)
			}
			fmt.Printf("wrap=%s text=%q\n  ids=%v\n  first5=%v len=%d\n",
				wrap, t, ids, idsHeader(ids), len(ids))
		}
	}
	// does the vocab contain en_XX as a piece?
	tk, _ := sentencepiece.NewTokenizer(sp)
	pieces := []string{"▁en_XX", "en_XX", "<s>", "<pad>", "</s>", "<unk>", "▁Vir", "chow"}
	for _, p := range pieces {
		e, _ := tk.Encode(strings.ReplaceAll(p, "▁", ""))
		fmt.Printf("encode %-12q -> %v\n", p, e[:3])
	}
}
func idsHeader(ids []int) []int { if len(ids) > 5 { return ids[:5] }; return ids }
func fatal(f string, a ...any) { fmt.Fprintf(os.Stderr, "FATAL: "+f+"\n", a...); os.Exit(1) }
