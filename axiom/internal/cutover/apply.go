// apply.go — the mutating apply paths of the window: delta tombstones
// on the target (deletes in reverse dependency order), and the
// rollback's shadow-table apply on the legacy side (idempotent,
// replace-on-diff — re-runnable to convergence without duplicates).
package cutover

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/databundle"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite"
)

// ShadowTablePrefix namespaces the rollback's catch-up tables on the
// legacy database: cutover_shadow_<table>, structurally LIKE their
// live twin (types and nullability), plus the apply bookkeeping
// columns. No constraints, no cross-table FKs — reversed rows may
// reference entities that exist only on the new store.
const ShadowTablePrefix = "cutover_shadow_"

// ShadowApplyResult is the apply's witness (counts, per table).
type ShadowApplyResult struct {
	Tables []ShadowTableApply `json:"tables"`
}

// ShadowTableApply is one shadow table's outcome.
type ShadowTableApply struct {
	Table      string `json:"table"`
	Applied    int64  `json:"applied"`    // inserted or replaced
	Idempotent int64  `json:"idempotent"` // identical on re-run — skipped
	Rows       int64  `json:"rows"`
}

// decodeKeys decodes tombstone canonical keys into typed wire values
// using the table's index (columns + key), for DELETE-by-key applies.
func decodeKeyVals(t *databundle.TableIndex, key string) ([]any, error) {
	line := t.Rows[key]
	vals, err := databundle.DecodeRow([]byte(line), t.Columns, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("table %s: decode tombstone row: %w", t.Table, err)
	}
	idx := map[string]int{}
	for i, c := range t.Columns {
		idx[c.Name] = i
	}
	out := make([]any, len(t.Key))
	for i, k := range t.Key {
		out[i] = vals[idx[k]]
	}
	return out, nil
}

// applyTombstones deletes tombstoned keys from the target, children
// before parents (reverse LibraryTables order). Idempotent: a key
// already gone deletes zero rows.
func applyTombstones(ctx context.Context, tgt *TargetHandle, tombstones map[string][]string, base *databundle.SourceIndex) (map[string]int64, error) {
	applied := map[string]int64{}
	for i := len(databundle.LibraryTables) - 1; i >= 0; i-- {
		spec := databundle.LibraryTables[i]
		keys, ok := tombstones[spec.Name]
		if !ok || len(keys) == 0 {
			continue
		}
		t := base.Tables[spec.Name]
		if t == nil {
			return applied, fmt.Errorf("tombstones for %s but the baseline index lacks the table", spec.Name)
		}
		var n int64
		for _, key := range keys {
			vals, err := decodeKeyVals(t, key)
			if err != nil {
				return applied, err
			}
			affected, err := tgt.deleteByKey(ctx, spec.Name, t.Key, vals)
			if err != nil {
				return applied, err
			}
			n += affected
		}
		applied[spec.Name] = n
	}
	return applied, nil
}

// TargetHandle is the delta-import gate's target abstraction: PG pool
// or SQLite file. It carries ONLY what the window needs beyond
// databundle.Import itself: key deletes and counts.
type TargetHandle struct {
	DSN        string
	SQLitePath string
	pool       *pgxpool.Pool
}

// OpenTarget connects the target for tombstone applies.
func OpenTarget(ctx context.Context, t Target) (*TargetHandle, error) {
	if t.DSN != "" {
		pool, err := pgxpool.New(ctx, t.DSN)
		if err != nil {
			return nil, fmt.Errorf("target open: %w", err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("target ping: %w", err)
		}
		return &TargetHandle{DSN: t.DSN, pool: pool}, nil
	}
	return &TargetHandle{SQLitePath: t.SQLitePath}, nil
}

// count returns the table's row count (the baseline precheck).
func (h *TargetHandle) count(ctx context.Context, table string) (int64, error) {
	if h.pool != nil {
		var n int64
		if err := h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, pgIdent(table))).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}
	db, err := openSQLiteRW(ctx, h.SQLitePath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int64
	if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, pgIdent(table))).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Close releases the target handle.
func (h *TargetHandle) Close() {
	if h.pool != nil {
		h.pool.Close()
	}
}

func (h *TargetHandle) deleteByKey(ctx context.Context, table string, key []string, vals []any) (int64, error) {
	where := make([]string, len(key))
	for i, k := range key {
		where[i] = fmt.Sprintf(`%s = $%d`, pgIdent(k), i+1)
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE %s`, pgIdent(table), strings.Join(where, " AND "))
	if h.pool != nil {
		tag, err := h.pool.Exec(ctx, q, vals...)
		if err != nil {
			return 0, fmt.Errorf("tombstone %s: %w", table, err)
		}
		return tag.RowsAffected(), nil
	}
	// SQLite wire values (timestamps bind as the canonical string —
	// key columns are ids/keys/seqs, never timestamps, but stay safe)
	db, err := openSQLiteRW(ctx, h.SQLitePath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	wire := make([]any, len(vals))
	for i, v := range vals {
		if t, ok := v.(time.Time); ok {
			wire[i] = databundle.CanonicalTimestamp(t)
			continue
		}
		wire[i] = v
	}
	res, err := db.ExecContext(ctx, q, wire...)
	if err != nil {
		return 0, fmt.Errorf("tombstone %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// pgIdent quotes one identifier (the databundle helper's twin).
func pgIdent(name string) string { return pgx.Identifier{name}.Sanitize() }

// applyReverseDelta lands the reverse-delta bundle's rows into the
// cutover_shadow_* tables on the legacy database. Idempotent per
// stable key: an identical existing row is skipped; a differing one is
// REPLACED (the shadow is a catch-up surface, re-runnable until the
// operator is satisfied — never a merge-conflict abort).
func applyReverseDelta(ctx context.Context, pool *pgxpool.Pool, rev *databundle.SourceIndex, runID string) (*ShadowApplyResult, error) {
	res := &ShadowApplyResult{}
	for _, spec := range databundle.LibraryTables {
		t, ok := rev.Tables[spec.Name]
		if !ok || len(t.Rows) == 0 {
			continue
		}
		shadow := ShadowTablePrefix + spec.Name
		// Structural contract: the shadow mirrors the live table's
		// column set exactly (LIKE), plus the two bookkeeping columns.
		if err := ensureShadowTable(ctx, pool, spec.Name, shadow); err != nil {
			return res, err
		}
		liveCols, err := poolColumms(ctx, pool, spec.Name)
		if err != nil {
			return res, err
		}
		if !sameColumns(liveCols, t.Columns) {
			return res, fmt.Errorf("shadow %s: live table columns and reverse-delta columns differ — the legacy schema and the new store drifted; refusing a lossy apply", spec.Name)
		}
		keyIdx := map[string]int{}
		for i, c := range t.Columns {
			keyIdx[c.Name] = i
		}
		sa := ShadowTableApply{Table: spec.Name}
		// Deterministic order: canonical key.
		keys := make([]string, 0, len(t.Rows))
		for k := range t.Rows {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			vals, err := databundle.DecodeRow([]byte(t.Rows[k]), t.Columns, nil, 0)
			if err != nil {
				return res, fmt.Errorf("shadow %s: %w", spec.Name, err)
			}
			keyVals := make([]any, len(t.Key))
			for i, kc := range t.Key {
				keyVals[i] = vals[keyIdx[kc]]
			}
			same, err := shadowRowIdentical(ctx, pool, shadow, t, k, keyVals)
			if err != nil {
				return res, err
			}
			if same {
				sa.Idempotent++
				continue
			}
			if err := shadowUpsert(ctx, pool, shadow, t, vals, keyVals, runID); err != nil {
				return res, err
			}
			sa.Applied++
		}
		sa.Rows = sa.Applied + sa.Idempotent
		res.Tables = append(res.Tables, sa)
	}
	return res, nil
}

// ensureShadowTable creates the shadow table + key index if absent
// (LIKE the live table: types + nullability + defaults, deliberately
// WITHOUT constraints, FKs, or indexes — reversed rows may reference
// entities that only ever existed on the new store).
func ensureShadowTable(ctx context.Context, pool *pgxpool.Pool, live, shadow string) error {
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (LIKE %s INCLUDING DEFAULTS, applied_at TIMESTAMPTZ NOT NULL DEFAULT now(), applied_run TEXT NOT NULL DEFAULT '')`,
		pgIdent(shadow), pgIdent(live))); err != nil {
		return fmt.Errorf("create %s: %w", shadow, err)
	}
	return nil
}

// poolColumms reads a table's column catalog (name + canonical tag)
// over the maintenance pool's connection.
func poolColumms(ctx context.Context, pool *pgxpool.Pool, table string) ([]databundle.ColumnRef, error) {
	rows, err := pool.Query(ctx, `
		SELECT column_name, data_type, udt_name
		  FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = $1
		 ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []databundle.ColumnRef
	for rows.Next() {
		var name, dataType, udt string
		if err := rows.Scan(&name, &dataType, &udt); err != nil {
			return nil, err
		}
		tag, err := databundle.TagOfPG(dataType, udt)
		if err != nil {
			return nil, fmt.Errorf("table %s column %s: %w", table, name, err)
		}
		out = append(out, databundle.ColumnRef{Name: name, Type: tag})
	}
	return out, rows.Err()
}

// shadowRowIdentical reports whether the shadow row's canonical form
// equals the incoming row (skip on re-run).
func shadowRowIdentical(ctx context.Context, pool *pgxpool.Pool, shadow string, t *databundle.TableIndex, canonicalKey string, keyVals []any) (bool, error) {
	where := make([]string, len(t.Key))
	args := make([]any, len(t.Key))
	for i, k := range t.Key {
		where[i] = fmt.Sprintf(`%s = $%d`, pgIdent(k), i+1)
		args[i] = keyVals[i]
	}
	selectList := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		selectList[i] = pgIdent(c.Name)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s`, strings.Join(selectList, ", "), pgIdent(shadow), strings.Join(where, " AND "))
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("shadow read %s: %w", shadow, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return false, nil
	}
	vals, err := databundle.ScanPGRow(rows, t.Columns)
	if err != nil {
		return false, err
	}
	var line bytes.Buffer
	if err := databundle.EncodeRow(&line, t.Columns, vals); err != nil {
		return false, err
	}
	got := strings.TrimSuffix(line.String(), "\n")
	return got == t.Rows[canonicalKey], nil
}

// shadowUpsert lands one row: DELETE by key + INSERT with bookkeeping
// (replace-on-diff; no unique-violation window — same transaction).
func shadowUpsert(ctx context.Context, pool *pgxpool.Pool, shadow string, t *databundle.TableIndex, vals, keyVals []any, runID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	where := make([]string, len(t.Key))
	for i, k := range t.Key {
		where[i] = fmt.Sprintf(`%s = $%d`, pgIdent(k), i+1)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s`, pgIdent(shadow), strings.Join(where, " AND ")), keyVals...); err != nil {
		return fmt.Errorf("shadow replace %s: %w", shadow, err)
	}
	cols := make([]string, 0, len(t.Columns)+2)
	placeholders := make([]string, 0, len(t.Columns)+2)
	args := make([]any, 0, len(t.Columns)+2)
	for i, c := range t.Columns {
		cols = append(cols, pgIdent(c.Name))
		placeholders = append(placeholders, fmt.Sprintf(`$%d`, len(args)+1))
		args = append(args, vals[i])
	}
	cols = append(cols, "applied_run")
	placeholders = append(placeholders, fmt.Sprintf(`$%d`, len(args)+1))
	args = append(args, runID)
	q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, pgIdent(shadow), strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("shadow insert %s: %w", shadow, err)
	}
	return tx.Commit(ctx)
}

// openSQLiteRW opens a library.sqlite for the window's bounded writes
// (tombstone deletes). It does NOT migrate and does not create a
// missing file — the delta-import gate owns creation.
func openSQLiteRW(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("sqlite open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
