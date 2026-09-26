package repair

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

func TestSchemaFilenameAuthor(t *testing.T) {
	got := SchemaFilename([]zoteroprovider.Creator{{LastName: "Horváth", FirstName: "Péter", CreatorType: "author"},
		{LastName: "Gleich", CreatorType: "author"}}, 2025, "Controlling")
	if got != "Horváth - 2025 - Controlling.pdf" {
		t.Fatalf("got %q", got)
	}
}
func TestSchemaFilenameInstitutional(t *testing.T) {
	// #291 flip: the colon used to survive into the filename (pinned
	// before) — the title cleanup now reads it as a separator. The
	// subtitle stays: the joined title fits the 80-rune budget.
	got := SchemaFilename([]zoteroprovider.Creator{{Name: "World Bank", CreatorType: "author"}}, 2026,
		"Global Economic Prospects, January 2026: Expand, Invest, Protect")
	if got != "World Bank - 2026 - Global Economic Prospects, January 2026 - Expand, Invest, Protect.pdf" {
		t.Fatalf("got %q", got)
	}
	long := SchemaFilename([]zoteroprovider.Creator{{LastName: "Müller", CreatorType: "author"}}, 2020,
		strings.Repeat("Sehr langer Buchtitel ", 8))
	if !filepath.IsLocal(long) || !strings.HasSuffix(long, "….pdf") {
		t.Fatalf("Kürzung: %q", long)
	}
}

func TestSchemaFilenameTitleCleanupSeparators(t *testing.T) {
	// #291 DoD: ':' and '/' read as ' - ' in the title component. The
	// old pinned colon variant is deliberately flipped (see the
	// institutional test above).
	got := SchemaFilename([]zoteroprovider.Creator{{LastName: "Autor", CreatorType: "author"}}, 2024,
		"Controlling: Instrumente/Praxis")
	if got != "Autor - 2024 - Controlling - Instrumente - Praxis.pdf" {
		t.Fatalf("got %q", got)
	}
}

func TestSchemaFilenameSubtitleBudget(t *testing.T) {
	// #291 DoD: the subtitle ships only when the joined title fits the
	// 80-rune budget — length decides (Bradford keeps it, Flew loses it).
	kept := SchemaFilename([]zoteroprovider.Creator{{LastName: "Bradford", CreatorType: "author"}}, 2017,
		"Wertorientierte Führung: Ein Handbuch")
	if kept != "Bradford - 2017 - Wertorientierte Führung - Ein Handbuch.pdf" {
		t.Fatalf("kurzer Untertitel muss bleiben: %q", kept)
	}
	longSub := strings.Repeat("Untertitelwort ", 10) // ~140 runes — over budget
	dropped := SchemaFilename([]zoteroprovider.Creator{{LastName: "Flew", CreatorType: "author"}}, 2012,
		"Understanding Media: "+longSub)
	if dropped != "Flew - 2012 - Understanding Media.pdf" {
		t.Fatalf("langer Untertitel muss fallen: %q", dropped)
	}
}

func TestSchemaFilenameGrownPatternSpringerPlus(t *testing.T) {
	// #291 DoD/reference: a document whose established attachment uses
	// the Springer '+'-encoding ('Dubs,+R.+-+2004+-+…') keeps it — the new
	// upload re-encodes the schema stem with '+' for every space. Local
	// consistency beats global uniformity.
	got := SchemaFilenameForFormat([]zoteroprovider.Creator{{LastName: "Dubs", CreatorType: "author"}}, 2024,
		"Managementlehre", "application/pdf",
		[]string{"Dubs,+R.+-+2004+-+Einfuehrung+in+die+Managementlehre.pdf"})
	if got != "Dubs+-+2024+-+Managementlehre.pdf" {
		t.Fatalf("got %q", got)
	}
}

func TestSchemaFilenameGrownPatternFormatMarker(t *testing.T) {
	// #291 DoD/reference: the '(EPUB)' suffix variant — a document with a
	// marker-suffixed attachment carries the marker over with the NEW
	// upload's own format tag. Without existing attachments the global
	// cascade applies (every other test in this file).
	got := SchemaFilenameForFormat([]zoteroprovider.Creator{{LastName: "Autor", CreatorType: "author"}}, 2020,
		"Titel", "application/pdf", []string{"Autor - 2020 - Titel (EPUB).epub"})
	if got != "Autor - 2020 - Titel (PDF).pdf" {
		t.Fatalf("got %q", got)
	}
	epub := SchemaFilenameForFormat([]zoteroprovider.Creator{{LastName: "Autor", CreatorType: "author"}}, 2020,
		"Titel", "application/epub+zip", []string{"Autor - 2020 - Titel.pdf"})
	if epub != "Autor - 2020 - Titel.epub" {
		t.Fatalf("plain grown name without marker/plus must not grow one: %q", epub)
	}
}

func TestSchemaFilenameGrownPatternNoFalsePositives(t *testing.T) {
	// #291 review: a '+' from the TITLE (C++/C#) or a parenthesized YEAR
	// in a space-separated reference name is NOT a grown pattern — the
	// global schema applies unchanged.
	cpp := SchemaFilenameForFormat([]zoteroprovider.Creator{{LastName: "Stroustrup", CreatorType: "author"}}, 2020,
		"C++ Programmierung: Grundlagen", "application/pdf",
		[]string{"Stroustrup - 2020 - C++ Programmierung - Grundlagen.pdf"})
	if cpp != "Stroustrup - 2020 - C++ Programmierung - Grundlagen.pdf" {
		t.Fatalf("C++ im Referenz-Titel darf kein +-Muster triggern: %q", cpp)
	}
	year := SchemaFilenameForFormat([]zoteroprovider.Creator{{LastName: "Autor", CreatorType: "author"}}, 2024,
		"Jahresbericht", "application/pdf",
		[]string{"Autor - 2024 - Jahresbericht (2024).pdf"})
	if year != "Autor - 2024 - Jahresbericht.pdf" {
		t.Fatalf("(2024) ist kein Format-Marker — kein Doppel-Suffix: %q", year)
	}
	mobi := SchemaFilenameForFormat([]zoteroprovider.Creator{{LastName: "Autor", CreatorType: "author"}}, 2020,
		"Titel", "application/pdf",
		[]string{"Autor - 2020 - Titel (MOBI).pdf"})
	if mobi != "Autor - 2020 - Titel.pdf" {
		t.Fatalf("(MOBI) ist kein bekannter Tag — kein Marker-Carry-Over: %q", mobi)
	}
}

func TestSchemaFilenameEmptySubtitleNoDanglingDash(t *testing.T) {
	// #291 review: 'Titel:' (empty subtitle) must not leave a dangling
	// ' - ' in the filename; ':' (empty main AND subtitle — the outer
	// join dangles entirely) must not leave 'Autor - 2024 -.pdf'.
	got := SchemaFilename([]zoteroprovider.Creator{{LastName: "Autor", CreatorType: "author"}}, 2024, "Titel:")
	if got != "Autor - 2024 - Titel.pdf" {
		t.Fatalf("got %q", got)
	}
	degenerate := SchemaFilename([]zoteroprovider.Creator{{LastName: "Autor", CreatorType: "author"}}, 2024, ":")
	if degenerate != "Autor - 2024.pdf" {
		t.Fatalf("degenerate title: got %q", degenerate)
	}
}

func TestSchemaFilenameNFCByteIdentity(t *testing.T) {
	// #291 DoD: macOS umlaut trap — NFD input (decomposed, as the platform
	// hands it over) must come out NFC, so the API filename and the on-disk
	// name are byte-identical (Go string comparison IS byte comparison).
	got := SchemaFilename([]zoteroprovider.Creator{{LastName: "Mu\u0308ller", CreatorType: "author"}}, 2020,
		"Gemu\u0308tlichkeit")
	want := "Müller - 2020 - Gemütlichkeit.pdf" // NFC in source
	if got != want {
		t.Fatalf("NFD input must normalize to NFC bytes: got %q want %q", got, want)
	}
	// idempotent: NFC input stays byte-identical
	if again := SchemaFilename([]zoteroprovider.Creator{{LastName: "Müller", CreatorType: "author"}}, 2020, "Gemütlichkeit"); again != want {
		t.Fatalf("NFC input must stay NFC: %q", again)
	}
}

func TestSchemaFilenameEditorOnlyUsesFirstEditor(t *testing.T) {
	// #287 (production case Learning Analytics, transcript): editors-only
	// volumes (Herausgeberwerke — the standard shape of German academic
	// Sammelbände) take the FIRST EDITOR's last name, consistent with
	// citation practice (Queckenberg et al. (Hg.)). The publisher is
	// never a name component.
	got := SchemaFilename([]zoteroprovider.Creator{
		{LastName: "Queckenberg", FirstName: "Lea", CreatorType: "editor"},
		{LastName: "Leschke", FirstName: "Robin", CreatorType: "editor"},
		{LastName: "Persike", FirstName: "Norman", CreatorType: "editor"},
	}, 2025, "Learning Analytics, Artificial Intelligence und Data Mining in der Hochschulbildung")
	if !strings.HasPrefix(got, "Queckenberg - 2025 - ") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "transcript") {
		t.Fatalf("publisher must never appear: %q", got)
	}
}

func TestSchemaFilenameAuthorBeatsEditor(t *testing.T) {
	// cascade order: an author wins over editors (mixed creator lists).
	got := SchemaFilename([]zoteroprovider.Creator{
		{LastName: "Editor", CreatorType: "editor"},
		{LastName: "Autor", CreatorType: "author"},
	}, 2024, "Titel")
	if got != "Autor - 2024 - Titel.pdf" {
		t.Fatalf("got %q", got)
	}
}

func TestSchemaFilenameSanitizesSeparators(t *testing.T) {
	got := SchemaFilename([]zoteroprovider.Creator{{LastName: "Müller & Höfe 100%", CreatorType: "author"}}, 2020,
		"Der Frühling +Mehr: Ein/Fall")
	if filepath.IsLocal(got) == false {
		t.Fatalf("not local: %q", got)
	}
	for _, bad := range []string{"/", "\\", "\x00"} {
		if contains(got, bad) {
			t.Fatalf("containing %q: %q", bad, got)
		}
	}
}

func TestQuarantineCopiesOriginal(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "orig.pdf")
	if err := os.WriteFile(src, []byte("BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	p1, err := Quarantine(root, "KEY1", src)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p1) != filepath.Join(root, "originals") {
		t.Fatalf("quarantine dir: %s", p1)
	}
	b, err := os.ReadFile(p1)
	if err != nil || string(b) != "BYTES" {
		t.Fatalf("inhalt: %q %v", b, err)
	}
	// second quarantine of the same key gets a DIFFERENT timestamped name
	p2, err := Quarantine(root, "KEY1", src)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatal("zweite Quarantäne muss eigenen Zeitstempel-Namen bekommen")
	}
}

func TestQuarantineExtensionFollowsSource(t *testing.T) {
	// #220: an EPUB original must quarantine as .epub (not a mislabeled
	// .pdf corpse); extension-less sources keep the .pdf default.
	root := t.TempDir()
	src := filepath.Join(root, "orig.epub")
	if err := os.WriteFile(src, []byte("EPUB"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Quarantine(root, "KEYE", src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(p, ".epub") != true || strings.HasSuffix(p, ".pdf") {
		t.Fatalf("epub quarantine name = %q, want .epub suffix", p)
	}
	bare := filepath.Join(root, "noext")
	if err := os.WriteFile(bare, []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}
	pb, err := Quarantine(root, "KEYB", bare)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(pb, ".pdf") {
		t.Fatalf("extension-less quarantine = %q, want .pdf default", pb)
	}
}

func TestSchemaFilenamePublisherNeverName(t *testing.T) {
	// #287 negative: a document with NO creators must not inherit the
	// publisher as its name component (the "transcript - 2025 - …" defect).
	// Honest "Unbekannt" instead — fixable via Zotero metadata.
	got := SchemaFilename(nil, 2026, "Global Economic Prospects, January 2026")
	if got != "Unbekannt - 2026 - Global Economic Prospects, January 2026.pdf" {
		t.Fatalf("got %q", got)
	}
}

// TestQuarantineCloseFailsClosed (#244 review): a Close-time flush/writeback
// failure must FAIL the quarantine — the custody chain (audit → delete of
// the Zotero original) must never proceed on a copy whose close failed.
// Close failures surface only after the page cache writes back, which no
// hermetic test can trigger on a healthy disk; the closeQuarantine seam
// stands in for that physical failure mode.
func TestQuarantineCloseFailsClosed(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "orig.pdf")
	if err := os.WriteFile(src, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := closeQuarantine
	closeQuarantine = func(f *os.File) error {
		_ = f.Close()
		return errors.New("ENOSPC: writeback failed")
	}
	defer func() { closeQuarantine = prev }()

	p, err := Quarantine(root, "KEYX", src)
	if err == nil {
		t.Fatalf("quarantine must fail when Close fails (got path %q)", p)
	}
	if !strings.Contains(err.Error(), "quarantine close") {
		t.Fatalf("error must name the close step: %v", err)
	}
}
