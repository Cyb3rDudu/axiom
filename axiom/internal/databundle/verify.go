// verify.go — the verify ladder over an imported target: per-table
// counts and canonical row digests (streamed in primary-key order —
// canonicalization makes jsonb key order irrelevant, so digest equality
// IS semantic equality of the envelopes; PostgreSQL renders jsonb in
// its internal order, a byte-stable envelope form is not a contract any
// engine offers), FK-invariant orphan scans from the engine's own
// constraint catalog, and SQLite pragma integrity verdicts.
package databundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
)

// VerifyOptions parameterize a verify run. Exactly one target engine.
type VerifyOptions struct {
	From       string
	DSN        string
	SQLitePath string
}

// TableVerify is one table's comparison verdict.
type TableVerify struct {
	Table      string
	CountOK    bool
	CountGot   int64
	CountWant  int64
	DigestOK   bool
	DigestGot  string
	JSONBVerdict string // semantic-envelope note when the table carries jsonb
}

// FKVerify is one FK relationship's orphan verdict.
type FKVerify struct {
	Table       string
	Constraint  string
	Orphans     int64
}

// VerifyResult is the operator-facing outcome. OK is the one-bit
// verdict; every check that fed it is in the report.
type VerifyResult struct {
	Manifest *Manifest
	Engine   string
	Tables   []TableVerify
	FKs      []FKVerify
	Pragmas  []string
	Warnings []string
	OK       bool
}

// Verify compares a bundle against a target database.
func Verify(ctx context.Context, opts VerifyOptions) (*VerifyResult, error) {
	man, err := LoadManifest(opts.From)
	if err != nil {
		return nil, err
	}
	var snk sink
	if opts.DSN != "" {
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

	res := &VerifyResult{Manifest: man, Engine: snk.engineName(), OK: true, Warnings: append([]string{}, man.Warnings...)}

	for _, tm := range man.Tables {
		if snk.engineName() == "sqlite" && !sqliteCarried(tm.Name) {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"table %s: outside the SQLite namespace — not compared (see import warnings)", tm.Name))
			continue
		}
		tv, err := verifyTable(ctx, snk, &tm)
		if err != nil {
			return res, err
		}
		if !tv.CountOK || !tv.DigestOK {
			res.OK = false
		}
		res.Tables = append(res.Tables, tv)
	}

	// FK invariants over every carried table's own constraints.
	for _, tm := range man.Tables {
		if snk.engineName() == "sqlite" && !sqliteCarried(tm.Name) {
			continue
		}
		fks, err := snk.foreignKeys(ctx, tm.Name)
		if err != nil {
			return res, fmt.Errorf("table %s: read FK catalog: %w", tm.Name, err)
		}
		for _, fk := range fks {
			orphans, err := orphanCount(ctx, snk, tm.Name, fk)
			if err != nil {
				return res, fmt.Errorf("table %s FK %s: orphan scan: %w", tm.Name, fk.Constraint, err)
			}
			if orphans > 0 {
				res.OK = false
			}
			res.FKs = append(res.FKs, FKVerify{Table: tm.Name, Constraint: fk.Constraint, Orphans: orphans})
		}
	}

	pragmas, err := snk.pragmaChecks(ctx)
	if err != nil {
		res.OK = false
		res.Pragmas = []string{err.Error()}
		return res, nil
	}
	res.Pragmas = pragmas
	return res, nil
}

// verifyTable streams the target's rows in PK order, canonicalizes,
// and compares count + digest against the manifest. The digest over
// canonical rows is the byte/semantic envelope readback: canonical
// equality is semantic equality (key-order-insensitive), enforced over
// EVERY row, not a sample.
func verifyTable(ctx context.Context, snk sink, tm *TableManifest) (TableVerify, error) {
	tv := TableVerify{Table: tm.Name, CountWant: tm.Count, DigestGot: "", JSONBVerdict: ""}
	n, err := snk.count(ctx, tm.Name)
	if err != nil {
		return tv, fmt.Errorf("table %s: count: %w", tm.Name, err)
	}
	tv.CountGot = n
	tv.CountOK = n == tm.Count

	cols := make([]ColumnRef, len(tm.Columns))
	for i, c := range tm.Columns {
		cols[i] = ColumnRef{Name: c.Name, Type: c.Type}
	}
	h := sha256.New()
	var line bytes.Buffer
	err = snk.streamOrdered(ctx, tm.Name, cols, tm.Key, func(vals []any) error {
		line.Reset()
		if err := EncodeRow(&line, cols, vals); err != nil {
			return err
		}
		h.Write(line.Bytes())
		return nil
	})
	if err != nil {
		return tv, fmt.Errorf("table %s: stream: %w", tm.Name, err)
	}
	tv.DigestGot = fmt.Sprintf("%x", h.Sum(nil))
	tv.DigestOK = tv.DigestGot == tm.RowsSHA256
	for _, c := range tm.Columns {
		if c.Type == TagJSONB {
			tv.JSONBVerdict = "envelopes canonical-equal (semantic; key order irrelevant)"
			break
		}
	}
	return tv, nil
}

// orphanCount counts rows whose FK column is set but references an
// absent parent — the invariant the dropped cross-component FKs used to
// guard, checked from data. Table/column identifiers in the built query
// come from the ENGINE'S OWN constraint catalogs (information_schema /
// PRAGMA foreign_key_list) and are identifier-quoted — never operator
// input; SQL identifiers cannot be parameterized.
func orphanCount(ctx context.Context, snk sink, table string, fk fkRef) (int64, error) {
	ident := pgIdent
	if snk.engineName() == "sqlite" {
		ident = sqlIdent
	}
	q := fmt.Sprintf(`SELECT count(*) FROM %s c LEFT JOIN %s p ON c.%s = p.%s WHERE c.%s IS NOT NULL AND p.%s IS NULL`,
		ident(table), ident(fk.RefTable), ident(fk.Column), ident(fk.RefColumn),
		ident(fk.Column), ident(fk.RefColumn))
	var n int64
	var err error
	if s, ok := snk.(*sqliteSink); ok {
		err = s.db.QueryRowContext(ctx, q).Scan(&n)
	} else {
		err = snk.(*pgSink).db.pool.QueryRow(ctx, q).Scan(&n)
	}
	return n, err
}
