package zotero

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestPreferredAttachmentPrefersPDF(t *testing.T) {
	atts := []Attachment{
		{Key: "EPUB", ContentType: "application/epub+zip", Filename: "a.epub"},
		{Key: "PDF", ContentType: "application/pdf", Filename: "a.pdf"},
	}
	got := PreferredAttachment(atts)
	if got == nil || got.Key != "PDF" {
		t.Fatalf("PreferredAttachment = %+v, want PDF", got)
	}
}

func TestPreferredAttachmentFallsBackToEPUB(t *testing.T) {
	atts := []Attachment{
		{Key: "EPUB", ContentType: "application/epub+zip", Filename: "a.epub"},
		{Key: "HTML", ContentType: "text/html", Filename: "a.html"},
	}
	got := PreferredAttachment(atts)
	if got == nil || got.Key != "EPUB" {
		t.Fatalf("PreferredAttachment = %+v, want EPUB", got)
	}
}

func TestPreferredAttachmentEmpty(t *testing.T) {
	if got := PreferredAttachment(nil); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestContentHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	want := hex.EncodeToString(sum[:])
	got, err := ContentHash(path)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}

func TestContentHashMissingFile(t *testing.T) {
	if _, err := ContentHash(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParseYear(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"2010", 2010},
		{"2010-05", 2010},
		{"2010-05-14", 2010},
		{"n.d.", 0},
		{"", 0},
	} {
		if tc.want == 0 {
			if got := ParseYear(tc.in); got != nil {
				t.Errorf("ParseYear(%q) = %d, want nil", tc.in, *got)
			}
		} else if got := ParseYear(tc.in); got == nil || *got != tc.want {
			t.Errorf("ParseYear(%q) = %v, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLocalFilePathDecodesPercentEscapes(t *testing.T) {
	// Zotero reports renamed attachments ("Author - Year - Title.pdf")
	// URL-encoded; the native path must decode for os.Open to work.
	cases := map[string]string{
		"file:///Users/x/Englert%20-%202019%20-%20Nachhaltiges.pdf": "/Users/x/Englert - 2019 - Nachhaltiges.pdf",
		"file:///Users/x/plain.pdf":                                 "/Users/x/plain.pdf",
		"/already/native/path.pdf":                                  "/already/native/path.pdf",
		// Apostrophes survive both encoded and raw.
		"file:///Users/x/D%27heur%20-%202013.pdf": "/Users/x/D'heur - 2013.pdf",
	}
	for in, want := range cases {
		if got := LocalFilePath(in); got != want {
			t.Errorf("LocalFilePath(%q) = %q, want %q", in, got, want)
		}
	}
}
