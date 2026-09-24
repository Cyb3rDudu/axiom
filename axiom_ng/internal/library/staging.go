// staging.go — hashed staging of import content (F06, #300). Files live
// under <artifact-root>/library_staging/<sha256>; the DB carries the
// descriptor only (no BLOB — DoD sonde checks column types). Retention:
// CleanupStaging deletes unreferenced staging files only — it structurally
// cannot touch canonical renditions (the provider's storage), which live
// outside this root.
package library

import (
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

// StoreImport writes content under its content hash and returns the hash.
// Content-addressed: identical bytes are stored once, atomically (temp
// file + rename — a crashed writer leaves either nothing or the whole
// file, never a torn one).
func (s *Staging) StoreImport(content []byte) (sha string, err error) {
	if s.root == "" {
		return "", contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			"staging root not configured (AXIOM_ARTIFACT_ROOT)")
	}
	sum := sha256.Sum256(content)
	sha = hex.EncodeToString(sum[:])
	dst := s.Path(sha)
	if _, err := os.Stat(dst); err == nil {
		return sha, nil // already staged (content addressing)
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.root, ".stage-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := os.Remove(tmp.Name()); cerr != nil && !os.IsNotExist(cerr) && err == nil {
			err = cerr
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
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
