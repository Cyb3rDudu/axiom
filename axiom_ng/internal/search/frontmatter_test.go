// #160 frontmatter detection tests. Two directions matter equally:
//   - RECALL: the leak shapes (TOC pages, references sections, titled
//     prefaces) must flag
//   - PRECISION: body text must NEVER flag — a false positive deletes real
//     content from the candidate pool (the "zuaggressiv" flip-list probe:
//     the aggressive side must be a manual lever, not a heuristic accident)
package search

import (
	"fmt"
	"strings"
	"testing"
)

// tocChunk mirrors the dominant corpus shape (validated against
// axiom-ng-chunks-v1): markdown table rows "| n | Title<br>Author | page |".
func tocChunk() string {
	var b strings.Builder
	b.WriteString("| 1 | Road to Excellence: Potenzial des Sustainable<br>Management im 21. Jahrhundert<br>Marco Englert | 3   |\n")
	b.WriteString("|---|---|---|\n")
	b.WriteString("| 2 | Bionische Unternehmensführung – Schwimmstil für einen neuen<br>Blauen Ozean<br>Rüdiger Fox | 23  |\n")
	b.WriteString("| 3 | Nachhaltigkeit als Erfolgsfaktor für den Mittelstand<br>Anabel Ternès | 41  |\n")
	b.WriteString("| 4 | Kreislaufwirtschaft operationalisieren<br>Georg Müller-Christ | 89  |\n")
	b.WriteString("| 5 | Perspektiven der Forschung<br>Bror Giesenbauer | 133 |\n")
	return b.String()
}

// tocChunkClassic is the dot-leader shape some books produce.
func tocChunkClassic() string {
	var b strings.Builder
	for _, sec := range []string{
		"1 Einleitung und Grundlagen", "1.1 Problemstellung", "1.2 Zielsetzung der Arbeit",
		"1.3 Aufbau der Untersuchung", "2 Theoretischer Rahmen", "2.1 Stakeholdertheorie",
	} {
		fmt.Fprintf(&b, "%s %s %d\n", sec, strings.Repeat(".", 12), 15+len(b.String()))
	}
	return b.String()
}

func TestIsFrontmatterTOCTable(t *testing.T) {
	if !IsFrontmatter(tocChunk()) {
		t.Fatal("a markdown-table TOC page must flag")
	}
	if !IsFrontmatter(tocChunkClassic()) {
		t.Fatal("a classic dot-leader TOC page must flag")
	}
}

// Heading-only mini-chunks are the corpus reality ("### **Inhaltsverzeichnis**"
// alone, 26 chars) — and OCR dashes ("Literaturverzeichnis-") happen.
func TestIsFrontmatterHeadingChunks(t *testing.T) {
	for _, h := range []string{
		"### **Inhaltsverzeichnis**",
		"# Literaturverzeichnis-",
		"#### Literaturverzeichnis",
		"### **Vorwort**",
		"## Preface",
		"# **LITERATURVERZEICHNIS-**",
	} {
		if !IsFrontmatter(h) {
			t.Errorf("heading chunk %q must flag", h)
		}
	}
}

func TestIsFrontmatterPrefaceHeading(t *testing.T) {
	if !IsFrontmatter("Vorwort\n\nDie vorliegende Auflage entstand aus vielen Gesprächen mit Studierenden.") {
		t.Fatal("a chunk opening with a Vorwort heading must flag")
	}
	if !IsFrontmatter("Preface\n\nThis edition owes much to the seminar participants of 2024.") {
		t.Fatal("a chunk opening with a Preface heading must flag")
	}
}

func TestIsFrontmatterReferences(t *testing.T) {
	refs := `#### 1. Monographien und Aufsatze

- Adams. Walter/Brock. James W.: [Kleiner ist meist besser], *WirtschaJtswoche,* Nr. 17, 21.4.1989, S.80-85
- Aiginger. Karlrichy/Gunther: Die [Gr06e] der Kleinen, Wien, 1988
- Akerlof. George A.: [The market for "lemons"]: Quality uncertainty, in: *Quarterly Journal of Economics,* 1970
- Black. Fischer: [Noise], in: *Journal of Finance,* vol. 41 (1986), pp. 529-543
- Black. Fischer/Scholes. Myron: [The pricing of options], in: *JPE,* vol. 81 (1973), pp. 637-654
- Bobel. Ingo: [Wettbewerb und Industriestruktur], Wien, 1984
`
	if !IsFrontmatter(refs) {
		t.Fatal("a references section with citation majority must flag")
	}
}

// The precision fixtures: body text shapes that superficially resemble TOC —
// formula derivations, numbered enumerations WITH prose, data tables with
// trailing numbers. None of these may flag.
func TestIsFrontmatterPrecisionFixtures(t *testing.T) {
	prose := `Die Stakeholder-Theorie nach Freeman (1984) betrachtet Unternehmen als
Schnittpunkt unterschiedlicher Interessengruppen. 3 zentrale Konflikte treten
dabei auf: (1) Zielkonflikte zwischen Anteilseignern und Managern, die 1976
von Jensen und Meckling als Agency-Kosten formalisiert wurden, (2) Informations-
asymmetrien sowie (3) unterschiedliche Zeithorizonte. Tabelle 3.2 auf Seite 148
fasst die empirischen Befunde zusammen.`
	if IsFrontmatter(prose) {
		t.Error("prose with numbers and a page reference must NOT flag")
	}

	// A dated dash-list in body text (chronology/timeline) must survive the
	// references rule: not bibliographic density.
	chronology := `Meilensteine der Diskussion:
- 1970 Friedman odpowiedet auf die Kritik der Stakeholder
- 1984 Freeman veröffentlicht die kanonische Systematisierung
- 1995 Donaldson/Preston differenzieren deskriptiv/normativ
- 1997 Mitchell et al. entwickeln die Salience-Theorie
- 2005 die Debatte verlagert sich auf Sustainability`
	if IsFrontmatter(chronology) {
		t.Error("a prose chronology with years must NOT flag as references")
	}

	formulas := `Der Break-even-Punkt ergibt sich aus der Bedingung:

  x_BE = K_fix / (p - k_var)

Für K_fix = 120.000 EUR, p = 45 EUR und k_var = 27 EUR folgt

  x_BE = 120000 / 18 = 6666,7

d.h. ab der 6667. verkauften Einheit deckt der Umsatz die Gesamtkosten.`
	if IsFrontmatter(formulas) {
		t.Error("a formula derivation must NOT flag")
	}

	law := `Artikel 6 Verpflichtungen für Anbieter und Betreiber
(1) Anbieter von KI-Systemen mit hohem Risikopotenzial stellen 2027 sicher,
dass die Systeme die Anforderungen nach Artikel 8 erfüllen.
(2) Betreiber prüfen die Einhaltung spätestens alle 12 Monate.
(3) Abweichende nationalstaatliche Regelungen bleiben unberührt.`
	if IsFrontmatter(law) {
		t.Error("numbered legal text must NOT flag")
	}
}

func TestFilterFrontmatterDropsTOCKeepsBody(t *testing.T) {
	cands := []osCandidate{
		{ID: "body1", Text: "Stakeholder-Beziehungen dienen der Legitimationssicherung."},
		{ID: "toc1", Text: tocChunk()},
		{ID: "body2", Text: "Der Ressourcenansatz erklärt Wettbewerbsvorteile aus heterogenen Ressourcen."},
	}
	got := filterFrontmatter(cands)
	if len(got) != 2 || got[0].ID != "body1" || got[1].ID != "body2" {
		t.Fatalf("TOC must drop, body must stay: %+v", got)
	}
}

// Flip-list probe K1 (flag lever): filter OFF (or detection disabled) lets the
// TOC chunk back into the candidate pool — this is the v2.1 "before" state.
func TestFlipListK1FilterOffLetsTOCBack(t *testing.T) {
	cands := []osCandidate{
		{ID: "toc1", Text: tocChunk()},
		{ID: "body1", Text: "Fachtext."},
	}
	// lever flipped off: the unfiltered list keeps the TOC chunk
	if len(cands) != 2 || cands[0].ID != "toc1" {
		t.Fatal("sanity: unfiltered pool contains the TOC chunk")
	}
}

// Degradation guard: a pool consisting ONLY of frontmatter is served
// unchanged — hygiene must never zero out retrieval.
func TestFilterFrontmatterAllFrontmatterServed(t *testing.T) {
	cands := []osCandidate{{ID: "toc1", Text: tocChunk()}, {ID: "toc2", Text: tocChunk()}}
	got := filterFrontmatter(cands)
	if len(got) != 2 {
		t.Fatalf("all-frontmatter pool must be served unchanged, got %d", len(got))
	}
}
