// Package repair — #184 fix-service support: quarantine + schema
// filenames. Both are RAG-side: the fix-service never writes.
package repair

import (
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// closeQuarantine closes the quarantine destination file. It is a seam so
// tests can force flush/writeback failures — Close-time errors are the one
// custody failure mode no hermetic unit test can trigger on a healthy disk
// (write-back of the page cache happens after Copy reports success).
var closeQuarantine = func(f *os.File) error { return f.Close() }

// Quarantine copies the ORIGINAL of an attachment into the
// RAG-managed quarantine root BEFORE any mutation (design nail):
// originals/<attachment-zotero-key>_<unixns><ext> — audit + rollback basis.
// The extension follows the SOURCE filename (#220: an EPUB original must
// not land as a .pdf corpse); extension-less sources default to .pdf
// (the pre-#220 shape).
// Returns the quarantine path.
func Quarantine(root, zoteroKey, sourcePath string) (string, error) {
	dir := filepath.Join(root, "originals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ext := strings.ToLower(path.Ext(sourcePath))
	if ext == "" {
		ext = ".pdf"
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s_%d%s", zoteroKey, time.Now().UnixNano(), ext))
	for i := 2; ; i++ { // same-nanosecond collision guard
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(dir, fmt.Sprintf("%s_%d_%d%s", zoteroKey, time.Now().UnixNano(), i, ext))
	}
	src, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("quarantine open source: %w", err)
	}
	// Read-side handle: a failed Close loses nothing (the copy already
	// happened) — explicitly nulled (#244 errcheck).
	defer func() { _ = src.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("quarantine create: %w", err)
	}
	closed := false
	defer func() {
		// Error-path close: the copy already failed; log only.
		if !closed {
			if cerr := out.Close(); cerr != nil {
				log.Printf("quarantine close %s: %v", dst, cerr)
			}
		}
	}()
	if _, err := io.Copy(out, src); err != nil {
		return "", fmt.Errorf("quarantine copy: %w", err)
	}
	// CUSTODY FAIL-CLOSED (#244 review): Close can surface flush/writeback
	// errors — a failed Close means the quarantine copy may be broken, and
	// the custody chain (audit → delete of the Zotero original) must NEVER
	// proceed on it. Return the error so Apply stops before any mutation.
	// Log too: the operator should see WHY custody refused.
	if cerr := closeQuarantine(out); cerr != nil {
		closed = true
		log.Printf("quarantine close %s: %v", dst, cerr)
		return "", fmt.Errorf("quarantine close %s: %w", dst, cerr)
	}
	closed = true
	return dst, nil
}

// SchemaFilename builds the dudu schema name:
//
//	{Autor|Institution} - {Jahr} - {Titel}.pdf
//
// First author's lastName (or institutional name), publication year, short
// title. There is NO filename patch mutation anywhere — this builder is the
// ONLY source of attachment filenames for repairs. Creators reuse the
// zotero projection shape (single definition, review W6).
func SchemaFilename(creators []zotero.Creator, year int, title, publisher string) string {
	return schemaFilename(creators, year, title, publisher, ".pdf")
}

// SchemaFilenameForFormat picks the extension from the attachment's
// content type (#220: EPUB repairs upload .epub, not .pdf).
func SchemaFilenameForFormat(creators []zotero.Creator, year int, title, publisher, contentType string) string {
	ext := ".pdf"
	if strings.Contains(contentType, "epub") {
		ext = ".epub"
	}
	return schemaFilename(creators, year, title, publisher, ext)
}

func schemaFilename(creators []zotero.Creator, year int, title, publisher, ext string) string {
	head := ""
	for _, c := range creators {
		if c.CreatorType != "author" {
			continue
		}
		if c.Name != "" { // institutional author (fieldMode 1)
			head = c.Name
		} else if c.LastName != "" {
			head = c.LastName
		}
		if head != "" {
			break
		}
	}
	if head == "" && publisher != "" {
		head = publisher // institutional reports (Weltbank GEP: creators NULL in projection)
	}
	if head == "" {
		head = "Unbekannt"
	}
	y := ""
	if year > 0 {
		y = fmt.Sprintf("%d", year)
	}
	return sanitize(head+" - "+y+" - "+shorten(title, 80)) + ext
}

// shorten trims to n runes at a word boundary.
func shorten(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, " -–:,"); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " -–:,") + "…"
}

// sanitize strips path separators and control characters; keeps umlauts,
// &, +, % (the Zotero upload form round-trips them url-encoded — pinned by
// write_test.go).
var sanitizeRe = regexp.MustCompile(`[/\\\x00-\x1f]`)

func sanitize(s string) string {
	s = sanitizeRe.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}
