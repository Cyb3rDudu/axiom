// manifest.go — the bundle manifest: structure, canonical write, and
// the load+verify path (sidecar digest first — a tampered manifest
// aborts before any digest inside it is trusted).
package databundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Format identity — the bundle's own version contract. An importer that
// does not know format_version N refuses loudly (forward compat is a
// decision, never a guess).
const (
	FormatName    = "axiom-data-bundle"
	FormatVersion = 1

	ManifestFile  = "manifest.json"
	SidecarFile   = "bundle.sha256"
	ComponentLib  = "library"
)

// ColumnRef is one column reference in a table catalog.
type ColumnRef struct {
	Name string `json:"name"`
	Type string `json:"type"` // canonical tag
}

// ColumnManifest is a column as declared in the manifest (the catalog
// the import validates targets against — names AND types).
type ColumnManifest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

// BatchManifest pins one batch file: name, row count, SHA-256 over the
// file's exact bytes.
type BatchManifest struct {
	File   string `json:"file"` // relative to the bundle root
	Count  int64  `json:"count"`
	SHA256 string `json:"sha256"`
}

// TableManifest carries one table's complete portability contract:
// column catalog, key columns (the stable ordering), row count, batch
// pins, aggregate rows digest, and the enum vocabularies observed at
// the source.
type TableManifest struct {
	Name       string              `json:"name"`
	Columns    []ColumnManifest    `json:"columns"`
	Key        []string            `json:"key"`
	Count      int64               `json:"count"`
	Batches    []BatchManifest     `json:"batches"`
	RowsSHA256 string              `json:"rows_sha256"`
	Enums      map[string][]string `json:"enums,omitempty"`
}

// SourceManifest is the provenance block: build identity, engine,
// export cutoff (the snapshot instant), and every migration ledger's
// applied versions.
type SourceManifest struct {
	Build        string              `json:"build"`
	Engine       string              `json:"engine"`
	ExportCutoff string              `json:"export_cutoff"`
	Migrations   map[string][]string `json:"migrations"`
}

// Manifest is the bundle's single entry point.
type Manifest struct {
	Format        string          `json:"format"`
	FormatVersion int             `json:"format_version"`
	Component     string          `json:"component"`
	CreatedAt     string          `json:"created_at"`
	Source        SourceManifest  `json:"source"`
	Tables        []TableManifest `json:"tables"`
	Warnings      []string        `json:"warnings"`
}

// Validate checks the self-consistent invariants an importer relies on
// (format identity, component, non-empty tables, key columns exist).
func (m *Manifest) Validate() error {
	if m.Format != FormatName {
		return fmt.Errorf("bundle format %q is not %q", m.Format, FormatName)
	}
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("bundle format_version %d is not supported (this build understands %d only)", m.FormatVersion, FormatVersion)
	}
	if m.Component == "" {
		return fmt.Errorf("bundle carries no component")
	}
	for i := range m.Tables {
		t := &m.Tables[i]
		if t.Name == "" {
			return fmt.Errorf("table %d has no name", i)
		}
		if len(t.Key) == 0 {
			return fmt.Errorf("table %s declares no key columns", t.Name)
		}
		if len(t.Columns) == 0 {
			return fmt.Errorf("table %s declares no columns", t.Name)
		}
		have := map[string]bool{}
		for _, c := range t.Columns {
			have[c.Name] = true
		}
		for _, k := range t.Key {
			if !have[k] {
				return fmt.Errorf("table %s key column %s is not in its catalog", t.Name, k)
			}
		}
		for col := range t.Enums {
			if !have[col] {
				return fmt.Errorf("table %s enum vocabulary for unknown column %s", t.Name, col)
			}
		}
	}
	return nil
}

// Table returns the table manifest by name, or nil.
func (m *Manifest) Table(name string) *TableManifest {
	for i := range m.Tables {
		if m.Tables[i].Name == name {
			return &m.Tables[i]
		}
	}
	return nil
}

// manifestBytes serializes the manifest deterministically (struct field
// order is fixed; json escapes deterministically) — the bytes the
// sidecar pins.
func manifestBytes(m *Manifest) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeManifest atomically lands manifest.json + bundle.sha256.
func writeManifest(root string, m *Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	mb, err := manifestBytes(m)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(mb)
	sidecar := fmt.Sprintf("%x  %s\n", sum, ManifestFile)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(root, ManifestFile), mb); err != nil {
		return err
	}
	return writeFileSync(filepath.Join(root, SidecarFile), []byte(sidecar))
}

func writeFileSync(path string, content []byte) error {
	tmp := path + ".tmp"
	// 0600: the bundle carries document metadata (titles, abstracts,
	// creators) — Fachdaten, not world-readable on a shared host.
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadManifest reads and verifies a bundle's entry point: the sidecar
// digest over manifest.json first, then structure validation. Any
// mismatch names the file and both digests — never file content.
func LoadManifest(root string) (*Manifest, error) {
	mb, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ManifestFile, err)
	}
	sb, err := os.ReadFile(filepath.Join(root, SidecarFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", SidecarFile, err)
	}
	want := strings.TrimSpace(string(sb))
	got := fmt.Sprintf("%x  %s", sha256.Sum256(mb), ManifestFile)
	if want != got {
		return nil, fmt.Errorf("manifest tamper-evidence failed: %s pins %s, manifest.json hashes to %s (the bundle is not trustworthy — aborting before any inner digest is consulted)",
			SidecarFile, want, strings.Fields(got)[0])
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", ManifestFile, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
