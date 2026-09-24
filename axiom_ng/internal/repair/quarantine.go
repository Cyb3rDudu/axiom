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
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
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

// SchemaFilename builds the dudu schema name. The convention MOVED to
// the Library component in F06 (#300 — filenames are part of the import
// contract); these wrappers keep the repair-side signatures (creators in
// the zotero projection shape) so review-hardened tests stay pinned.
func SchemaFilename(creators []zotero.Creator, year int, title string) string {
	return library.SchemaFilename(toLibraryCreators(creators), year, title)
}

// SchemaFilenameForFormat picks the extension from the attachment's
// content type (#220: EPUB repairs upload .epub, not .pdf) and carries
// the document's existing attachment filenames for the #291 grown-pattern
// exception (empty/nil = no existing attachments → global schema).
func SchemaFilenameForFormat(creators []zotero.Creator, year int, title, contentType string, existing []string) string {
	return library.SchemaFilenameForFormat(toLibraryCreators(creators), year, title, contentType, existing)
}

// toLibraryCreators adapts the zotero projection shape onto the Library
// domain creator (single definition of the convention, review W6).
func toLibraryCreators(cs []zotero.Creator) []library.Creator {
	if len(cs) == 0 {
		return nil
	}
	out := make([]library.Creator, len(cs))
	for i, c := range cs {
		out[i] = library.Creator{
			FirstName: c.FirstName, LastName: c.LastName,
			Name: c.Name, CreatorType: c.CreatorType,
		}
	}
	return out
}
