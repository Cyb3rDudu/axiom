// export.go — bundle export from the source PostgreSQL database: one
// REPEATABLE READ, READ ONLY snapshot transaction carries the whole
// export (the cutoff), rows stream in primary-key order in bounded
// batches, every batch is hashed while written.
package databundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/version"
	"github.com/jackc/pgx/v5"
)

// ExportOptions parameterize an export run.
type ExportOptions struct {
	DSN       string // source PostgreSQL DSN
	Out       string // bundle directory (created)
	Build     string // build identity line; "" = this binary's banner
	BatchRows int64  // rows per batch file; 0 = default
}

// DefaultBatchRows keeps batches small enough to verify in memory.
const DefaultBatchRows = 500

// ExportResult is the operator-facing outcome.
type ExportResult struct {
	Manifest *Manifest
	Path     string
}

// Export writes a component bundle. Only component "library" exists.
func Export(ctx context.Context, component string, opts ExportOptions) (*ExportResult, error) {
	if component != ComponentLib {
		return nil, fmt.Errorf("unknown component %q (this build exports %q only)", component, ComponentLib)
	}
	db, err := openPG(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	batchRows := opts.BatchRows
	if batchRows <= 0 {
		batchRows = DefaultBatchRows
	}
	build := opts.Build
	if build == "" {
		build = version.Banner()
	}

	// One snapshot for everything: REPEATABLE READ freezes the read
	// view; READ ONLY states the intent. now() inside is the cutoff.
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY`); err != nil {
		return nil, fmt.Errorf("set snapshot isolation: %w", err)
	}
	var cutoff time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&cutoff); err != nil {
		return nil, err
	}

	engine, err := db.engineVersionInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	ledgers, err := db.migrationLedgersInTx(ctx, tx)
	if err != nil {
		return nil, err
	}

	man := &Manifest{
		Format:        FormatName,
		FormatVersion: FormatVersion,
		Component:     component,
		CreatedAt:     canonicalTimestamp(time.Now().UTC()),
		Warnings:      []string{}, // non-nil: serializes [] not null
		Source: SourceManifest{
			Build:        build,
			Engine:       engine,
			ExportCutoff: canonicalTimestamp(cutoff),
			Migrations:   ledgers,
		},
	}

	tablesDir := filepath.Join(opts.Out, "tables")
	// 0700: bundle content is document metadata (Fachdaten).
	if err := os.MkdirAll(tablesDir, 0o700); err != nil {
		return nil, err
	}

	for _, spec := range LibraryTables {
		cat, err := catalogQuery(ctx, tx, spec.Name)
		if err != nil {
			return nil, fmt.Errorf("catalog %s: %w", spec.Name, err)
		}
		if cat == nil {
			man.Warnings = append(man.Warnings,
				fmt.Sprintf("table %s absent at the source (migration stand or component scope) — not carried", spec.Name))
			continue
		}
		// stable keys are component truth, validated against the catalog
		colNames := map[string]bool{}
		for _, c := range cat.columns {
			if c.Type == TagJSONB && c.Nullable {
				return nil, fmt.Errorf("table %s column %s: a NULLABLE jsonb column is outside bundle format v1 (the jsonb null VALUE maps to the null token; an explicit null marker is the format-v2 upgrade) — refusing a lossy export",
					cat.table, c.Name)
			}
			colNames[c.Name] = true
		}
		for _, k := range spec.Keys {
			if !colNames[k] {
				return nil, fmt.Errorf("table %s: stable key column %s not in the engine catalog — spec drift", spec.Name, k)
			}
		}
		cat.key = spec.Keys
		// status vocabularies: engine enum types (pg_enum) + the
		// component's declared text-vocabulary columns — both ride the
		// manifest and gate the import decode.
		if len(cat.enums) == 0 {
			cat.enums = spec.Vocab
		} else {
			maps.Copy(cat.enums, spec.Vocab)
		}
		tm, err := exportTable(ctx, tx, cat, tablesDir, batchRows)
		if err != nil {
			return nil, err
		}
		man.Tables = append(man.Tables, *tm)
	}

	if len(man.Tables) == 0 {
		return nil, fmt.Errorf("source carries none of the component's tables — refusing an empty bundle")
	}
	if err := writeManifest(opts.Out, man); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	return &ExportResult{Manifest: man, Path: opts.Out}, nil
}

// exportTable streams one table into batch files under dir, returning
// its manifest entry (counts, digests, catalog, vocabularies).
func exportTable(ctx context.Context, tx pgx.Tx, cat *pgCatalog, dir string, batchRows int64) (*TableManifest, error) {
	cols := make([]ColumnRef, len(cat.columns))
	for i, c := range cat.columns {
		cols[i] = ColumnRef{Name: c.Name, Type: c.Type}
	}
	order := make([]string, len(cat.key))
	for i, k := range cat.key {
		order[i] = pgIdent(k)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s`,
		pgSelectList(cols), pgIdent(cat.table), strings.Join(order, ", "))

	tableDir := filepath.Join(dir, cat.table)
	if err := os.MkdirAll(tableDir, 0o700); err != nil {
		return nil, err
	}

	tm := &TableManifest{
		Name:    cat.table,
		Columns: cat.columns,
		Key:     cat.key,
		Enums:   cat.enums,
	}
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("stream %s: %w", cat.table, err)
	}
	defer rows.Close()

	// Each encoded line feeds three sinks at once: the table digest
	// (all rows concatenated), the current batch digest, and the batch
	// file buffer. Bounded memory: one line + one batch at a time.
	tableHash := sha256.New()
	batchHash := sha256.New()
	var batchBuf bytes.Buffer
	var line bytes.Buffer
	var batchCount, batchIdx, total int64
	batches := make([]BatchManifest, 0, 1) // non-nil: empty tables serialize [] not null

	flush := func() error {
		if batchCount == 0 {
			return nil
		}
		name := fmt.Sprintf("%04d.jsonl", batchIdx)
		path := filepath.Join(tableDir, name)
		// writeFileSync (tmp+rename, 0600): also on the RE-EXPORT path a
		// pre-existing file with looser modes is replaced, never kept.
		if err := writeFileSync(path, batchBuf.Bytes()); err != nil {
			return err
		}
		batches = append(batches, BatchManifest{
			File:   filepath.ToSlash(filepath.Join("tables", cat.table, name)),
			Count:  batchCount,
			SHA256: fmt.Sprintf("%x", batchHash.Sum(nil)),
		})
		batchBuf.Reset()
		batchHash = sha256.New()
		batchCount = 0
		batchIdx++
		return nil
	}

	for rows.Next() {
		vals, err := pgScanRow(rows, cols)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", cat.table, err)
		}
		line.Reset()
		if err := EncodeRow(&line, cols, vals); err != nil {
			return nil, fmt.Errorf("encode %s: %w", cat.table, err)
		}
		b := line.Bytes()
		tableHash.Write(b)
		batchHash.Write(b)
		batchBuf.Write(b)
		batchCount++
		total++
		if batchCount >= batchRows {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stream %s: %w", cat.table, err)
	}
	if err := flush(); err != nil {
		return nil, err
	}

	tm.Count = total
	tm.Batches = batches
	tm.RowsSHA256 = fmt.Sprintf("%x", tableHash.Sum(nil))
	return tm, nil
}
