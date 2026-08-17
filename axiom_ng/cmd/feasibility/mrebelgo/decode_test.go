package main

import (
	"testing"

	"github.com/tggo/goSentencePiece"
)

func TestDecodeChunk0Beam(t *testing.T) {
	tok, err := sentencepiece.NewTokenizer("/tmp/mrebel_sp.bpe.model")
	if err != nil { t.Fatal(err) }
	addedTokens = map[int32]string{
		250054: "<triplet>", 250055: "<relation>", 250070: "<concept>",
		250058: "tp_XX", 250059: "<loc>", 250061: "<per>", 250064: "<org>",
	}
	ids := []int64{250058, 250054, 104260, 65646, 6, 250070, 137261, 6, 250070, 2831, 111, 2}
	got := decodeSeq(tok, ids)
	want := "tp_XX<triplet> Lieferanten <concept> Einkauf <concept> part of"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
