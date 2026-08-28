package repair

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

func TestSchemaFilenameAuthor(t *testing.T) {
	got := SchemaFilename([]zotero.Creator{{LastName: "Horváth", FirstName: "Péter", CreatorType: "author"},
		{LastName: "Gleich", CreatorType: "author"}}, 2025, "Controlling", "")
	if got != "Horváth - 2025 - Controlling.pdf" {
		t.Fatalf("got %q", got)
	}
}

func TestSchemaFilenameInstitutional(t *testing.T) {
	got := SchemaFilename([]zotero.Creator{{Name: "World Bank", CreatorType: "author"}}, 2026,
		"Global Economic Prospects, January 2026: Expand, Invest, Protect", "")
	if got != "World Bank - 2026 - Global Economic Prospects, January 2026: Expand, Invest, Protect.pdf" {
		t.Fatalf("got %q", got)
	}
	long := SchemaFilename([]zotero.Creator{{LastName: "Müller", CreatorType: "author"}}, 2020,
		strings.Repeat("Sehr langer Buchtitel ", 8), "")
	if !filepath.IsLocal(long) || !strings.HasSuffix(long, "….pdf") {
		t.Fatalf("Kürzung: %q", long)
	}
}

func TestSchemaFilenameSanitizesSeparators(t *testing.T) {
	got := SchemaFilename([]zotero.Creator{{LastName: "Müller & Höfe 100%", CreatorType: "author"}}, 2020,
		"Der Frühling +Mehr: Ein/Fall", "")
	if filepath.IsLocal(got) == false {
		t.Fatalf("not local: %q", got)
	}
	for _, bad := range []string{"/", "\\", "\x00"} {
		if contains(got, bad) {
			t.Fatalf("containing %q: %q", bad, got)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
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

func TestSchemaFilenamePublisherFallback(t *testing.T) {
	got := SchemaFilename(nil, 2026, "Global Economic Prospects, January 2026", "World Bank")
	if got != "World Bank - 2026 - Global Economic Prospects, January 2026.pdf" {
		t.Fatalf("got %q", got)
	}
}
