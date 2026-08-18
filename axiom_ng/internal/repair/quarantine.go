// Package repair — #184 fix-service support: quarantine + schema
// filenames. Both are RAG-side: the fix-service never writes.
package repair

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// Quarantine copies the ORIGINAL pdf of an attachment into the
// RAG-managed quarantine root BEFORE any mutation (design nail):
// originals/<attachment-zotero-key>_<unixms>.pdf — audit + rollback basis.
// Returns the quarantine path.
func Quarantine(root, zoteroKey, sourcePath string) (string, error) {
	dir := filepath.Join(root, "originals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s_%d.pdf", zoteroKey, time.Now().UnixNano()))
	for i := 2; ; i++ { // same-nanosecond collision guard
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(dir, fmt.Sprintf("%s_%d_%d.pdf", zoteroKey, time.Now().UnixNano(), i))
	}
	src, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("quarantine open source: %w", err)
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("quarantine create: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		return "", fmt.Errorf("quarantine copy: %w", err)
	}
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
	return sanitize(head+" - "+y+" - "+shorten(title, 80)) + ".pdf"
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
