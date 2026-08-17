package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/tggo/goSentencePiece"
)

// loadAddedTokens reads the id->literal map for special tokens (added_tokens_map.json),
// so decode can render them inline like HF skip_special_tokens=False.
func loadAddedTokens(mdir string) {
	addedTokens = map[int32]string{}
	b, err := os.ReadFile(mdir + "/added_tokens_map.json")
	if err != nil { fatal("added map: %v", err) }
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil { fatal("added map json: %v", err) }
	for id, s := range m {
		var i int
		fmt.Sscanf(id, "%d", &i)
		addedTokens[int32(i)] = s
	}
}

// decodeSeq renders ids -> text, matching HF tokenizer.decode(skip_special_tokens=False):
// base-vocab runs are decoded by goSentencePiece.Decode (spaces on ▁); added/special ids are
// rendered as their literal strings inline (e.g. <triplet>, tp_XX, en_XX); <pad>(1) and </s>(2) omitted.
func decodeSeq(tok *sentencepiece.Tokenizer, ids []int64) string {
	var sb strings.Builder
	build := func() {}
	_ = build
	var baseRun []int
	flush := func() {
		if len(baseRun) == 0 { return }
		txt, err := tok.Decode(baseRun)
		if err == nil {
			sb.WriteString(cleanText(txt, sb.Len() > 0))
		}
		baseRun = baseRun[:0]
	}
	for i, id := range ids {
		if id == eosID && i > 0 { continue } // skip internal </s> (matches caller truncating at first eos)
		if str, ok := addedTokens[int32(id)]; ok {
			if id == 1 || id == 2 { continue } // <pad>, </s>
			flush()
			sb.WriteString(str)
			continue
		}
		if id < 0 || id >= vocab { continue }
		baseRun = append(baseRun, int(id))
	}
	flush()
	return normalizeHF(sb.String())
}

// goSentencePiece.Decode merges pieces with spaces (▁ -> space). HF's mbart decode also puts
// a space before pieces that follow a piece WITHOUT ▁ (pieces that wouldn't start a word). To
// stay close to HF, cleanText/normalizeHF reassemble: we rely on goSentencePiece.Decode which
// already handles the ▁->space rule the same way sentencepiece does (HF uses the same SPM decode).
func cleanText(s string, _ bool) string { return s }

// normalizeHF: HF clean_up_tokenization_spaces collapses runs of spaces. tokens already merged.
func normalizeHF(s string) string { return strings.Join(strings.Fields(s), " ") }

var tripleRe = regexp.MustCompile(`<triplet>\s*(.+?)\s*<(\w+)>\s*(.+?)\s*<(\w+)>\s*([^<]+)`)

// parseTriples mirrors _parse_mrebel_output (regex finditer); dedup done by caller.
func parseTriples(decoded string) []triple {
	s := strings.ReplaceAll(decoded, "</s>", "")
	s = strings.ReplaceAll(s, "<pad>", "")
	out := []triple{}
	for _, m := range tripleRe.FindAllStringSubmatch(s, -1) {
		head, htype := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		tail, ttype := strings.TrimSpace(m[3]), strings.TrimSpace(m[4])
		rel := strings.TrimSpace(m[5])
		if head != "" && tail != "" && rel != "" && len(head) >= 2 && len(tail) >= 2 {
			out = append(out, triple{Head: head, HeadType: mt(htype),
				Tail: tail, TailType: mt(ttype), Relation: rel})
		}
	}
	return out
}

// dedupTriples mirrors the runner: first-seen across beams by (head.lower, relation, tail.lower).
func dedupTriples(ts []triple) []triple {
	seen := map[string]bool{}
	out := []triple{}
	for _, t := range ts {
		key := strings.ToLower(t.Head) + "|" + t.Relation + "|" + strings.ToLower(t.Tail)
		if seen[key] { continue }
		seen[key] = true
		out = append(out, t)
	}
	return out
}

func mt(k string) string {
	switch strings.ToLower(k) {
	case "per": return "PERSON"
	case "org": return "ORGANIZATION"
	case "loc": return "LOCATION"
	case "media": return "WORK"
	default: return "CONCEPT"
	}
}

func fatal(f string, a ...any) { fmt.Fprintf(os.Stderr, "FATAL: "+f+"\n", a...); os.Exit(1) }
