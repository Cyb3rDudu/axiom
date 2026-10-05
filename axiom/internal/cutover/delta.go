// delta.go — the window's set arithmetic over canonical row indexes:
//
//	freeze delta      source(now) vs baseline bundle  -> delta bundle + report
//	tombstones        keys the source dropped since the baseline
//	freeze anchor     target state at cutover completion (key -> row digest)
//	reverse delta     target(now) vs freeze anchor    -> what rollback must land
//
// All rows flow through the databundle canonical codec, so every
// digest here is comparable with every digest the bundle format and
// the shadow comparison already produce.
package cutover

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom/internal/databundle"
	"github.com/Cyb3rDudu/axiom/axiom/internal/version"
)

// DeltaReport is the machine-readable delta bookkeeping every cutover
// leaves behind (tables, counts, tombstone keys — identifiers only).
type DeltaReport struct {
	Format        string              `json:"format"` // axiom-cutover-delta-report
	FormatVersion int                 `json:"format_version"`
	Kind          string              `json:"kind"` // freeze | reverse
	RunID         string              `json:"run_id"`
	Baseline      string              `json:"baseline,omitempty"`
	Cutoff        string              `json:"cutoff"`
	Build         string              `json:"build"`
	Tables        []TableDelta        `json:"tables"`
	TombstoneKeys map[string][]string `json:"tombstone_keys,omitempty"`
	OK            bool                `json:"ok"`
}

// TableDelta is one table's delta counts.
type TableDelta struct {
	Table      string `json:"table"`
	Added      int64  `json:"added"`
	Changed    int64  `json:"changed"`
	Tombstoned int64  `json:"tombstoned"`
	Unchanged  int64  `json:"unchanged"`
}

// tableDelta computes one table's delta rows: lines to carry (added +
// changed) and tombstone keys (present in base, gone in cur).
func tableDelta(table string, base, cur *databundle.TableIndex) (carry []string, tombstones []string, td TableDelta, err error) {
	td = TableDelta{Table: table}
	if base != nil && cur != nil {
		if !sameColumns(base.Columns, cur.Columns) || !sameKeys(base.Key, cur.Key) {
			return nil, nil, td, fmt.Errorf("table %s: the source catalog drifted from the baseline bundle (columns/keys differ) — re-baseline before the window", table)
		}
	}
	for key, line := range cur.Rows {
		b, ok := base.Rows[key]
		switch {
		case !ok:
			td.Added++
		case b != line:
			td.Changed++
		default:
			td.Unchanged++
			continue
		}
		carry = append(carry, line)
	}
	for key := range base.Rows {
		if _, ok := cur.Rows[key]; !ok {
			tombstones = append(tombstones, key)
			td.Tombstoned++
		}
	}
	sort.Slice(carry, func(i, j int) bool { return carry[i] < carry[j] })
	sort.Strings(tombstones)
	return carry, tombstones, td, nil
}

func sameColumns(a, b []databundle.ColumnRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeDeltaBundle writes a complete bundle under dir carrying the
// delta rows of cur vs base. The manifest's per-table metadata
// (columns, keys, enums) comes from the BASELINE manifest — proven
// equal to the live source catalog by tableDelta — so a delta bundle
// is a first-class bundle: importable, verifiable, shadow-comparable.
func writeDeltaBundle(dir string, baseline *databundle.Manifest, base, cur *databundle.SourceIndex, runID, kind string, logf func(string, ...any)) (*DeltaReport, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tables"), 0o700); err != nil {
		return nil, err
	}
	rep := &DeltaReport{
		Format: "axiom-cutover-delta-report", FormatVersion: 1, Kind: kind,
		RunID: runID, Baseline: "baseline", Build: version.Banner(),
		Cutoff: databundle.CanonicalTimestamp(cur.Cutoff),
	}
	man := &databundle.Manifest{
		Format: databundle.FormatName, FormatVersion: databundle.FormatVersion,
		Component: baseline.Component, Warnings: []string{},
		CreatedAt: databundle.CanonicalTimestampNow(),
		Source: databundle.SourceManifest{
			Build: version.Banner(), Engine: cur.Engine,
			ExportCutoff: rep.Cutoff, Migrations: baseline.Source.Migrations,
		},
	}
	for _, tm := range baseline.Tables {
		spec := tm.Name
		b, c := base.Tables[spec], cur.Tables[spec]
		if b == nil || c == nil {
			return nil, fmt.Errorf("table %s: absent from the %s side — the window requires the full library data set", spec, map[bool]string{true: "baseline", false: "source"}[b == nil])
		}
		carry, tombstones, td, err := tableDelta(spec, b, c)
		if err != nil {
			return nil, err
		}
		td.Table = spec
		rep.Tables = append(rep.Tables, td)
		if len(tombstones) > 0 {
			if rep.TombstoneKeys == nil {
				rep.TombstoneKeys = map[string][]string{}
			}
			rep.TombstoneKeys[spec] = tombstones
		}

		tableDir := filepath.Join(dir, "tables", spec)
		if err := os.MkdirAll(tableDir, 0o700); err != nil {
			return nil, err
		}
		out := &databundle.TableManifest{
			Name: spec, Columns: tm.Columns, Key: tm.Key, Enums: tm.Enums,
		}
		var buf strings.Builder
		var batchHash = sha256.New()
		var batchIdx, batchCount int64
		flush := func() error {
			if batchCount == 0 {
				return nil
			}
			name := fmt.Sprintf("%04d.jsonl", batchIdx)
			path := filepath.Join(tableDir, name)
			if err := databundle.WriteBatchFile(path, []byte(buf.String())); err != nil {
				return err
			}
			out.Batches = append(out.Batches, databundle.BatchManifest{
				File:  filepath.ToSlash(filepath.Join("tables", spec, name)),
				Count: batchCount, SHA256: fmt.Sprintf("%x", batchHash.Sum(nil)),
			})
			buf.Reset()
			batchHash = sha256.New()
			batchCount = 0
			batchIdx++
			return nil
		}
		for _, line := range carry {
			buf.WriteString(line)
			buf.WriteByte('\n')
			batchHash.Write([]byte(line + "\n"))
			batchCount++
			out.Count++
			if batchCount >= 500 {
				if err := flush(); err != nil {
					return nil, err
				}
			}
		}
		if err := flush(); err != nil {
			return nil, err
		}
		man.Tables = append(man.Tables, *out)
		if logf != nil {
			logf("delta %s: +%d added  ~%d changed  -%d tombstoned  =%d unchanged", spec, td.Added, td.Changed, td.Tombstoned, td.Unchanged)
		}
	}
	if err := databundle.WriteManifestDir(dir, man); err != nil {
		return nil, fmt.Errorf("write delta manifest: %w", err)
	}
	rep.OK = true
	return rep, nil
}

// Anchor is the freeze anchor: per table, stable key -> SHA-256 of the
// canonical line, plus the read's provenance. The reverse-delta base.
type Anchor struct {
	Format        string        `json:"format"` // axiom-cutover-anchor
	FormatVersion int           `json:"format_version"`
	RunID         string        `json:"run_id"`
	Engine        string        `json:"engine"`
	Cutoff        string        `json:"cutoff"`
	Tables        []AnchorTable `json:"tables"`
}

// AnchorTable is one table's key->digest map.
type AnchorTable struct {
	Table string            `json:"table"`
	Count int64             `json:"count"`
	Rows  map[string]string `json:"rows"` // canonical key -> sha256(canonical line)
}

// writeAnchor indexes the target canonically and lands the anchor
// artifact. Digests only — the anchor is evidence, not data.
func writeAnchor(path, runID string, idx *databundle.SourceIndex) (*Anchor, error) {
	a := &Anchor{
		Format: "axiom-cutover-anchor", FormatVersion: 1, RunID: runID,
		Engine: idx.Engine, Cutoff: databundle.CanonicalTimestamp(idx.Cutoff),
	}
	for _, name := range sortedTableNames(idx) {
		t := idx.Tables[name]
		at := AnchorTable{Table: name, Count: t.Count(), Rows: map[string]string{}}
		for k, line := range t.Rows {
			at.Rows[k] = fmt.Sprintf("%x", sha256.Sum256([]byte(line)))
		}
		a.Tables = append(a.Tables, at)
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(path, append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	return a, nil
}

// loadAnchor reads an anchor artifact.
func loadAnchor(path string) (*Anchor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("anchor: read %s: %w", path, err)
	}
	var a Anchor
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("anchor: decode: %w", err)
	}
	if a.Format != "axiom-cutover-anchor" {
		return nil, fmt.Errorf("anchor: format %q is not axiom-cutover-anchor", a.Format)
	}
	return &a, nil
}

// anchorMap flattens an anchor into table -> (key -> digest).
func (a *Anchor) anchorMap() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, t := range a.Tables {
		out[t.Table] = t.Rows
	}
	return out
}

// reverseDelta diffs the live target against the freeze anchor: rows
// new or changed since the cutover — exactly what a post-write
// rollback must land in the shadow tables. Returns the delta report +
// the carrying bundle directory.
func reverseDelta(dir string, anchor *Anchor, target *databundle.SourceIndex, baselineMan *databundle.Manifest, runID string, logf func(string, ...any)) (*DeltaReport, error) {
	am := anchor.anchorMap()
	// A synthetic base index from the anchor digests: rows present with
	// a digest equal to the live line's are unchanged; everything else
	// carries. Tombstones = anchor keys the target dropped.
	base := &databundle.SourceIndex{Tables: map[string]*databundle.TableIndex{}}
	for _, name := range sortedTableNames(target) {
		live := target.Tables[name]
		digests := am[name]
		base.Tables[name] = &databundle.TableIndex{
			Table: name, Columns: live.Columns, Key: live.Key,
			Rows: map[string]string{},
		}
		for k, line := range live.Rows {
			d, known := digests[k]
			switch {
			case known && d == fmt.Sprintf("%x", sha256.Sum256([]byte(line))):
				base.Tables[name].Rows[k] = line // unchanged — occupies the base slot
			case known:
				// anchor knows the key, digest differs → changed; a
				// phantom base row (never equal to a canonical line)
				// makes tableDelta classify it Changed, not Added.
				base.Tables[name].Rows[k] = "\x00changed"
			}
		}
	}
	// Anchor keys with no live row must count as tombstones: give the
	// base a phantom row (line "") for each so tableDelta tombstones it.
	for tname, digests := range am {
		b := base.Tables[tname]
		if b == nil {
			continue
		}
		live := target.Tables[tname]
		for k := range digests {
			if _, ok := live.Rows[k]; !ok {
				b.Rows[k] = "\x00phantom" // dropped at target — tombstone, never carried
			}
		}
	}
	return writeDeltaBundle(dir, baselineMan, base, target, runID, "reverse", logf)
}

// sortedTableNames lists an index's tables in stable order.
func sortedTableNames(idx *databundle.SourceIndex) []string {
	out := make([]string, 0, len(idx.Tables))
	for name := range idx.Tables {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sha256Sum is the digest helper (hex).
func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
