package frontmatter

// #198 item 1: per-class frontmatter classification for the KG. The #160
// retrieval detector (search.IsFrontmatter) gates TOC/preface/references
// — byte-identical semantics now live HERE and search delegates. The KG
// gate adds three classes the retrieval filter never had: author lists,
// index/register back-matter, and chapter byline/title lines (the
// "named_after Fifka / discoverer Weber" edge class, extracted from
// `### heading` + `**24**` + author-name byline chunks).
//
// Every fixture below is a REAL production shape (axiom_db, 2026-08-20
// corpus probe over 76k KG evidence chunks) or a measured false-positive
// that must stay ungated. PRECISION FIRST: a false positive deletes real
// KG evidence.

import (
	"testing"
)

func TestClassifyTOC(t *testing.T) {
	cases := []struct {
		name, text string
	}{
		{"heading", "# **Inhaltsverzeichnis**"},
		{"heading en", "### Contents"},
		{"table rows", "| 3 | Titel des Kapitels<br>Max Mustermann | 23 |\n| 4 | Zweites Kapitel | 45 |\n| 5 | Drittes Kapitel<br>Noch ein Autor | 67 |\n| 6 | Viertes Kapitel | 89 |"},
		{"dot leader", "5.1.2 Zieldefinition …………… 148\n5.1.3 Maßnahmenkatalog …… 152\n5.2 Umsetzung ………………… 160\n5.3 Kontrolle ………………… 178"},
	}
	for _, c := range cases {
		if got := Classify(c.text); got != ClassTOC {
			t.Errorf("%s: want toc, got %q", c.name, got)
		}
		if !IsFrontmatter(c.text) {
			t.Errorf("%s: #160 retrieval semantics must gate TOC", c.name)
		}
	}
}

func TestClassifyBibliography(t *testing.T) {
	cases := []struct {
		name, text string
	}{
		{"heading", "# **Literaturverzeichnis**"},
		{"heading en", "## References"},
		{"dash list", "- Ackermann, C., Parsons, T.: Der Begriff \"Sozialsystem\" als theoretisches Instrument, in: Jensen, S. (Hrsg.) Talcott Parsons, Opladen 1976.\n- Ackermann, T.: Methoden zur Implementierung, St. Gallen 2003.\n- Bayer, H.: Konzepte der Steuerung, in: Zeitschrift, Vol. 3, pp. 12-25, Berlin 1999.\n- Meier, P.: Handbuch Organisation, 2. Aufl., Wiesbaden 2001.\n- Schulze, R. (Hrsg.): Managementforschung, Bd. 12, Wiesbaden 2002."},
	}
	for _, c := range cases {
		if got := Classify(c.text); got != ClassBibliography {
			t.Errorf("%s: want bibliography, got %q", c.name, got)
		}
		if !IsFrontmatter(c.text) {
			t.Errorf("%s: #160 retrieval semantics must gate bibliography", c.name)
		}
	}
}

func TestClassifyPreface(t *testing.T) {
	for _, h := range []string{"## **Geleitwort**", "# Vorwort", "### Preface\n\nBuy-outs aus diversifizierten Konzernen haben in j\u00fcngster Zeit an Bedeutung gewonnen."} {
		if got := Classify(h); got != ClassPreface {
			t.Errorf("want preface, got %q for %.30s", got, h)
		}
		if !IsFrontmatter(h) {
			t.Errorf("#160 retrieval semantics must gate preface: %.30s", h)
		}
	}
}

func TestClassifyAuthors(t *testing.T) {
	cases := []struct {
		name, text string
	}{
		{"verzeichnis", "# **Autorenverzeichnis**\n\nProfessor Dr. **Oliver Budzinski**, Technische Universität Ilmenau, Ilmenau\n\nSachgebiet: Geldpolitik und –theorie\n\nDr. **Peter Haric**, Leitbetriebe Austria Institut, Wien"},
		{"about the authors", "## About the Authors\n\nDr. Tiffany Cheng Han Leung is Assistant Professor in the Faulty of Business at the City University of Macau, China."},
		{"ünter die", "### Über die Autoren\n\nProf. Dr. Max Bergmann ist als Berater tätig."},
		// #198 rider: observed production shape keeping bio entities alive.
		{"über die herausgeber", "#### **Über die Herausgeber**\n\nProf. Dr. René Schmidpeter ist Professor für nachhaltige Entwicklung."},
		// #198 rider: heading-less title-page editor line — names + italic
		// trailing *Hrsg.* marker (title-page typography; prose never does this).
		{"hrsg title page", "Marco Englert Anabel Ternès *Hrsg.*"},
	}
	for _, c := range cases {
		if got := Classify(c.text); got != ClassAuthors {
			t.Errorf("%s: want authors, got %q", c.name, got)
		}
	}
	// Precision: a citation line with a PLAIN (Hrsg.) — bibliography prose,
	// not a title page — must stay ungated.
	if got := Classify("Müller, H. (Hrsg.): Handbuch Organisation, 2. Aufl., Wiesbaden 2001. Das Standardwerk der Organisationstheorie."); got != ClassNone {
		t.Errorf("citation (Hrsg.) must stay ungated, got %q", got)
	}
	if got := Classify("Marco Englert Anabel Ternès (Hrsg.) hat das Buch gemeinsam mit anderen herausgegeben und dabei viele Beiträge geschrieben."); got != ClassNone {
		t.Errorf("prose (Hrsg.) must stay ungated, got %q", got)
	}
}

func TestClassifyIndex(t *testing.T) {
	cases := []struct {
		name, text string
	}{
		{"heading de", "## **Sachregister**\n\nKalkulation 158\nKapazitätsbeanspruchung 169\nKapitalbindungszinsen 156\nKapitaleigner 36 f., 40\nKapitalerhöhung 158\nKapitalmarkt 41"},
		{"heading en", "### Index\n\nActivity, 52\nActivity chain, 52\nAdvantage of effectiveness, 11\nAdvantage of efficiency, 11, 36\nAnalytical–technical dimension, 66\nArena of change, 71"},
		{"shape no heading", "126, 133, 427 Just-in-time-Produktion 156 Kalkulation 158 Kapazitätsbeanspruchung 169 Kapitalbindungszinsen 156 Kapitaleigner 36 f., 40 Kapitalerhöhung 158"},
	}
	for _, c := range cases {
		if got := Classify(c.text); got != ClassIndex {
			t.Errorf("%s: want index, got %q", c.name, got)
		}
	}
}

func TestClassifyTitleLinesByline(t *testing.T) {
	// The exact production shape of the Fifka/Weber evidence chunk.
	cases := []struct{ name, text string }{
		{"weber byline", "### <span id=\"page-495-0\"></span>**Zur Wirkung und Nutzung nachhaltiger Marken und Siegel**\n\n**24**\n\nTorsten Weber"},
		{"ternes byline", "### <span id=\"page-108-0\"></span>**Nachhaltigkeit und Digitalisierung als Chance für Unternehmen**\n\n**4**\n\nAnabel Ternès"},
		{"multi author", "### <span id=\"page-210-0\"></span>**Denken und Handeln in Ökosystemen**\n\n**10**\n\nKlaus-Stephan Otto, Stefan Rösler und Tina Teucher"},
		{"subtitle+authors", "### <span id=\"page-422-0\"></span>**Psychologisches Nachhaltigkeitscoaching im Management**\n\n**20**\n\nWarum die Große Transformation stockt\n\nErik Müller-Schoppen und Roland Pfennig"},
	}
	for _, c := range cases {
		if got := Classify(c.text); got != ClassTitleLines {
			t.Errorf("%s: want title_lines, got %q", c.name, got)
		}
	}
}

// Precision: measured production false-positive classes must stay ungated —
// a body chunk with a numbered-subsection line ("1" NOT bold + real prose),
// heading-only mini-chunks, prose with years, normal tables.
func TestClassifyPrecisionNegatives(t *testing.T) {
	cases := []struct{ name, text string }{
		{"numbered subsection + prose (probe FP)", "### > **Herunterbrechen der Vision in Mitarbeiterziele**\n\n1\n\nUm die Vision zu verwirklichen, müssen alle Mitarbeiter und Führungskräfte die Vision nicht nur kennengelernt und verstanden haben, sondern auch wissen, welche konkreten Aufgaben von ihnen persönlich umgesetzt werden müssen."},
		{"heading only", "### **Ergebnis**\n\nDie ersten Rohvisionen liegen vor und können im nächsten Schritt zusammengeführt werden."},
		{"numbered heading chunk", "### 4 **Veränderungsprojekt**\n\nDer Auftrag für eine Veränderung wird in Organisationen meist im Rahmen eines Projekts umgesetzt."},
		{"body prose with year", "Die Studie von 2019 zeigt, dass nachhaltige Unternehmensführung im Zeitraum 2010–2011 um 40% wuchs. Allerdings bleibt die Kritik: Initiativen müssen messbar sein, sonst verpuffen sie."},
		{"normal list", "- Erstens muss die Strategie angepasst werden.\n- Zweitens sind die Prozesse zu verschlanken.\n- Drittens folgt die Schulung der Mitarbeitenden.\n- Viertens wird die Erfolgsmessung definiert.\n- Fünftens steht die Wiederholung an."},
		{"glossar heading (NOT register)", "## **Glossar**\n\nNachhaltigkeit bezeichnet eine Entwicklung, die den Bedürfnissen der Gegenwart entspricht."},
		// NOTE pre-existing #160 behavior (NOT introduced by #198): a data
		// table whose LAST column holds bare small integers ("| Jahr | Umsatz |"
		// with 120/140/160/180 values) matches the TOC table shape — #160's
		// corpus calibration accepted that; changing it would alter pinned
		// retrieval semantics. Flagged to the owner, not silently "fixed".
	}
	for _, c := range cases {
		if got := Classify(c.text); got != ClassNone {
			t.Errorf("%s: must stay ungated, got %q", c.name, got)
		}
		if IsFrontmatter(c.text) {
			t.Errorf("%s: #160 retrieval semantics must keep this", c.name)
		}
	}
}

// Retrieval compatibility: the classes authors/index/title_lines are KG-only
// gates — the #160 retrieval filter must NOT drop them from search results.
func TestRetrievalKeepsKGOnlyClasses(t *testing.T) {
	kgOnly := []string{
		"# **Autorenverzeichnis**\n\nProfessor Dr. Oliver Budzinski, TU Ilmenau",
		"### Index\n\nActivity, 52\nActivity chain, 52\nAdvantage, 11, 36",
		"### **Ein Kapitel**\n\n**4**\n\nAnabel Ternès",
	}
	for _, text := range kgOnly {
		if IsFrontmatter(text) {
			t.Errorf("retrieval filter must not gate KG-only classes: %.40s", text)
		}
	}
}
