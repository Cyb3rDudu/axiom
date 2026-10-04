// shadow.go — the DM08 (#317) shadow-read: the legacy mirror copy (the
// pull-point source) against the imported Library copy, over the FULL
// library data set, with every deviation classified against an
// EXPLICIT normalization allowlist — anything not covered is
// unexpected (red). Read-only on both sides; the source read is pinned
// to one REPEATABLE READ READ ONLY snapshot so a run is deterministic
// against a live mirror (the pull point is the snapshot, recorded in
// the report). The comparison is canonical and per-column: identical
// data through identical canonical encodings is byte-equal, so the
// comparison has the same teeth as the DM04 digests — plus field-level
// classification a digest cannot do. Semantic surfaces (issue spec):
// records, collections, renditions, selections, repair readback, raw
// envelopes — every Library table rides one of these groups; the full
// set is compared, not a sample.
package databundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/version"
	"github.com/jackc/pgx/v5"
)

// ShadowOptions parameterize a shadow run. Exactly one target engine.
type ShadowOptions struct {
	SourceDSN  string // legacy mirror copy (the pull-point source)
	DSN        string // imported copy, PostgreSQL target
	SQLitePath string // imported copy, library.sqlite target
	Out        string // report artifact path (JSON); "" = no file
	MaxSamples int    // per-surface sample rows in the report; 0 = default
}

// shadowDefaultSamples bounds report size; counts stay exact regardless.
const shadowDefaultSamples = 20

// ---------------------------------------------------------------------------
// Report shape (the committed artifact)

// FieldDiff is one differing column of one compared row. Values are
// value digests (12 hex), never row values — the data family's
// no-leaks discipline (failures name tables, columns, counts, digests
// and key identifiers only).
type FieldDiff struct {
	Column string `json:"column"`
	Source string `json:"source_digest"`
	Target string `json:"target_digest"`
	// Rule: the allowlist rule that absorbed this difference ("" when
	// the difference is unexpected).
	Rule string `json:"rule,omitempty"`
}

// RowDiff is one deviating row (or a structural deviation, Key="schema").
type RowDiff struct {
	Key    string      `json:"key"`
	Kind   string      `json:"kind"` // field_diff | missing_on_target | extra_on_target | structural
	Fields []FieldDiff `json:"fields,omitempty"`
	Note   string      `json:"note,omitempty"`
}

// SurfaceResult is one table's comparison verdict.
type SurfaceResult struct {
	// Surface: the semantic group from the issue spec (records,
	// collections, renditions, selections, repair-readback,
	// raw-envelopes, …) — one row per Library table, grouped for the
	// operator.
	Surface string `json:"surface"`
	Table   string `json:"table"`
	Status  string `json:"status"` // compared | skipped
	Note    string `json:"note,omitempty"`

	SourceRows int64 `json:"source_rows"`
	TargetRows int64 `json:"target_rows"`
	Compared   int64 `json:"compared"` // rows present on both sides
	Equal      int64 `json:"equal"`
	// Normalized: rows whose differences were ALL absorbed by explicit
	// allowlist rules (each absorption counted per rule in the report's
	// allowlist; sample rows carry the rule ids).
	Normalized int64 `json:"normalized"`
	Unexpected int64 `json:"unexpected"`
	// MissingOnTarget / ExtraOnTarget: stable keys present on one side
	// only — structural, always unexpected.
	MissingOnTarget int64     `json:"missing_on_target"`
	ExtraOnTarget   int64     `json:"extra_on_target"`
	Samples         []RowDiff `json:"samples"`
}

// AllowlistEntry is one approved normalization — explicit, scoped,
// individually justified. Anything not covered here (or by the
// canonical read encodings below) is unexpected.
type AllowlistEntry struct {
	ID            string `json:"id"`
	Scope         string `json:"scope"`
	Justification string `json:"justification"`
	Applied       int64  `json:"applied"` // absorptions this run
}

// ShadowReport is the operator-facing outcome and the committed
// evidence artifact.
type ShadowReport struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	Build         string `json:"build"`
	GeneratedAt   string `json:"generated_at"`
	// SourceCutoff: the pull point — the source snapshot's now(),
	// canonical form. Both sides derive from this point (the target is
	// the bundle import of the same pull), which is what makes the
	// comparison deterministic.
	SourceCutoff string `json:"source_cutoff"`
	SourceEngine string `json:"source_engine"`
	TargetEngine string `json:"target_engine"`
	// TargetCutoff: the PG target snapshot's now() (canonical form) —
	// with SourceCutoff it evidences the both-sides-one-pull-point
	// premise; empty for a SQLite file target (a file copy carries no
	// clock; its pull point is the export the import consumed).
	TargetCutoff string          `json:"target_cutoff,omitempty"`
	TargetMode   string          `json:"target_mode"` // snapshot read | file read
	Tables       []SurfaceResult `json:"tables"`
	// Canonicalization: read-time encodings applied to BOTH sides by
	// construction (differences of these kinds cannot appear as diffs —
	// listed so the operator sees the full normalization surface).
	Canonicalization []AllowlistEntry `json:"canonicalization"`
	// Allowlist: comparison-time absorption rules. Each absorption is
	// visible (per-rule count + sample rows); extending this list
	// requires review — a rule that hides a real deviation is exactly
	// the drift this tool exists to catch.
	Allowlist []AllowlistEntry `json:"allowlist"`
	Warnings  []string         `json:"warnings"`
	OK        bool             `json:"ok"`
}

// ShadowSurfaceOf maps a Library table to its semantic surface (the
// issue's comparison vocabulary). Every LibraryTables member has
// exactly one group — the "full library data set" promise.
func ShadowSurfaceOf(table string) string {
	switch table {
	case "zotero_documents":
		return "records"
	case "zotero_collections", "zotero_item_collections":
		return "collections"
	case "zotero_attachments":
		return "renditions"
	case "zotero_selections", "zotero_collection_selections":
		return "selections"
	case "repair_cases":
		return "repair-readback"
	case "zotero_items":
		return "raw-envelopes"
	case "zotero_sources":
		return "sources"
	case "zotero_write_audit":
		return "legacy-write-audit"
	default:
		return "library-acquisition" // library_* namespace (imports, events, steps, ids, provenance, revisions, anchors, audit, leases)
	}
}

// shadowCanonicalization documents the read-time encodings (applied by
// the shared canonical codec to both sides — construction-time
// normalization, differences of these kinds never surface as diffs).
var shadowCanonicalization = []AllowlistEntry{
	{ID: "timestamp-utc-micros", Scope: "column tag timestamp",
		Justification: "Timestamps encode to UTC RFC3339 with fixed microseconds on both engines; timezone rendering and sub-microsecond precision differences are not semantic."},
	{ID: "json-canonical-shape", Scope: "column tag jsonb",
		Justification: "jsonb has no key-order contract on any engine; both sides encode through permutation-stable canonical JSON (sorted keys, compact separators). Key order and whitespace are not semantic."},
	{ID: "bool-integer-fold", Scope: "column tag bool",
		Justification: "SQLite carries booleans as 0/1 integers, PostgreSQL as true/false; both fold to the canonical boolean."},
	{ID: "uuid-case-form", Scope: "column tag uuid",
		Justification: "UUIDs encode to the lowercase hyphenated form; case differences are not semantic."},
	{ID: "decimal-token-verbatim", Scope: "column tag numeric",
		Justification: "Numeric values travel as verbatim decimal tokens (no float64 round-trip); scale is preserved exactly as each engine renders it — value differences are compared by the numeric-value rule below."},
}

// shadowAllowlist is THE comparison-time absorption list. Adding an
// entry is a reviewed decision (issue + docs), never a run-time
// discovery: an unmatched difference fails the run.
var shadowAllowlist = []AllowlistEntry{
	{ID: "numeric-value", Scope: "column tag numeric",
		Justification: "Lexical decimal forms of the same value (trailing zeros, exponent expansion) differ between engine renderings; the numbers compare by exact rational value. Semantics: the stored quantity, not its spelling."},
	{ID: "float-value", Scope: "column tag float64",
		Justification: "IEEE-754 doubles compare by parsed value, tolerating lexical rendering differences ('1e-06' vs '0.000001'). Same bits are guaranteed by the scan path; this rule is the belt."},
	{ID: "timestamp-instant", Scope: "column tag timestamp",
		Justification: "Canonical forms differing while denoting the same instant (a future engine writing a different fixed precision) compare by instant. The canonical form is identical today; this rule is the belt."},
	{ID: "json-number-value", Scope: "column tag jsonb",
		Justification: "Inside JSON documents, number tokens compare by value (exact rational), not lexically: PostgreSQL jsonb re-renders numeric tokens through its numeric type (e.g. exponent expansion) while SQLite stores the imported text verbatim — equal numbers may spell differently. Structure, strings and booleans remain strictly byte-compared after canonicalization."},
}

// Deliberately NO id-remap entry: the DM04 import preserves stable-key
// ids verbatim (idempotent per stable key), so internal ids do NOT get
// re-minted across the migration — a differing id IS an unexpected
// diff. If a future migration mints ids, that future adds the entry
// through review, not this tool quietly.

// ---------------------------------------------------------------------------
// Source / target readers

// pgSnapshot reads one PostgreSQL database pinned to a single
// REPEATABLE READ READ ONLY snapshot (the pull point).
type pgSnapshot struct {
	db     *pgDB
	tx     pgx.Tx
	cutoff time.Time
}

func openPGSnapshot(ctx context.Context, dsn string) (*pgSnapshot, error) {
	db, err := openPG(ctx, dsn)
	if err != nil {
		return nil, err
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY`); err != nil {
		tx.Rollback(ctx)
		db.Close()
		return nil, fmt.Errorf("set snapshot isolation: %w", err)
	}
	var cutoff time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&cutoff); err != nil {
		tx.Rollback(ctx)
		db.Close()
		return nil, err
	}
	return &pgSnapshot{db: db, tx: tx, cutoff: cutoff}, nil
}

func (p *pgSnapshot) close() error {
	err := p.tx.Rollback(context.Background()) // read-only: rollback is the clean end
	p.db.Close()
	return err
}

// readTable reads every row of table (canonicalized under cols, keyed
// by the stable key columns) into memory. ponytail: full-table map in
// memory — personal-library scale (thousands of rows); stream-merge if
// a future corpus outgrows it.
// openSQLiteShadow opens the imported library.sqlite for READING
// only — never the import path's opener: that one creates missing
// files, sets WAL and applies migrations, all of which would betray
// the shadow's read-only premise (a target on an older schema would
// be silently brought to head instead of surfacing as structural
// drift, and a mistyped path would mint a fresh empty database).
// mode=ro refuses every write INCLUDING file creation; query_only(1)
// is the belt. A missing path fails loudly, before any open.
func openSQLiteShadow(path string) (*sqliteSink, error) {
	if path == "" {
		return nil, fmt.Errorf("data bundle: a SQLite path is required")
	}
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("cannot stat shadow target %s: %w", path, err)
		}
		return nil, fmt.Errorf("shadow target %s does not exist — the shadow reads a frozen imported copy, it never creates one", path)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)&_pragma=query_only(1)", path, sqliteBusyTimeoutMs))
	if err != nil {
		return nil, fmt.Errorf("open read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &sqliteSink{db: db}, nil
}

func (p *pgSnapshot) readTable(ctx context.Context, table string, cols []ColumnRef, keyCols []string) (map[string]string, int64, error) {
	rows, err := p.tx.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s`,
		pgSelectList(cols), pgIdent(table)))
	if err != nil {
		return nil, 0, fmt.Errorf("stream %s: %w", table, err)
	}
	defer rows.Close()
	out := make(map[string]string)
	var line bytes.Buffer
	keyIdx, err := keyIndices(cols, keyCols)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		vals, err := pgScanRow(rows, cols)
		if err != nil {
			return nil, 0, fmt.Errorf("scan %s: %w", table, err)
		}
		key, err := canonicalKeyAt(cols, keyIdx, vals)
		if err != nil {
			return nil, 0, err
		}
		if _, dup := out[key]; dup {
			return nil, 0, fmt.Errorf("table %s: stable key %s is not unique — the comparison refuses a lossy read", table, redactKey(key))
		}
		line.Reset()
		if err := EncodeRow(&line, cols, vals); err != nil {
			return nil, 0, fmt.Errorf("encode %s: %w", table, err)
		}
		out[key] = line.String()
	}
	return out, int64(len(out)), rows.Err()
}

// sqliteReadTable is the SQLite-file read (a frozen imported copy — no
// concurrent writer domain to pin).
func sqliteReadTable(ctx context.Context, s *sqliteSink, table string, cols []ColumnRef, keyCols []string) (map[string]string, int64, error) {
	out := make(map[string]string)
	var line bytes.Buffer
	keyIdx, err := keyIndices(cols, keyCols)
	if err != nil {
		return nil, 0, err
	}
	err = s.streamOrdered(ctx, table, cols, keyCols, func(vals []any) error {
		key, err := canonicalKeyAt(cols, keyIdx, vals)
		if err != nil {
			return err
		}
		if _, dup := out[key]; dup {
			return fmt.Errorf("table %s: stable key %s is not unique — the comparison refuses a lossy read", table, redactKey(key))
		}
		line.Reset()
		if err := EncodeRow(&line, cols, vals); err != nil {
			return err
		}
		out[key] = line.String()
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("stream %s: %w", table, err)
	}
	return out, int64(len(out)), nil
}

// canonicalKey encodes the stable-key values canonically and joins
// them — a collision-free row identity under the canonical encodings.
// (Name-keyed convenience; the table readers precompute the key index
// once and call canonicalKeyAt per row.)
func canonicalKey(cols []ColumnRef, keyCols []string, vals []any) (string, error) {
	idx, err := keyIndices(cols, keyCols)
	if err != nil {
		return "", err
	}
	return canonicalKeyAt(cols, idx, vals)
}

func canonicalKeyAt(cols []ColumnRef, idx []int, vals []any) (string, error) {
	var buf bytes.Buffer
	for i, ix := range idx {
		if i > 0 {
			buf.WriteByte(0x1f)
		}
		if err := encodeTagged(&buf, cols[ix].Type, vals[ix]); err != nil {
			return "", fmt.Errorf("key column %s: %w", cols[ix].Name, err)
		}
	}
	return buf.String(), nil
}

// ---------------------------------------------------------------------------
// Comparison

// Shadow runs the shadow-read comparison over the full library data
// set and returns the report (OK = zero unexpected deviations).
func Shadow(ctx context.Context, opts ShadowOptions) (rep *ShadowReport, err error) {
	if opts.SourceDSN == "" {
		return nil, fmt.Errorf("shadow: --source-dsn (the legacy mirror copy) is required")
	}
	if (opts.DSN == "") == (opts.SQLitePath == "") {
		return nil, fmt.Errorf("shadow: exactly one of DSN (imported PostgreSQL copy) or SQLitePath (imported library.sqlite) is required")
	}
	maxSamples := opts.MaxSamples
	if maxSamples <= 0 {
		maxSamples = shadowDefaultSamples
	}
	build := version.Banner()

	src, err := openPGSnapshot(ctx, opts.SourceDSN)
	if err != nil {
		return nil, fmt.Errorf("source open: %w", err)
	}
	defer src.close()

	rep = &ShadowReport{
		Format:           "axiom-shadow-report",
		FormatVersion:    1,
		Build:            build,
		GeneratedAt:      canonicalTimestamp(time.Now().UTC()),
		SourceCutoff:     canonicalTimestamp(src.cutoff),
		SourceEngine:     "postgresql",
		Warnings:         []string{},
		Canonicalization: shadowCanonicalization,
		Allowlist:        newShadowAllowlist(),
	}
	// OK means: at least one surface compared, and zero unexpected
	// deviations anywhere. A run that compared nothing is not green.
	rep.OK = false
	comparedAny := false
	// An aborted run still leaves its evidence: when --out is set and
	// the run fails after this point, a best-effort failure report
	// lands at the artifact path (OK=false, the abort reason in the
	// warnings — the cutover window keeps its paper trail). Failures
	// BEFORE the report exists (argument validation, source open)
	// deliberately write nothing: loud stderr/exit instead of
	// destroying a previous run's artifact.
	written := false
	defer func() {
		if err == nil || opts.Out == "" || written || rep == nil {
			return
		}
		rep.OK = false
		rep.Warnings = append(rep.Warnings, "shadow aborted: "+err.Error())
		if b, jerr := json.MarshalIndent(rep, "", "  "); jerr == nil {
			_ = writeFileSync(opts.Out, append(b, '\n'))
		}
	}()

	var tgtSnap *pgSnapshot
	var tgtSQLite *sqliteSink
	if opts.DSN != "" {
		tgtSnap, err = openPGSnapshot(ctx, opts.DSN)
		if err != nil {
			return rep, fmt.Errorf("target open: %w", err)
		}
		defer tgtSnap.close()
		rep.TargetEngine = "postgresql"
		rep.TargetMode = "snapshot read (REPEATABLE READ READ ONLY)"
		rep.TargetCutoff = canonicalTimestamp(tgtSnap.cutoff)
	} else {
		tgtSQLite, err = openSQLiteShadow(opts.SQLitePath)
		if err != nil {
			return rep, fmt.Errorf("target open: %w", err)
		}
		defer tgtSQLite.close()
		rep.TargetEngine = "sqlite"
		rep.TargetMode = "file read (mode=ro, query_only — frozen imported copy; no migration, no writes)"
	}

	for _, spec := range LibraryTables {
		sr := SurfaceResult{Surface: ShadowSurfaceOf(spec.Name), Table: spec.Name, Samples: []RowDiff{}}

		// The comparison set is the SOURCE catalog (name + canonical
		// tag); the pull-point gate runs BEFORE any target scope rule:
		// a table the source does not carry is red on EVERY target
		// engine (the shadow requires the full library data set).
		cat, err := catalogQuery(ctx, src.tx, spec.Name)
		if err != nil {
			return rep, fmt.Errorf("source catalog %s: %w", spec.Name, err)
		}
		if cat == nil {
			// The shadow is a GATE over the FULL library data set: a
			// table the pull-point source does not carry is a red
			// structural deviation (a partial restore must not pass as a
			// green run). Export may warn about a migration stand — this
			// tool refuses one.
			sr.Status = "compared"
			sr.Unexpected = 1
			sr.Samples = append(sr.Samples, RowDiff{Key: "schema", Kind: "structural",
				Note: "table absent at the source — the shadow requires the full library data set at the pull point"})
			rep.Tables = append(rep.Tables, sr)
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("table %s absent at the source", spec.Name))
			continue
		}
		cols := make([]ColumnRef, len(cat.columns))
		for i, c := range cat.columns {
			cols[i] = ColumnRef{Name: c.Name, Type: c.Type}
		}

		// F12 scope rule (target namespace), AFTER the pull-point gate:
		// the library.sqlite target carries the library_* namespace only
		// — the legacy mirror set has no SQLite home. The source must
		// carry the table on every engine; the skip documents where the
		// target cannot land it. A scope fact, not a deviation.
		if tgtSQLite != nil && !spec.SQLite {
			sr.Status = "skipped"
			sr.Note = "outside the SQLite target namespace (F12: the legacy mirror set lands on PostgreSQL targets only)"
			rep.Tables = append(rep.Tables, sr)
			continue
		}

		// Target structural check: same column names (and, PG target,
		// same canonical tags — a drifted target schema is red, not
		// mis-scanned).
		var tgtCols []ColumnRef
		if tgtSnap != nil {
			tcat, err := catalogQuery(ctx, tgtSnap.tx, spec.Name)
			if err != nil {
				return rep, fmt.Errorf("target catalog %s: %w", spec.Name, err)
			}
			if tcat == nil {
				sr.Status = "compared"
				sr.Unexpected = 1
				sr.Samples = append(sr.Samples, RowDiff{Key: "schema", Kind: "structural",
					Note: "table present at the source, absent from the target — the imported copy must carry it"})
				rep.Tables = append(rep.Tables, sr)
				continue
			}
			for _, c := range tcat.columns {
				tgtCols = append(tgtCols, ColumnRef{Name: c.Name, Type: c.Type})
			}
			if note, ok := columnSetMatches(cols, tgtCols); !ok {
				sr.Status = "compared"
				sr.Unexpected = 1
				sr.Samples = append(sr.Samples, RowDiff{Key: "schema", Kind: "structural", Note: note})
				rep.Tables = append(rep.Tables, sr)
				continue
			}
		} else {
			tgtCols, err = tgtSQLite.catalogColumns(ctx, spec.Name)
			if err != nil {
				return rep, fmt.Errorf("target catalog %s: %w", spec.Name, err)
			}
			if tgtCols == nil {
				sr.Status = "compared"
				sr.Unexpected = 1
				sr.Samples = append(sr.Samples, RowDiff{Key: "schema", Kind: "structural",
					Note: "table present at the source, absent from the target file"})
				rep.Tables = append(rep.Tables, sr)
				continue
			}
			// The SQLite schema declares engine-native types (uuid,
			// timestamps, jsonb all TEXT) — names must match; values
			// read and canonicalize under the SOURCE tags, exactly as
			// the import/verify codec does.
			if !slices.EqualFunc(cols, tgtCols, func(a, b ColumnRef) bool { return a.Name == b.Name }) {
				sr.Status = "compared"
				sr.Unexpected = 1
				sr.Samples = append(sr.Samples, RowDiff{Key: "schema", Kind: "structural",
					Note: fmt.Sprintf("column set mismatch: source %v vs target %v", columnNames(cols), columnNames(tgtCols))})
				rep.Tables = append(rep.Tables, sr)
				continue
			}
		}

		srcRows, srcCount, err := src.readTable(ctx, spec.Name, cols, spec.Keys)
		if err != nil {
			return rep, err
		}
		var tgtRows map[string]string
		var tgtCount int64
		if tgtSnap != nil {
			tgtRows, tgtCount, err = tgtSnap.readTable(ctx, spec.Name, cols, spec.Keys)
		} else {
			tgtRows, tgtCount, err = sqliteReadTable(ctx, tgtSQLite, spec.Name, cols, spec.Keys)
		}
		if err != nil {
			return rep, err
		}
		sr.Status = "compared"
		sr.SourceRows, sr.TargetRows = srcCount, tgtCount
		if err := compareRows(&sr, cols, srcRows, tgtRows, maxSamples, rep.Allowlist); err != nil {
			return rep, err
		}
		rep.Tables = append(rep.Tables, sr)
		comparedAny = true
	}

	rep.OK = comparedAny
	for _, t := range rep.Tables {
		if t.Unexpected > 0 || t.MissingOnTarget > 0 || t.ExtraOnTarget > 0 {
			rep.OK = false
		}
	}
	if opts.Out != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return rep, err
		}
		b = append(b, '\n')
		if err := writeFileSync(opts.Out, b); err != nil {
			return rep, fmt.Errorf("report write: %w", err)
		}
		written = true
	}
	return rep, nil
}

// newShadowAllowlist copies the template so every run counts into its
// own entries (the template stays pristine for the next run).
func newShadowAllowlist() []AllowlistEntry {
	out := make([]AllowlistEntry, len(shadowAllowlist))
	copy(out, shadowAllowlist)
	return out
}

func columnSetMatches(src, tgt []ColumnRef) (string, bool) {
	if len(src) != len(tgt) {
		return fmt.Sprintf("column count %d vs %d", len(src), len(tgt)), false
	}
	for i := range src {
		if src[i].Name != tgt[i].Name {
			return fmt.Sprintf("column %d: %s vs %s (order and set must match)", i, src[i].Name, tgt[i].Name), false
		}
		if src[i].Type != tgt[i].Type {
			return fmt.Sprintf("column %s: tag %s vs %s", src[i].Name, src[i].Type, tgt[i].Type), false
		}
	}
	return "", true
}

func columnNames(cols []ColumnRef) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// compareRows classifies every stable-key union member of one surface,
// in sorted key order so sample selection and artifacts are
// byte-stable. Structural deviations (missing/extra stable keys) are
// counted and sampled separately from field-level unexpected diffs —
// both are red.
func compareRows(sr *SurfaceResult, cols []ColumnRef, src, tgt map[string]string, maxSamples int, allow []AllowlistEntry) error {
	sample := func(d RowDiff) {
		if len(sr.Samples) < maxSamples {
			sr.Samples = append(sr.Samples, d)
		}
	}
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		srow := src[key]
		trow, ok := tgt[key]
		if !ok {
			sr.MissingOnTarget++
			sample(RowDiff{Key: displayKey(key), Kind: "missing_on_target"})
			continue
		}
		sr.Compared++
		sv, err := decodeRowObject([]byte(srow))
		if err != nil {
			return fmt.Errorf("table %s key %s: %w", sr.Table, displayKey(key), err)
		}
		tv, err := decodeRowObject([]byte(trow))
		if err != nil {
			return fmt.Errorf("table %s key %s: %w", sr.Table, displayKey(key), err)
		}
		var fields []FieldDiff
		absorbed := true
		for _, c := range cols {
			a, b := sv[c.Name], tv[c.Name]
			if string(a) == string(b) {
				continue
			}
			fd := FieldDiff{Column: c.Name, Source: valueDigest(a), Target: valueDigest(b)}
			rule, okRule := classifyColumnDiff(c.Type, a, b)
			if okRule {
				fd.Rule = rule
			} else {
				absorbed = false
			}
			fields = append(fields, fd)
		}
		if len(fields) == 0 {
			sr.Equal++
			continue
		}
		// count absorptions even in red rows: the per-rule totals must
		// agree with what the samples show (a red row may still carry
		// absorbed fields alongside the unexpected one)
		countAbsorptions(fields, allow)
		if absorbed {
			sr.Normalized++
		} else {
			sr.Unexpected++
		}
		sample(RowDiff{Key: displayKey(key), Kind: "field_diff", Fields: fields})
	}
	extras := make([]string, 0)
	for k := range tgt {
		if _, ok := src[k]; !ok {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	for _, key := range extras {
		sr.ExtraOnTarget++
		sample(RowDiff{Key: displayKey(key), Kind: "extra_on_target"})
	}
	return nil
}

func countAbsorptions(fields []FieldDiff, allow []AllowlistEntry) {
	for _, f := range fields {
		for i := range allow {
			if allow[i].ID == f.Rule {
				allow[i].Applied++
			}
		}
	}
}

// decodeRowObject parses one canonical row line into raw JSON values.
// A malformed line — or an empty/null object, which would leave every
// column nil and compare "equal" — is an ERROR, never an empty map
// (the false-green guard).
func decodeRowObject(line []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("decode canonical row line: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("canonical row line carries no columns")
	}
	return m, nil
}

// valueDigest renders a 12-hex digest of a canonical value — enough to
// distinguish equality classes without carrying row values into the
// report (no-leaks discipline).
func valueDigest(v json.RawMessage) string {
	sum := sha256.Sum256(v)
	return hex.EncodeToString(sum[:])[:12]
}

// redactKey trims very long keys to a digest (the no-leaks ceiling for
// pathological composite keys).
func redactKey(key string) string {
	if len(key) > 200 {
		return valueDigest(json.RawMessage(key)) + "…"
	}
	return key
}

// displayKey renders a canonical stable key for report samples and
// error messages: the JSON-encoded key tokens are decoded to their
// bare values (uuids, zotero keys, numbers — verbatim, UseNumber so
// int64 keys keep their token) and joined with " | ", so operators
// read identifiers instead of escaped JSON with \u001f separators.
// The map identity (canonicalKey output) stays untouched; this is the
// human-facing projection at the sample boundary, with redactKey's
// digest ceiling applied to pathological length.
func displayKey(key string) string {
	parts := strings.Split(key, "\x1f")
	out := make([]string, len(parts))
	for i, p := range parts {
		dec := json.NewDecoder(strings.NewReader(p))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			out[i] = p // not a JSON token — show raw (never expected)
			continue
		}
		out[i] = fmt.Sprint(v)
	}
	return redactKey(strings.Join(out, " | "))
}

// classifyColumnDiff decides whether a canonical-value DIFFERENCE is
// an approved normalization (rule id, true) or unexpected ("", false).
// Contract: called only with differing values — compareRows guards
// byte-equality before consulting the allowlist.
func classifyColumnDiff(tag string, a, b json.RawMessage) (string, bool) {
	switch tag {
	case TagNumeric:
		if ratEqual(tokenString(a), tokenString(b)) {
			return "numeric-value", true
		}
	case TagFloat64:
		if fa, ea := strconv.ParseFloat(tokenString(a), 64); ea == nil {
			if fb, eb := strconv.ParseFloat(tokenString(b), 64); eb == nil && fa == fb {
				return "float-value", true
			}
		}
	case TagTimestamp:
		var sa, sb string
		if json.Unmarshal(a, &sa) == nil && json.Unmarshal(b, &sb) == nil {
			ta, ea := parseCanonicalTimestamp(sa)
			tb, eb := parseCanonicalTimestamp(sb)
			if ea == nil && eb == nil && ta.Equal(tb) {
				return "timestamp-instant", true
			}
		}
	case TagJSONB:
		if jsonDeepEqualByValue(a, b) {
			return "json-number-value", true
		}
	}
	return "", false
}

// tokenString unwraps a raw JSON token (number or string) to its text.
func tokenString(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(bytes.TrimSpace(v))
}

func ratEqual(a, b string) bool {
	ra, okA := new(big.Rat).SetString(a)
	rb, okB := new(big.Rat).SetString(b)
	return okA && okB && ra.Cmp(rb) == 0
}

// jsonDeepEqualByValue compares two canonical JSON documents with
// number tokens by exact rational value — everything else (structure,
// strings, booleans, null) strictly.
func jsonDeepEqualByValue(a, b json.RawMessage) bool {
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	var va, vb any
	if da.Decode(&va) != nil || db.Decode(&vb) != nil || da.More() || db.More() {
		return false
	}
	return jsonValueEqual(va, vb)
}

func jsonValueEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		return ok && ratEqual(x.String(), y.String())
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case nil:
		return b == nil
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonValueEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			yv, present := y[k]
			if !present || !jsonValueEqual(v, yv) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
