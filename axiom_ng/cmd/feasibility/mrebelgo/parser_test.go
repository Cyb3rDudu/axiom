package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/tggo/goSentencePiece"
)

// Parser parity fixtures: real Python mREBEL raw outputs + parsed triples
// (from the carrier oracle run, mrebel_ref_50.json). The Go parser must produce
// the identical triple set (deduped across the 3 beams, first-seen order).
func TestParseTriplesFixtures(t *testing.T) {
	b, err := os.ReadFile("parser_fixtures.json")
	if err != nil { t.Fatal(err) }
	var fix map[string]struct {
		Raw     []string `json:"raw"`
		Triples []triple `json:"triples"`
	}
	if err := json.Unmarshal(b, &fix); err != nil { t.Fatal(err) }
	if len(fix) < 5 { t.Fatalf("need >=5 fixtures, got %d", len(fix)) }
	for idx, f := range fix {
		var got []triple
		for _, raw := range f.Raw {
			got = append(got, parseTriples(raw)...)
		}
		got = dedupTriples(got)
		want := f.Triples
		if len(got) != len(want) {
			t.Fatalf("chunk %s: got %d triples, want %d\n got=%v\nwant=%v", idx, len(got), len(want), got, want)
		}
		gs := map[string]bool{}
		for _, x := range got { gs[x.Head+"|"+x.Relation+"|"+x.Tail] = true }
		for _, x := range want {
			if !gs[x.Head+"|"+x.Relation+"|"+x.Tail] {
				t.Fatalf("chunk %s: missing triple %v", idx, x)
			}
		}
	}
}

// Decode parity: a real beam sequence must decode to the exact string Python produces
// (modulo </s>/<pad> padding, which the caller truncates).
func TestDecodeChunk0Beam(t *testing.T) {
	tok, err := sentencepiece.NewTokenizer("/tmp/mrebel_sp.bpe.model")
	if err != nil { t.Skip("sp model not local:", err) }
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
