package zotero

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// PreferredAttachment picks the attachment to enqueue for a document. The rule
// for v1: prefer a PDF over an EPUB when both exist; fall back to EPUB only if
// no PDF is present. Returns nil when there is nothing processable. Selection
// is deterministic (first match wins) so hash-based job dedup is stable.
func PreferredAttachment(atts []Attachment) *Attachment {
	if len(atts) == 0 {
		return nil
	}
	var pdf *Attachment
	var epub *Attachment
	for i := range atts {
		a := &atts[i]
		switch strings.ToLower(a.ContentType) {
		case "application/pdf":
			if pdf == nil {
				pdf = a
			}
		case "application/vnd.openxmlformats-officedocument.epub+zip",
			"application/epub", "application/epub+zip":
			if epub == nil {
				epub = a
			}
		default:
			if epub == nil && strings.HasSuffix(strings.ToLower(a.Filename), ".epub") {
				epub = a
			}
		}
	}
	if pdf != nil {
		return pdf
	}
	return epub
}

// LocalFilePath converts a Zotero file:// URI into a native filesystem path
// (e.g. "file:///Users/x/y.pdf" -> "/Users/x/y.pdf"). Percent-escapes are
// decoded — Zotero reports renamed attachments ("Author - Year - Title.pdf")
// URL-encoded. Non-file URIs are returned unchanged so a plain path still
// works; undecodable input falls back to the raw path.
func LocalFilePath(uri string) string {
	const prefix = "file://"
	if !strings.HasPrefix(uri, prefix) {
		return uri
	}
	raw := strings.TrimPrefix(uri, prefix)
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

// ContentHash returns a stable sha256 hex digest of a local file's contents,
// used as the idempotency key for ingest jobs. Missing files yield an error so
// callers can mark the job FILE_NOT_FOUND rather than silently skip.
func ContentHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ParseYear extracts a four-digit year from a Zotero date string (e.g.
// "2010", "2010-05", "2010-05-14"). It returns nil when no year is present.
func ParseYear(date string) *int {
	for _, part := range strings.FieldsFunc(date, func(r rune) bool {
		return r == '-' || r == '/' || r == '.'
	}) {
		if len(part) == 4 && part[0] >= '1' && part[0] <= '2' {
			if y, err := strconv.Atoi(part); err == nil && y > 999 && y < 3000 {
				return &y
			}
		}
	}
	return nil
}
