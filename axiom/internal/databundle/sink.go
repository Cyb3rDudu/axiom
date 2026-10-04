// sink.go — the import/verify target abstraction: one interface, two
// engines (PostgreSQL, SQLite). The import is data-only — zero DDL — so
// it runs under the DM07 DML-only runtime role (axiom_library); the
// target's schema must already exist (migrated via the component's own
// migration path).
package databundle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// fkRef is one declared foreign key relationship (verify's orphan scans).
type fkRef struct {
	Constraint string
	Column     string
	RefTable   string
	RefColumn  string
}

// sink is one import/verify target engine.
type sink interface {
	close() error
	engineName() string
	// catalogColumns returns the table's columns as (name, tag); PG maps
	// from its own types, SQLite names only (values carry the type
	// discipline). Absent table → nil.
	catalogColumns(ctx context.Context, table string) ([]ColumnRef, error)
	enumVocabularies(ctx context.Context, table string) (map[string][]string, error)
	count(ctx context.Context, table string) (int64, error)
	// streamOrdered yields rows in stable-key order, values in cols
	// order, canonicalizable via EncodeRow. The digest basis of verify.
	streamOrdered(ctx context.Context, table string, cols []ColumnRef, keyCols []string, fn func(vals []any) error) error
	// insertRow writes one row, explicit every column (no DB defaults).
	insertRow(ctx context.Context, table string, cols []ColumnRef, vals []any) error
	// rowByPK loads one existing row's canonical values (PK order);
	// errSinkAbsent when not present. The idempotency compare.
	// ponytail: per-row compare, not a batched upsert — row counts are
	// 10³–10⁵ and the payload-compare discipline is the contract; batch
	// COPY only if volumes demand it.
	rowByPK(ctx context.Context, table string, cols []ColumnRef, key []string, keyVals []any) ([]any, error)
	// foreignKeys lists the table's declared FKs (engine catalog truth).
	foreignKeys(ctx context.Context, table string) ([]fkRef, error)
	// resyncSequences re-aligns sequence-backed PKs after explicit-id
	// inserts (PG: setval under the runtime role's USAGE grant; SQLite:
	// AUTOINCREMENT self-updates — no-op).
	resyncSequences(ctx context.Context, table string) error
	// pragmaChecks returns engine-specific integrity verdicts (SQLite:
	// integrity_check + foreign_key_check; PG: none — FK scans cover it).
	pragmaChecks(ctx context.Context) ([]string, error)
}

// ---------------------------------------------------------------------------
// PostgreSQL sink

type pgSink struct {
	db *pgDB
}

func (s *pgSink) close() error { s.db.Close(); return nil }
func (s *pgSink) engineName() string { return "postgresql" }

func (s *pgSink) catalogColumns(ctx context.Context, table string) ([]ColumnRef, error) {
	cat, err := s.db.catalog(ctx, table)
	if err != nil || cat == nil {
		return nil, err
	}
	out := make([]ColumnRef, len(cat.columns))
	for i, c := range cat.columns {
		out[i] = ColumnRef{Name: c.Name, Type: c.Type}
	}
	return out, nil
}

func (s *pgSink) enumVocabularies(ctx context.Context, table string) (map[string][]string, error) {
	cat, err := s.db.catalog(ctx, table)
	if err != nil || cat == nil {
		return nil, err
	}
	return cat.enums, nil
}

func (s *pgSink) count(ctx context.Context, table string) (int64, error) {
	var n int64
	err := s.db.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s`, pgIdent(table))).Scan(&n)
	return n, err
}

func (s *pgSink) streamOrdered(ctx context.Context, table string, cols []ColumnRef, keyCols []string, fn func([]any) error) error {
	order := make([]string, len(keyCols))
	for i, k := range keyCols {
		order[i] = pgIdent(k)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s`,
		pgSelectList(cols), pgIdent(table), strings.Join(order, ", "))
	rows, err := s.db.pool.Query(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		vals, err := pgScanRow(rows, cols)
		if err != nil {
			return err
		}
		if err := fn(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *pgSink) insertRow(ctx context.Context, table string, cols []ColumnRef, vals []any) error {
	ph := make([]string, len(cols))
	names := make([]string, len(cols))
	for i, c := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
		names[i] = pgIdent(c.Name)
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`,
		pgIdent(table), strings.Join(names, ", "), strings.Join(ph, ", "))
	_, err := s.db.pool.Exec(ctx, q, pgWire(cols, vals)...)
	return err
}

func (s *pgSink) rowByPK(ctx context.Context, table string, cols []ColumnRef, key []string, keyVals []any) ([]any, error) {
	where := make([]string, len(key))
	for i, k := range key {
		where[i] = fmt.Sprintf(`%s = $%d`, pgIdent(k), i+1)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s`,
		pgSelectList(cols), pgIdent(table), strings.Join(where, " AND "))
	dests := pgScanDests(cols)
	if err := s.db.pool.QueryRow(ctx, q, keyVals...).Scan(dests...); err != nil {
		return nil, errPGNoRows(err)
	}
	vals := make([]any, len(dests))
	for i, d := range dests {
		vals[i] = scanValue(d)
	}
	return vals, nil
}

func (s *pgSink) foreignKeys(ctx context.Context, table string) ([]fkRef, error) {
	rows, err := s.db.pool.Query(ctx, `
		SELECT tc.constraint_name, kcu.column_name,
		       ccu.table_name, ccu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		JOIN information_schema.constraint_column_usage ccu
		  ON ccu.constraint_name = tc.constraint_name AND ccu.table_schema = tc.table_schema
		WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_schema = current_schema() AND tc.table_name = $1
		ORDER BY tc.constraint_name, kcu.ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fkRef
	for rows.Next() {
		var r fkRef
		if err := rows.Scan(&r.Constraint, &r.Column, &r.RefTable, &r.RefColumn); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *pgSink) resyncSequences(ctx context.Context, table string) error {
	rows, err := s.db.pool.Query(ctx, `
		SELECT a.attname, pg_get_serial_sequence($1, a.attname)
		FROM pg_attribute a
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped`, table)
	if err != nil {
		return err
	}
	type pair struct{ col, seq string }
	var pairs []pair
	for rows.Next() {
		var col string
		var seq *string // NULL for non-serial columns
		if err := rows.Scan(&col, &seq); err != nil {
			rows.Close()
			return err
		}
		if seq != nil && *seq != "" {
			pairs = append(pairs, pair{col, *seq})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range pairs {
		// setval to the current max, is_called — next val is max+1; an
		// empty table keeps the sequence untouched (nothing imported).
		q := fmt.Sprintf(`SELECT setval(%s, COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)`,
			quoteLit(p.seq), pgIdent(p.col), pgIdent(table))
		if _, err := s.db.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("resync sequence %s: %w", p.seq, err)
		}
	}
	return nil
}

func (s *pgSink) pragmaChecks(ctx context.Context) ([]string, error) { return nil, nil }

// ---------------------------------------------------------------------------
// SQLite sink (library.sqlite — the library_* namespace only)

type sqliteSink struct {
	db *sql.DB
}

// openSQLiteSink opens (creating + migrating on first use) the Library
// SQLite file with the engine's operating pragmas, then hands the
// import a raw single-writer handle. Migration reuse: the component's
// own sqlite.Open owns the schema — the sink never emits DDL.
func openSQLiteSink(ctx context.Context, path string) (*sqliteSink, error) {
	if path == "" {
		return nil, fmt.Errorf("data bundle: a SQLite path is required")
	}
	rep, err := sqliteLibraryOpen(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := rep.Close(); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate",
		path, 5000)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // the single writer slot
	return &sqliteSink{db: db}, nil
}

func (s *sqliteSink) close() error { return s.db.Close() }
func (s *sqliteSink) engineName() string { return "sqlite" }

func (s *sqliteSink) tableExists(ctx context.Context, table string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&n)
	return n == 1, err
}

func (s *sqliteSink) catalogColumns(ctx context.Context, table string) ([]ColumnRef, error) {
	exists, err := s.tableExists(ctx, table)
	if err != nil || !exists {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, sqlIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnRef
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, ColumnRef{Name: name, Type: sqliteTagOf(ctype)})
	}
	return out, rows.Err()
}

// sqliteTagOf maps a declared SQLite type to the canonical tag by the
// documented dialect mapping (TEXT/INTEGER/REAL + canonical value
// discipline). Unknown declared types stay "text" — the codec's own
// type teeth are the enforcement on write.
func sqliteTagOf(declared string) string {
	d := strings.ToUpper(strings.TrimSpace(declared))
	switch {
	case strings.Contains(d, "INT"):
		return TagInt64
	case strings.Contains(d, "REAL"), strings.Contains(d, "FLOA"), strings.Contains(d, "DOUB"):
		return TagFloat64
	default:
		return TagText // text, uuid, timestamp, enum, jsonb, numeric-as-token
	}
}

func (s *sqliteSink) enumVocabularies(ctx context.Context, table string) (map[string][]string, error) {
	return nil, nil // no engine vocabulary; manifest + CHECK are the teeth
}

func (s *sqliteSink) count(ctx context.Context, table string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s`, sqlIdent(table))).Scan(&n)
	return n, err
}

// sqliteScanDests scans dynamically-typed SQLite columns: TEXT→string,
// INTEGER→int64, REAL→float64.
func sqliteScanDests(n int) []any {
	dests := make([]any, n)
	for i := range dests {
		var v any
		dests[i] = &v
	}
	return dests
}

// sqliteNormalize folds a driver value into the canonical value shape
// (bools as 0/1 ints are folded back; everything else passes).
func sqliteNormalize(v any) any { return v }

func (s *sqliteSink) streamOrdered(ctx context.Context, table string, cols []ColumnRef, keyCols []string, fn func([]any) error) error {
	order := make([]string, len(keyCols))
	names := make([]string, len(cols))
	for i, k := range keyCols {
		order[i] = sqlIdent(k)
	}
	for i, c := range cols {
		names[i] = sqlIdent(c.Name)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s`,
		strings.Join(names, ", "), sqlIdent(table), strings.Join(order, ", "))
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	dests := sqliteScanDests(len(cols))
	for rows.Next() {
		if err := rows.Scan(dests...); err != nil {
			return err
		}
		vals := make([]any, len(dests))
		for i, d := range dests {
			vals[i] = *(d.(*any))
		}
		if err := fn(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *sqliteSink) insertRow(ctx context.Context, table string, cols []ColumnRef, vals []any) error {
	ph := make([]string, len(cols))
	names := make([]string, len(cols))
	for i, c := range cols {
		ph[i] = "?"
		names[i] = sqlIdent(c.Name)
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`,
		sqlIdent(table), strings.Join(names, ", "), strings.Join(ph, ", "))
	wire, err := sqliteWire(cols, vals)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, q, wire...)
	return err
}

func (s *sqliteSink) rowByPK(ctx context.Context, table string, cols []ColumnRef, key []string, keyVals []any) ([]any, error) {
	where := make([]string, len(key))
	names := make([]string, len(cols))
	for i, k := range key {
		where[i] = fmt.Sprintf(`%s = ?`, sqlIdent(k))
	}
	for i, c := range cols {
		names[i] = sqlIdent(c.Name)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s`,
		strings.Join(names, ", "), sqlIdent(table), strings.Join(where, " AND "))
	rows, err := s.db.QueryContext(ctx, q, sqliteKeyWire(keyVals)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, errSinkAbsent
	}
	dests := sqliteScanDests(len(cols))
	if err := rows.Scan(dests...); err != nil {
		return nil, err
	}
	vals := make([]any, len(dests))
	for i, d := range dests {
		vals[i] = *(d.(*any))
	}
	return vals, rows.Err()
}

// sqliteKeyWire formats PK lookup values the way the import stored
// them (timestamps back to canonical strings — time.Time would bind in
// the driver's own format).
func sqliteKeyWire(keyVals []any) []any {
	out := make([]any, len(keyVals))
	for i, v := range keyVals {
		if t, ok := v.(time.Time); ok {
			out[i] = canonicalTimestamp(t)
			continue
		}
		out[i] = v
	}
	return out
}

func (s *sqliteSink) foreignKeys(ctx context.Context, table string) ([]fkRef, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, sqlIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fkRef
	for rows.Next() {
		var id, seq int
		var refTable, from, to string
		var onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return nil, err
		}
		out = append(out, fkRef{Constraint: fmt.Sprintf("fk_%d_%d", id, seq), Column: from, RefTable: refTable, RefColumn: to})
	}
	return out, rows.Err()
}

func (s *sqliteSink) resyncSequences(ctx context.Context, table string) error {
	return nil // AUTOINCREMENT tracks inserted ids itself
}

// pragmaChecks proves the SQLite file: integrity_check must report ok,
// foreign_key_check must return zero rows.
func (s *sqliteSink) pragmaChecks(ctx context.Context) ([]string, error) {
	var integrity string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return nil, err
	}
	if integrity != "ok" {
		return nil, fmt.Errorf("PRAGMA integrity_check: %s", integrity)
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	violations := 0
	for rows.Next() {
		violations++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if violations > 0 {
		return nil, fmt.Errorf("PRAGMA foreign_key_check: %d violation(s)", violations)
	}
	return []string{"sqlite integrity_check: ok", "sqlite foreign_key_check: 0 violations"}, nil
}
