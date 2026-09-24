// staging.go — hashed staging of import content (F06, #300). Files live
// under <artifact-root>/library_staging/<sha256>; the DB carries the
// descriptor only (no BLOB — DoD sonde checks column types). Retention:
// CleanupStaging deletes unreferenced staging files only — it structurally
// cannot touch canonical renditions (the provider's storage), which live
// outside this root.
package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
)

// errSizeLimit is the staged-content overflow (InvalidArgument class).
func errSizeLimit(max int64) error {
	return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument,
		fmt.Sprintf("import content exceeds the configured limit (%d bytes)", max))
}

// Staging is the hashed file store for import content.
type Staging struct {
	root string // <artifact-root>/library_staging
}

// NewStaging builds the staging store under the artifact root. An empty
// root yields an unusable staging (StoreImport reports Unavailable) — the
// honest state until the operator configures AXIOM_ARTIFACT_ROOT.
func NewStaging(artifactRoot string) *Staging {
	if artifactRoot == "" {
		return &Staging{}
	}
	return &Staging{root: filepath.Join(artifactRoot, "library_staging")}
}

// Stage streams r into a temp file under the staging root, bounded by
// max bytes (overflow fails AND removes the temp — no partial trace),
// computing the content digest and size INCREMENTALLY. head returns the
// first bytes so the caller validates magic bytes without buffering the
// stream (F06 review: full buffering peaked at 2× the content limit —
// an OOM configured via the 2 GiB hard cap). The temp file stays
// unnamed until Commit: a rejected intake leaves no content-addressed
// trace.
func (s *Staging) Stage(r io.Reader, max int64) (tmpPath, sha string, size int64, head []byte, err error) {
	if s.root == "" {
		return "", "", 0, nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"staging root not configured (AXIOM_ARTIFACT_ROOT)")
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return "", "", 0, nil, err
	}
	tmp, err := os.CreateTemp(s.root, ".stage-*")
	if err != nil {
		return "", "", 0, nil, err
	}
	defer func() {
		if cerr := tmp.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	headw := &headWriter{max: 64}
	n, cerr := io.Copy(io.MultiWriter(tmp, h, headw), io.LimitReader(r, max+1))
	if cerr != nil {
		return "", "", 0, nil, cerr
	}
	if n > max {
		return "", "", 0, nil, errSizeLimit(max)
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), n, headw.head, nil
}

// Commit renames a staged temp file under its content digest
// (content-addressed; identical bytes are stored once, atomically — a
// crashed writer leaves either nothing or the whole file, never a torn
// one).
func (s *Staging) Commit(tmpPath, sha string) error {
	dst := s.Path(sha)
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(tmpPath) // already staged (content addressing)
		return nil
	}
	return os.Rename(tmpPath, dst)
}

// Discard removes a staged temp file (validation failure — no trace).
// A committed (renamed) temp no longer exists; not-exist is fine.
func (s *Staging) Discard(tmpPath string) {
	if tmpPath == "" {
		return
	}
	_ = os.Remove(tmpPath)
}

// DigestWith returns hex(sha256(prefix || staged bytes)) WITHOUT
// buffering the file — the payload identity over canonical request JSON
// plus content, computed from the staged temp.
func (s *Staging) DigestWith(tmpPath string, prefix []byte) (string, error) {
	f, err := os.Open(tmpPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	h.Write(prefix)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// headWriter captures the first max bytes of a stream.
type headWriter struct {
	max  int
	head []byte
}

func (w *headWriter) Write(p []byte) (int, error) {
	if len(w.head) < w.max {
		take := w.max - len(w.head)
		if take > len(p) {
			take = len(p)
		}
		w.head = append(w.head, p[:take]...)
	}
	return len(p), nil
}

// StoreImport is the []byte convenience over Stage+Commit.
func (s *Staging) StoreImport(content []byte) (string, error) {
	tmp, sha, _, _, err := s.Stage(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "", err
	}
	if err := s.Commit(tmp, sha); err != nil {
		s.Discard(tmp)
		return "", err
	}
	return sha, nil
}

// stagingHashRe is the exact shape of a staging name: 64 lowercase hex
// chars — rejects "..", short names and directory components outright.
var stagingHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Open returns a reader over the staged content.
func (s *Staging) Open(sha string) (io.ReadCloser, error) {
	if sha == "" {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "staging hash is blank")
	}
	if !stagingHashRe.MatchString(sha) {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "staging hash malformed")
	}
	f, err := os.Open(s.Path(sha))
	if os.IsNotExist(err) {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "staged content "+sha+" unknown or expired")
	}
	return f, err
}

// Path is the staging file path for a content hash.
func (s *Staging) Path(sha string) string { return filepath.Join(s.root, sha) }

// CleanupStaging is the retention hook: it removes staging files whose
// mtime predates cutoff AND that no library_imports row references.
// ponytail: full-table scan of referenced hashes per run — fine for the
// import volumes of a personal library; index-driven retention if that
// ever changes. Never follows symlinks, never leaves the staging root.
func (s *Store) CleanupStaging(ctx context.Context, st *Staging, cutoff time.Time) (int, error) {
	if st == nil || st.root == "" {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT staging_sha256 FROM library_imports`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	referenced := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return 0, err
		}
		referenced[sha] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(st.root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().Before(cutoff) || referenced[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(st.root, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// mediaTypeFromMagic derives the rendition format from magic bytes ONLY —
// declared extensions and client MIME types are never sufficient (the
// intake rule; shared shape with the F03 reference fake).
func mediaTypeFromMagic(b []byte) (string, error) {
	switch {
	case len(b) >= 5 && string(b[:5]) == "%PDF-":
		return "application/pdf", nil
	case len(b) >= 4 && string(b[:4]) == "PK\x03\x04":
		return "application/epub+zip", nil
	}
	return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument,
		fmt.Sprintf("content magic bytes match neither PDF nor EPUB (got %d bytes)", len(b)))
}
