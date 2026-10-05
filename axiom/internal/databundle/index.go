// index.go — the canonical row-index API (DM09 #318): the machinery
// the cutover orchestration builds on, exported over the same codec
// the export/import/verify/shadow paths use. One table's rows reduce
// to map[canonicalKey]canonicalLine — the delta, freeze-anchor and
// reverse-delta computations are set arithmetic over exactly that
// representation, so every digest the window produces is comparable
// with every digest the bundle format already pins.
//
// Readers:
//
//	IndexBundle      a bundle directory (baseline or freeze evidence)
//	IndexPostgres    a live PostgreSQL source, ONE pinned snapshot
//	IndexSQLiteFile  an imported library.sqlite, read-only
//
// Writer: WriteManifestDir lands manifest.json + bundle.sha256 (the
// exported twin of the export path's writer — delta bundles are
// manifest-complete bundles, just row subsets).
package databundle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TableIndex is one table's canonical row index: every row keyed by
// its stable-key encoding, valued by its full canonical line WITHOUT
// the trailing newline (EncodeRow appends one; the index form is the
// trimmed line — the digest basis everywhere else).
// Columns/Key carry the catalog the read ran under, so a consumer can
// decode key values back into typed wire values (tombstones, shadow
// applies).
type TableIndex struct {
	Table   string
	Columns []ColumnRef
	Key     []string
	Rows    map[string]string
}

// Count is the row count of the index.
func (t *TableIndex) Count() int64 { return int64(len(t.Rows)) }

// SourceIndex is a whole-source read: every Library table the source
// carries, plus the snapshot's provenance (engine line, cutoff).
type SourceIndex struct {
	Tables   map[string]*TableIndex
	Engine   string
	Cutoff   time.Time
	Warnings []string
}

// IndexBundle indexes a bundle directory: every declared table, rows
// decoded straight from the batch lines (already canonical — the line
// IS the value; no re-encoding drift is possible). The bundle's own
// manifest verification (sidecar first) runs before anything is
// indexed; the loaded manifest returns for provenance.
func IndexBundle(ctx context.Context, root string) (*SourceIndex, *Manifest, error) {
	man, err := LoadManifest(root)
	if err != nil {
		return nil, nil, err
	}
	idx := &SourceIndex{Tables: map[string]*TableIndex{}, Warnings: []string{}}
	for _, tm := range man.Tables {
		cols := make([]ColumnRef, len(tm.Columns))
		for i, c := range tm.Columns {
			cols[i] = ColumnRef{Name: c.Name, Type: c.Type}
		}
		ti := &TableIndex{Table: tm.Name, Columns: cols, Key: tm.Key, Rows: map[string]string{}}
		for _, b := range tm.Batches {
			batch, err := readBatch(root, &b, &tm)
			if err != nil {
				return nil, nil, err
			}
			keyIdx, err := keyIndices(cols, tm.Key)
			if err != nil {
				return nil, nil, err
			}
			for _, line := range batch.lines {
				vals, err := DecodeRow(line, cols, tm.Enums, 0)
				if err != nil {
					return nil, nil, fmt.Errorf("table %s: %w", tm.Name, err)
				}
				key, err := canonicalKeyAt(cols, keyIdx, vals)
				if err != nil {
					return nil, nil, fmt.Errorf("table %s: %w", tm.Name, err)
				}
				if _, dup := ti.Rows[key]; dup {
					return nil, nil, fmt.Errorf("table %s: stable key %s appears twice in the bundle — refusing a lossy index", tm.Name, redactKey(key))
				}
				ti.Rows[key] = string(line)
			}
		}
		idx.Tables[tm.Name] = ti
	}
	if man.Source.ExportCutoff != "" {
		if cut, err := parseCanonicalTimestamp(man.Source.ExportCutoff); err == nil {
			idx.Cutoff = cut
		}
	}
	idx.Engine = man.Source.Engine
	idx.Warnings = append(idx.Warnings, man.Warnings...)
	return idx, man, nil
}

// IndexPostgres reads every Library table from a live PostgreSQL
// source under ONE REPEATABLE READ READ ONLY snapshot (the same pull
// discipline as the export: the cutoff is inside the snapshot). A
// table the source does not carry is recorded in Warnings and left
// absent — the cutover decides whether that is drift (the shadow gate
// does, loudly).
func IndexPostgres(ctx context.Context, dsn string) (*SourceIndex, error) {
	snap, err := openPGSnapshot(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer snap.close()
	idx := &SourceIndex{Engine: "postgresql", Cutoff: snap.cutoff, Tables: map[string]*TableIndex{}, Warnings: []string{}}
	engine, err := snap.db.engineVersionInTx(ctx, snap.tx)
	if err != nil {
		return nil, err
	}
	idx.Engine = engine
	for _, spec := range LibraryTables {
		cat, err := catalogQuery(ctx, snap.tx, spec.Name)
		if err != nil {
			return nil, fmt.Errorf("catalog %s: %w", spec.Name, err)
		}
		if cat == nil {
			idx.Warnings = append(idx.Warnings, fmt.Sprintf("table %s absent at the source", spec.Name))
			continue
		}
		cols := make([]ColumnRef, len(cat.columns))
		for i, c := range cat.columns {
			cols[i] = ColumnRef{Name: c.Name, Type: c.Type}
		}
		rows, _, err := snap.readTable(ctx, spec.Name, cols, spec.Keys)
		if err != nil {
			return nil, err
		}
		trimIndexLines(rows)
		idx.Tables[spec.Name] = &TableIndex{Table: spec.Name, Columns: cols, Key: spec.Keys, Rows: rows}
	}
	return idx, nil
}

// IndexSQLiteFile reads every carried Library table from a
// library.sqlite file, read-only (mode=ro, query_only — the shadow's
// opener discipline: a missing path fails, nothing is created).
func IndexSQLiteFile(ctx context.Context, path string) (*SourceIndex, error) {
	s, err := openSQLiteShadow(path)
	if err != nil {
		return nil, err
	}
	defer s.close()
	idx := &SourceIndex{Engine: "sqlite", Tables: map[string]*TableIndex{}, Warnings: []string{}}
	for _, spec := range LibraryTables {
		if !spec.SQLite {
			continue
		}
		cols, err := s.catalogColumns(ctx, spec.Name)
		if err != nil {
			return nil, fmt.Errorf("catalog %s: %w", spec.Name, err)
		}
		if cols == nil {
			idx.Warnings = append(idx.Warnings, fmt.Sprintf("table %s absent in the sqlite file", spec.Name))
			continue
		}
		rows, _, err := sqliteReadTable(ctx, s, spec.Name, cols, spec.Keys)
		if err != nil {
			return nil, err
		}
		trimIndexLines(rows)
		idx.Tables[spec.Name] = &TableIndex{Table: spec.Name, Columns: cols, Key: spec.Keys, Rows: rows}
	}
	return idx, nil
}

// trimIndexLines strips the one trailing newline EncodeRow appends —
// the index's line form is newline-free (bundle lines are too).
func trimIndexLines(rows map[string]string) {
	for k, v := range rows {
		if strings.HasSuffix(v, "\n") {
			rows[k] = v[:len(v)-1]
		}
	}
}

// WriteManifestDir lands manifest.json + bundle.sha256 atomically (the
// export path's writer, exported for producers of complete manifests —
// the cutover's delta bundles).
func WriteManifestDir(root string, m *Manifest) error { return writeManifest(root, m) }

// WriteBatchFile lands one batch file with the export path's
// discipline (tmp+rename, 0600) from pre-encoded canonical lines.
func WriteBatchFile(path string, lines []byte) error {
	return writeFileSync(path, lines)
}

// CanonicalTimestamp renders t in the bundle's canonical UTC µs form —
// the one timestamp spelling every cutover artifact uses.
func CanonicalTimestamp(t time.Time) string { return canonicalTimestamp(t) }

// CanonicalTimestampNow is CanonicalTimestamp(time.Now().UTC()).
func CanonicalTimestampNow() string { return canonicalTimestamp(time.Now().UTC()) }

// TagOfPG maps an information_schema (data_type, udt_name) pair to the
// canonical column tag — exported for catalog comparisons outside the
// import/verify paths (the cutover's shadow-table contract check).
func TagOfPG(dataType, udtName string) (string, error) { return tagOfPG(dataType, udtName) }

// ScanPGRow scans one pgx row into canonical values under cols — the
// exported twin of the import path's row scan (same dest adapters).
func ScanPGRow(rows pgx.Rows, cols []ColumnRef) ([]any, error) { return pgScanRow(rows, cols) }
