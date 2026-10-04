// import.go — bundle import into a target engine. Data-only (zero DDL,
// runs under the DM07 DML-only role), dependency-ordered, idempotent
// per stable primary key + canonical payload comparison: a retry never
// duplicates. Digests are verified BEFORE a batch's transaction begins
// — a mismatch isolates the batch (nothing applied) and aborts the
// whole import.
package databundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ImportOptions parameterize an import run. Exactly one target engine:
// DSN (PostgreSQL) or SQLitePath.
type ImportOptions struct {
	From       string // bundle directory
	DSN        string // PostgreSQL target (DML-only role suffices)
	SQLitePath string // library.sqlite target (library_* namespace only)
	Merge      bool   // allow import into a non-empty target
}

// TableImport is one table's outcome.
type TableImport struct {
	Table      string
	Inserted   int64
	Idempotent int64 // existing identical rows — skipped, not duplicated
	Skipped    bool  // engine-namespace skip (legacy table on SQLite)
	SkipNote   string
}

// ImportResult is the operator-facing outcome.
type ImportResult struct {
	Manifest *Manifest
	Engine   string
	Tables   []TableImport
	Warnings []string
}

// Import applies a bundle to the target. The whole import is resumable:
// re-running after an abort skips identical rows and continues (the
// occupied-target guard fires first — a resume needs the explicit merge
// mode, which is also the no-duplication discipline).
func Import(ctx context.Context, opts ImportOptions) (*ImportResult, error) {
	man, err := LoadManifest(opts.From)
	if err != nil {
		return nil, err
	}
	if man.Component != ComponentLib {
		return nil, fmt.Errorf("bundle component %q does not match this importer (%q)", man.Component, ComponentLib)
	}
	var snk sink
	if opts.DSN != "" {
		if opts.SQLitePath != "" {
			return nil, fmt.Errorf("two targets given — pass exactly one of DSN (PostgreSQL) or SQLitePath")
		}
		p, err := openPG(ctx, opts.DSN)
		if err != nil {
			return nil, err
		}
		snk = &pgSink{db: p}
	} else if opts.SQLitePath != "" {
		s, err := openSQLiteSink(ctx, opts.SQLitePath)
		if err != nil {
			return nil, err
		}
		snk = s
	} else {
		return nil, fmt.Errorf("no target: pass exactly one of DSN (PostgreSQL) or SQLitePath")
	}
	defer snk.close()

	res := &ImportResult{Manifest: man, Engine: snk.engineName(), Warnings: append([]string{}, man.Warnings...)}

	for _, tm := range man.Tables {
		ti, err := importTable(ctx, snk, opts, &tm)
		if err != nil {
			return res, err
		}
		if ti.SkipNote != "" {
			res.Warnings = append(res.Warnings, ti.SkipNote)
		}
		res.Tables = append(res.Tables, ti)
	}
	return res, nil
}

// importTable applies one table (all batches) with every contract
// check. Skipped tables (engine namespace) report Skipped=true.
func importTable(ctx context.Context, snk sink, opts ImportOptions, tm *TableManifest) (TableImport, error) {
	ti := TableImport{Table: tm.Name}

	if snk.engineName() == "sqlite" && !sqliteCarried(tm.Name) {
		ti.Skipped = true
		ti.SkipNote = fmt.Sprintf(
			"table %s: the SQLite target carries the library_* namespace only (F12) — skipped, %d row(s) not applied",
			tm.Name, tm.Count)
		return ti, nil
	}

	// Structural validation: the target must carry exactly the bundle's
	// column set — migration-stand drift is loud, never best-effort.
	// On PostgreSQL the canonical TYPES are compared too (the engine
	// reports real types); SQLite column tags are nominal by design
	// (TEXT affinity under the documented dialect mapping) — names only.
	tcols, err := snk.catalogColumns(ctx, tm.Name)
	if err != nil {
		return ti, fmt.Errorf("table %s: read target catalog: %w", tm.Name, err)
	}
	if tcols == nil {
		return ti, fmt.Errorf("table %s: absent from the target — migrate the target schema first (the import is data-only, zero DDL)", tm.Name)
	}
	if len(tcols) != len(tm.Columns) {
		return ti, fmt.Errorf("table %s: target carries %d columns, bundle declares %d — migration-stand mismatch, refusing", tm.Name, len(tcols), len(tm.Columns))
	}
	strictTypes := snk.engineName() == "postgresql"
	for i, c := range tm.Columns {
		if tcols[i].Name != c.Name {
			return ti, fmt.Errorf("table %s: column %d is %q in the target but %q in the bundle — migration-stand mismatch, refusing",
				tm.Name, i+1, tcols[i].Name, c.Name)
		}
		if strictTypes && tcols[i].Type != c.Type {
			return ti, fmt.Errorf("table %s column %s: target type is %q, bundle declares %q — type drift, refusing",
				tm.Name, c.Name, tcols[i].Type, c.Type)
		}
	}
	cols := make([]ColumnRef, len(tm.Columns))
	for i, c := range tm.Columns {
		cols[i] = ColumnRef{Name: c.Name, Type: c.Type}
	}

	// Non-empty guard: an occupied target demands the explicit merge
	// mode (an accidental double import onto live data must not run).
	n, err := snk.count(ctx, tm.Name)
	if err != nil {
		return ti, fmt.Errorf("table %s: count target: %w", tm.Name, err)
	}
	if n > 0 && !opts.Merge {
		return ti, fmt.Errorf("table %s: target already carries %d row(s) — import into a non-empty target requires the explicit merge mode", tm.Name, n)
	}

	// Enum teeth part 1: the target's own vocabulary must cover the
	// bundle's (PG reads pg_enum; SQLite relies on CHECK constraints +
	// the manifest vocabulary below).
	targetVocab, err := snk.enumVocabularies(ctx, tm.Name)
	if err != nil {
		return ti, fmt.Errorf("table %s: read target enum vocabularies: %w", tm.Name, err)
	}
	enums := map[string][]string{}
	for col, vocab := range tm.Enums {
		enums[col] = vocab
		if tv := targetVocab[col]; tv != nil {
			for _, v := range vocab {
				if !slices.Contains(tv, v) {
					return ti, fmt.Errorf("table %s column %s: enum value %q exists at the source but not in the target's vocabulary — refusing loudly (no silent default)",
						tm.Name, col, v)
				}
			}
		}
	}
	// Engine teeth independent of the manifest: enum-TYPED columns get
	// the TARGET's own pg_enum vocabulary injected into the decode gate
	// — a tampered manifest (vocab stripped) still cannot smuggle an
	// unknown enum past decode; the column constraint stays the
	// final backstop.
	for _, c := range tm.Columns {
		if c.Type == TagEnum {
			if tv := targetVocab[c.Name]; tv != nil {
				enums[c.Name] = tv
			}
		}
	}

	keyIdx, err := keyIndices(cols, tm.Key)
	if err != nil {
		return ti, err
	}

	var rowBase int64
	for _, b := range tm.Batches {
		batch, err := readBatch(opts.From, &b, tm)
		if err != nil {
			return ti, err // digest mismatch: batch isolated, import aborts
		}
		var rowNum int64
		for _, line := range batch.lines {
			rowNum++
			vals, err := DecodeRow(line, cols, enums, rowBase+rowNum)
			if err != nil {
				return ti, fmt.Errorf("table %s: %w", tm.Name, err)
			}
			inserted, err := upsertRow(ctx, snk, tm, cols, keyIdx, vals)
			if err != nil {
				return ti, fmt.Errorf("table %s: %w", tm.Name, err)
			}
			if inserted {
				ti.Inserted++
			} else {
				ti.Idempotent++
			}
		}
		rowBase += rowNum
	}
	if err := snk.resyncSequences(ctx, tm.Name); err != nil {
		return ti, fmt.Errorf("table %s: %w", tm.Name, err)
	}
	return ti, nil
}

// batchFile is one verified batch: its per-row lines.
type batchFile struct {
	lines [][]byte
}

// readBatch loads one batch file and verifies its digest FIRST — the
// mismatch aborts before anything of this batch is applied (isolated)
// and names both digests, never content.
func readBatch(root string, b *BatchManifest, tm *TableManifest) (*batchFile, error) {
	path := filepath.Join(root, filepath.FromSlash(b.File))
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("table %s: read batch %s: %w", tm.Name, b.File, err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(raw))
	if got != b.SHA256 {
		return nil, fmt.Errorf("table %s: DIGEST MISMATCH in %s — manifest pins %s, file hashes to %s; batch isolated (nothing applied), import aborted",
			tm.Name, b.File, b.SHA256, got)
	}
	var lines [][]byte
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		var line []byte
		if i < 0 {
			line = raw
			raw = nil
		} else {
			line = raw[:i]
			raw = raw[i+1:]
		}
		if len(line) == 0 {
			return nil, fmt.Errorf("table %s: batch %s carries an empty line — corrupt row framing", tm.Name, b.File)
		}
		lines = append(lines, line)
	}
	if int64(len(lines)) != b.Count {
		return nil, fmt.Errorf("table %s: batch %s carries %d lines, manifest declares %d", tm.Name, b.File, len(lines), b.Count)
	}
	return &batchFile{lines: lines}, nil
}

// upsertRow implements the idempotency contract: same PK + identical
// canonical payload → skip; same PK + different payload → loud conflict
// (data state, never auto-merged); absent PK → insert with explicit
// values only (no DB defaults).
func upsertRow(ctx context.Context, snk sink, tm *TableManifest, cols []ColumnRef, keyIdx []int, vals []any) (bool, error) {
	keyVals := make([]any, len(keyIdx))
	for i, ki := range keyIdx {
		keyVals[i] = vals[ki]
	}
	existing, err := snk.rowByPK(ctx, tm.Name, cols, tm.Key, keyVals)
	if err != nil {
		if errors.Is(err, errSinkAbsent) {
			if err := snk.insertRow(ctx, tm.Name, cols, vals); err != nil {
				return false, fmt.Errorf("insert (pk %s): %w", pkLabel(tm.Key, keyVals), err)
			}
			return true, nil
		}
		return false, fmt.Errorf("load existing (pk %s): %w", pkLabel(tm.Key, keyVals), err)
	}
	var a, bb bytes.Buffer
	if err := EncodeRow(&a, cols, existing); err != nil {
		return false, err
	}
	if err := EncodeRow(&bb, cols, vals); err != nil {
		return false, err
	}
	if !bytes.Equal(a.Bytes(), bb.Bytes()) {
		return false, fmt.Errorf("merge conflict at pk %s: the target row differs from the bundle row — data state, never auto-merged (resolve manually or re-import into an empty target)",
			pkLabel(tm.Key, keyVals))
	}
	return false, nil // identical — idempotent skip
}

// pkLabel renders PK values for errors: identifiers only, never row
// text (documents, titles and secrets stay out of logs).
func pkLabel(key []string, vals []any) string {
	parts := make([]string, len(key))
	for i, v := range vals {
		switch x := v.(type) {
		case string:
			parts[i] = fmt.Sprintf("%s=%s", key[i], strconv.Quote(x))
		case int64:
			parts[i] = fmt.Sprintf("%s=%d", key[i], x)
		default:
			parts[i] = fmt.Sprintf("%s=<%T>", key[i], v)
		}
	}
	return fmt.Sprintf("(%s)", strings.Join(parts, ", "))
}

// keyIndices maps key column names to catalog positions.
func keyIndices(cols []ColumnRef, key []string) ([]int, error) {
	pos := map[string]int{}
	for i, c := range cols {
		pos[c.Name] = i
	}
	out := make([]int, len(key))
	for i, k := range key {
		p, ok := pos[k]
		if !ok {
			return nil, fmt.Errorf("key column %s not in the catalog", k)
		}
		out[i] = p
	}
	return out, nil
}
