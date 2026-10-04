// pg.go — the PostgreSQL end of bundle operations: the exporter (the
// source database is PostgreSQL — the legacy world lives there) and the
// PostgreSQL sink for import/verify. All catalog truth comes from the
// engine's own catalogs (information_schema, pg_enum, pg_index) — no
// hard-coded schema drift.
package databundle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgDB is a thin pool wrapper with the catalog vocabulary.
type pgDB struct {
	pool *pgxpool.Pool
}

func openPG(ctx context.Context, dsn string) (*pgDB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("data bundle: a PostgreSQL DSN is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &pgDB{pool: pool}, nil
}

func (p *pgDB) Close() { p.pool.Close() }

// engineVersion is the manifest's engine identity line.
func (p *pgDB) engineVersion(ctx context.Context) (string, error) {
	return engineVersionQ(ctx, p.pool)
}

// engineVersionInTx reads the identity INSIDE the export snapshot (the
// manifest's provenance must describe the same database state as the
// exported rows).
func (p *pgDB) engineVersionInTx(ctx context.Context, q pgQuerier) (string, error) {
	return engineVersionQ(ctx, q)
}

func engineVersionQ(ctx context.Context, q pgQuerier) (string, error) {
	var v string
	err := q.QueryRow(ctx, `SELECT version()`).Scan(&v)
	if err != nil {
		return "", err
	}
	if i := strings.IndexByte(v, ','); i > 0 {
		v = v[:i] // "PostgreSQL 16.9" — drop the platform tail
	}
	return v, nil
}

// migrationLedgers snapshots every migration ledger that exists — the
// manifest's migration-state provenance.
func (p *pgDB) migrationLedgers(ctx context.Context) (map[string][]string, error) {
	return migrationLedgersQ(ctx, p.pool)
}

// migrationLedgersInTx snapshots every ledger INSIDE the export
// snapshot (same-state guarantee as the rows).
func (p *pgDB) migrationLedgersInTx(ctx context.Context, q pgQuerier) (map[string][]string, error) {
	return migrationLedgersQ(ctx, q)
}

func migrationLedgersQ(ctx context.Context, q pgQuerier) (map[string][]string, error) {
	ledgers := []string{"schema_migrations", "library_schema_migrations", "store_schema_migrations"}
	out := map[string][]string{}
	for _, l := range ledgers {
		var exists bool
		if err := q.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1)`, l,
		).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT version FROM %s ORDER BY version`, pgx.Identifier{l}.Sanitize()))
		if err != nil {
			return nil, err
		}
		var vers []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			vers = append(vers, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out[l] = vers
	}
	return out, nil
}

// tagOfPG maps an information_schema type to a canonical tag (the
// catalog spells types in lower case — 'uuid', 'timestamp with time
// zone', 'USER-DEFINED'). Unknown types are a loud refusal — the
// format stays portable or stops.
func tagOfPG(dataType, udtName string) (string, error) {
	switch strings.ToLower(dataType) {
	case "uuid":
		return TagUUID, nil
	case "text", "character varying", "character":
		return TagText, nil
	case "timestamp with time zone":
		return TagTimestamp, nil
	case "boolean":
		return TagBool, nil
	case "smallint", "integer", "bigint":
		return TagInt64, nil
	case "real", "double precision":
		return TagFloat64, nil
	case "json", "jsonb":
		return TagJSONB, nil
	case "numeric":
		return TagNumeric, nil
	case "user-defined":
		return TagEnum, nil // pg_enum vocabulary attached separately
	default:
		return "", fmt.Errorf("PostgreSQL type %q (udt %q) has no canonical mapping — refusing a lossy export", dataType, udtName)
	}
}

// pgCatalog describes one table as the engine sees it.
type pgCatalog struct {
	table   string
	columns []ColumnManifest
	key     []string
	enums   map[string][]string
}

// pgQuerier is the shared query surface of pool and transaction — the
// catalog reads run over either (export snapshots run in-tx).
type pgQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// catalog reads a table's full catalog from the engine. Absent table →
// (nil, nil).
func (p *pgDB) catalog(ctx context.Context, table string) (*pgCatalog, error) {
	return catalogQuery(ctx, p.pool, table)
}

// catalogQuery is catalog() over any pgQuerier (pool or snapshot tx).
func catalogQuery(ctx context.Context, q pgQuerier, table string) (*pgCatalog, error) {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1 AND table_type = 'BASE TABLE')`, table,
	).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT column_name, data_type, udt_name, is_nullable
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	cat := &pgCatalog{table: table, enums: map[string][]string{}}
	type enumCol struct{ name, udt string }
	var enumCols []enumCol
	for rows.Next() {
		var name, dataType, udt, nullable string
		if err := rows.Scan(&name, &dataType, &udt, &nullable); err != nil {
			rows.Close()
			return nil, err
		}
		tag, err := tagOfPG(dataType, udt)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("table %s: %w", table, err)
		}
		cat.columns = append(cat.columns, ColumnManifest{Name: name, Type: tag, Nullable: nullable == "YES"})
		if tag == TagEnum {
			enumCols = append(enumCols, enumCol{name, udt})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // one connection per tx — never query with rows open
	for _, ec := range enumCols {
		vocab, err := pgEnumVocab(ctx, q, ec.udt)
		if err != nil {
			return nil, fmt.Errorf("table %s enum %s: %w", table, ec.udt, err)
		}
		cat.enums[ec.name] = vocab
	}
	krows, err := q.Query(ctx, `
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = current_schema() AND tc.table_name = $1
		ORDER BY kcu.ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer krows.Close()
	for krows.Next() {
		var k string
		if err := krows.Scan(&k); err != nil {
			return nil, err
		}
		cat.key = append(cat.key, k)
	}
	if err := krows.Err(); err != nil {
		return nil, err
	}
	return cat, nil
}

// pgEnumVocab reads an enum type's vocabulary (source-declared truth
// for the manifest) over any querier (snapshot tx included).
func pgEnumVocab(ctx context.Context, q pgQuerier, udtName string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT e.enumlabel FROM pg_type t
		JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE t.typname = $1
		ORDER BY e.enumsortorder`, udtName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// pgSelectList builds the explicit column list (catalog order); uuid
// and numeric are cast ::text so the value arrives as the canonical
// string form.
func pgSelectList(cols []ColumnRef) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		switch c.Type {
		case TagUUID, TagNumeric:
			parts[i] = fmt.Sprintf(`%s::text AS %s`, pgx.Identifier{c.Name}.Sanitize(), pgx.Identifier{c.Name}.Sanitize())
		default:
			parts[i] = pgx.Identifier{c.Name}.Sanitize()
		}
	}
	return strings.Join(parts, ", ")
}

// pgScanDests builds typed scan destinations per tag (pgx decodes into
// exactly these); every destination is POINTER-TO-POINTER — nil means
// SQL NULL regardless of the declared nullability (nullable BIGINTs
// and TEXTs both exist in the Library schema), mapped to JSON null.
func pgScanDests(cols []ColumnRef) []any {
	dests := make([]any, len(cols))
	for i, c := range cols {
		switch c.Type {
		case TagTimestamp:
			dests[i] = new(*time.Time)
		case TagBool:
			dests[i] = new(*bool)
		case TagInt64:
			dests[i] = new(*int64)
		case TagFloat64:
			dests[i] = new(*float64)
		case TagJSONB:
			dests[i] = new(*[]byte)
		default: // uuid, text, enum, numeric — all text-carried
			dests[i] = new(*string)
		}
	}
	return dests
}

// scanValue dereferences one typed scan destination into its value.
func scanValue(p any) any {
	switch v := p.(type) {
	case **time.Time:
		if *v == nil {
			return nil
		}
		return **v
	case **string:
		if *v == nil {
			return nil
		}
		return **v
	case **bool:
		if *v == nil {
			return nil
		}
		return **v
	case **int64:
		if *v == nil {
			return nil
		}
		return **v
	case **float64:
		if *v == nil {
			return nil
		}
		return **v
	case **[]byte:
		if *v == nil {
			return nil
		}
		return **v
	}
	return nil
}

// pgScanRow scans the current row into freshly allocated values in
// catalog order.
func pgScanRow(rows pgx.Rows, cols []ColumnRef) ([]any, error) {
	dests := pgScanDests(cols)
	if err := rows.Scan(dests...); err != nil {
		return nil, err
	}
	vals := make([]any, len(dests))
	for i, d := range dests {
		vals[i] = scanValue(d)
	}
	return vals, nil
}

// pgWire converts decoded canonical values to pgx insert parameters.
func pgWire(cols []ColumnRef, vals []any) []any {
	out := make([]any, len(vals))
	for i, c := range cols {
		v := vals[i]
		if c.Type == TagJSONB {
			if b, ok := v.([]byte); ok {
				out[i] = string(b)
				continue
			}
		}
		out[i] = v
	}
	return out
}

// errSinkAbsent is the sink's row-absent signal (rowByPK).
var errSinkAbsent = errors.New("sink: row absent")

// errPGNoRows folds pgx.ErrNoRows for the sink's rowByPK contract.
func errPGNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errSinkAbsent
	}
	return err
}
