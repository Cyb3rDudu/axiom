package repo

// #198 item 5: the German query normalizer. The Go side (query) and the SQL
// side (stored forms) must implement the IDENTICAL spec — this unit test
// pins the spec; the IT (kg_read_quality_it_test.go) pins the SQL/Go
// equivalence end-to-end. Spec, in order:
//  1. lowercase
//  2. ß -> ss
//  3. strip everything outside [a-z0-9äöü] (hyphens, spaces, punctuation,
//     wildcards — % and _ die here, so no LIKE-escape is needed)
//  4. bilingual families: theory -> theorie, sustainability -> nachhaltigkeit
//  5. light plural stem: strip ONE trailing suffix from {en, er, e, s} when
//     the result stays >= 5 chars
// Both sides apply the same steps, so equivalently-normalized forms match.

import (
	"testing"
)

func TestNormalizeKGTerm(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Stakeholdertheorie", "stakeholdertheori"},             // stem e
		{"Stakeholder-Theorie", "stakeholdertheori"},            // hyphen strip + stem
		{"stakeholder theory", "stakeholdertheori"},             // space strip + family
		{"Stakeholder-Theory", "stakeholdertheori"},             // hyphen + family
		{"Wesentlichkeit", "wesentlichkeit"},                    // untouched
		{"Doppelte Wesentlichkeiten", "doppeltewesentlichkeit"}, // stem en
		{"ESG-Managementsystem", "esgmanagementsystem"},
		{"esg-managementsystemen", "esgmanagementsystem"}, // stored plural
		{"Nachhaltigkeit", "nachhaltigkeit"},
		{"Sustainability", "nachhaltigkeit"}, // family
		{"sustainability", "nachhaltigkeit"}, // family + no stem needed
		{"Straße", "strass"},                 // ß folding + stem e (both sides)
		{"ESG", "esg"},                       // too short to stem
		{"Ökonomie", "ökonomi"},              // umlaut kept, stem e
		{"\"Anführungen\" (und) – Gedankenstriche", "anführungenundgedankenstrich"},
		{"100%_wildcard", "100wildcard"}, // % and _ stripped — no LIKE injection
	}
	for _, c := range cases {
		if got := normalizeKGTerm(c.in); got != c.want {
			t.Errorf("normalizeKGTerm(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The stem never applies below 6 chars of input.
	if got := normalizeKGTerm("Haus"); got != "haus" {
		t.Errorf("Haus (4 chars) must NOT stem: got %q", got)
	}
}
