// shadow_pg_it_test.go — the DM08 (#317) PostgreSQL evidence chain,
// gated on AXIOM_TEST_DATABASE_URL (scratch-DB convention, same as the
// bundle roundtrip legs). The chain mirrors the release-validation
// drill: pull-point mirror (source) → bundle export → import into a
// schema-only target → shadow run. Witnesses:
//
//   - GREEN: full data set, every surface compared, zero unexpected,
//     report artifact written and parseable, source cutoff recorded.
//   - SONDE (red, the teeth): a deliberately injected SEMANTIC
//     deviation in the imported copy (a title changed, a raw envelope
//     value changed) surfaces as unexpected; the run goes red.
//   - LEXICAL sonde (green, the bounds): a difference that is pure
//     numeric spelling (0.850 vs 0.85) is absorbed by the documented
//     numeric-value rule and reported as normalized — the allowlist
//     demonstrably does not swallow semantic differences, only the
//     listed lexical classes.
package databundle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// shadowPGFixture builds the drill pair: a seeded source mirror and a
// bundle-fed schema-only target (the DM04 rehearsal shape).
func shadowPGFixture(t *testing.T) (src, tgt string) {
	t.Helper()
	src, cleanupSrc := scratchDB(t, "shsrc")
	t.Cleanup(cleanupSrc)
	seedSource(t, src)

	tgt, cleanupTgt := scratchDB(t, "shtgt")
	t.Cleanup(cleanupTgt)

	ctx := context.Background()
	bundle := t.TempDir()
	if _, err := Export(ctx, "library", ExportOptions{DSN: src, Out: bundle, Build: "axiom v0.2.2-it (commit test, release build)"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := Import(ctx, ImportOptions{From: bundle, DSN: tgt}); err != nil {
		t.Fatalf("import: %v", err)
	}
	return src, tgt
}

func execTarget(t *testing.T, dsn, q string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func findSurface(t *testing.T, rep *ShadowReport, table string) SurfaceResult {
	t.Helper()
	for _, s := range rep.Tables {
		if s.Table == table {
			return s
		}
	}
	t.Fatalf("report lacks table %s", table)
	return SurfaceResult{}
}

func TestShadowPostgresGreen(t *testing.T) {
	src, tgt := shadowPGFixture(t)
	ctx := context.Background()

	report := filepath.Join(t.TempDir(), "shadow-report.json")
	rep, err := Shadow(ctx, ShadowOptions{SourceDSN: src, DSN: tgt, Out: report})
	if err != nil {
		t.Fatalf("shadow: %v", err)
	}
	if !rep.OK {
		summary, _ := json.MarshalIndent(rep.Tables, "", "  ")
		t.Fatalf("shadow not OK:\n%s", summary)
	}
	if rep.SourceCutoff == "" || rep.TargetEngine != "postgresql" {
		t.Fatalf("report provenance incomplete: %+v", rep)
	}

	// every library table compared, full data set — not a sample
	compared := 0
	for _, s := range rep.Tables {
		if s.Status != "compared" {
			t.Fatalf("table %s: status %q (%s) — the source carries it", s.Table, s.Status, s.Note)
		}
		if s.Compared != s.SourceRows || s.Compared != s.TargetRows {
			t.Fatalf("table %s: compared %d, source %d, target %d — union mismatch",
				s.Table, s.Compared, s.SourceRows, s.TargetRows)
		}
		if s.Unexpected != 0 || s.Normalized != 0 || s.MissingOnTarget != 0 || s.ExtraOnTarget != 0 {
			t.Fatalf("table %s: unexpected %d normalized %d missing %d extra %d on a clean import",
				s.Table, s.Unexpected, s.Normalized, s.MissingOnTarget, s.ExtraOnTarget)
		}
		if s.Equal != s.Compared {
			t.Fatalf("table %s: equal %d != compared %d", s.Table, s.Equal, s.Compared)
		}
		compared++
	}
	if compared != len(LibraryTables) {
		t.Fatalf("compared %d tables, want %d", compared, len(LibraryTables))
	}

	// the artifact parses and agrees with the verdict
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("report artifact: %v", err)
	}
	var fromDisk ShadowReport
	if err := json.Unmarshal(b, &fromDisk); err != nil {
		t.Fatalf("report artifact not JSON: %v", err)
	}
	if !fromDisk.OK || fromDisk.SourceCutoff != rep.SourceCutoff {
		t.Fatal("report artifact disagrees with the run")
	}
}

// TestShadowPostgresSondeRed — the witness with teeth: a semantic
// deviation injected into the imported copy must surface as unexpected
// (red). Two probes: a scalar record field (title) and a value inside a
// raw envelope.
func TestShadowPostgresSondeRed(t *testing.T) {
	src, tgt := shadowPGFixture(t)
	ctx := context.Background()

	execTarget(t, tgt, `UPDATE zotero_documents SET title = title || ' INJECTED DRIFT' WHERE zotero_key = 'DOC1'`)
	execTarget(t, tgt, `UPDATE zotero_items SET raw_data = jsonb_set(raw_data, '{title}', to_jsonb('Injected Drift'::text)) WHERE zotero_key = 'ITEM1'`)

	rep, err := Shadow(ctx, ShadowOptions{SourceDSN: src, DSN: tgt})
	if err != nil {
		t.Fatalf("shadow: %v", err)
	}
	if rep.OK {
		t.Fatal("injected semantic deviations did not fail the run — the comparison has no teeth")
	}
	docs := findSurface(t, rep, "zotero_documents")
	if docs.Unexpected != 1 || len(docs.Samples) == 0 {
		t.Fatalf("records surface: unexpected %d, samples %d", docs.Unexpected, len(docs.Samples))
	}
	sawTitle := false
	for _, f := range docs.Samples[0].Fields {
		if f.Column == "title" && f.Rule == "" {
			sawTitle = true
		}
	}
	if !sawTitle {
		t.Fatalf("records sample lacks the injected title diff: %+v", docs.Samples[0])
	}
	items := findSurface(t, rep, "zotero_items")
	if items.Unexpected != 1 {
		t.Fatalf("raw-envelopes surface: unexpected %d, want 1", items.Unexpected)
	}

	// value digests differ across sides; no rule absorbed either probe
	for _, a := range rep.Allowlist {
		if a.Applied != 0 {
			t.Fatalf("allowlist rule %q absorbed a semantic deviation", a.ID)
		}
	}
}

// TestShadowPostgresLexicalAbsorbed — the allowlist bounds: a pure
// numeric-spelling difference on the LEGACY side (0.850 vs the target's
// 0.85) is absorbed by the numeric-value rule, counted as normalized,
// visible in the report — and the run stays green. A real value change
// on the same column goes red (the rule does not swallow semantics).
func TestShadowPostgresLexicalAbsorbed(t *testing.T) {
	src, tgt := shadowPGFixture(t)
	ctx := context.Background()

	// legacy side renders the same rational with more scale
	srcPool := mustPool(t, src)
	defer srcPool.Close()
	if _, err := srcPool.Exec(ctx,
		`UPDATE repair_cases SET verify_score = 0.850 WHERE id = '77777777-7777-4777-8777-777777777777'`); err != nil {
		t.Fatal(err)
	}

	rep, err := Shadow(ctx, ShadowOptions{SourceDSN: src, DSN: tgt})
	if err != nil {
		t.Fatalf("shadow: %v", err)
	}
	if !rep.OK {
		t.Fatal("lexical-only numeric difference was not absorbed")
	}
	rc := findSurface(t, rep, "repair_cases")
	if rc.Normalized != 1 || rc.Unexpected != 0 {
		t.Fatalf("repair-readback: normalized %d unexpected %d, want 1/0", rc.Normalized, rc.Unexpected)
	}
	found := false
	for _, a := range rep.Allowlist {
		if a.ID == "numeric-value" && a.Applied == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("numeric-value rule did not count its absorption")
	}

	// the same column, a REAL value change: red
	if _, err := srcPool.Exec(ctx,
		`UPDATE repair_cases SET verify_score = 0.9 WHERE id = '77777777-7777-4777-8777-777777777777'`); err != nil {
		t.Fatal(err)
	}
	rep, err = Shadow(ctx, ShadowOptions{SourceDSN: src, DSN: tgt})
	if err != nil {
		t.Fatalf("shadow: %v", err)
	}
	if rep.OK {
		t.Fatal("semantic numeric change was absorbed — the rule overreached")
	}
	rc = findSurface(t, rep, "repair_cases")
	if rc.Unexpected != 1 {
		t.Fatalf("repair-readback: unexpected %d, want 1", rc.Unexpected)
	}
}

// TestShadowPostgresStructuralRed — rows only on one side are structural
// deviations (red): a missing stable key on the target and an extra row
// the legacy side never had.
func TestShadowPostgresStructuralRed(t *testing.T) {
	src, tgt := shadowPGFixture(t)
	ctx := context.Background()

	execTarget(t, tgt, `DELETE FROM zotero_selections WHERE document_id = '55555555-5555-4555-8555-555555555555'`)
	execTarget(t, tgt, `INSERT INTO zotero_collection_selections (collection_key, mode, updated_at)
	                   VALUES ('GHOST', 'included', '2026-10-05 12:00:00.000001+00')`)

	rep, err := Shadow(ctx, ShadowOptions{SourceDSN: src, DSN: tgt})
	if err != nil {
		t.Fatalf("shadow: %v", err)
	}
	if rep.OK {
		t.Fatal("structural deviations did not fail the run")
	}
	sel := findSurface(t, rep, "zotero_selections")
	if sel.MissingOnTarget != 1 || sel.Compared != 0 {
		t.Fatalf("selections: missing %d compared %d, want 1/0", sel.MissingOnTarget, sel.Compared)
	}
	cs := findSurface(t, rep, "zotero_collection_selections")
	if cs.ExtraOnTarget != 1 {
		t.Fatalf("collection selections: extra %d, want 1", cs.ExtraOnTarget)
	}
}

// TestShadowPostgresAbsentAtSourceRed — the shadow is a gate over the
// FULL library data set: a table missing from the pull-point source
// (e.g. a partial restore) is a red structural deviation, not a
// silent skip.
func TestShadowPostgresAbsentAtSourceRed(t *testing.T) {
	src, tgt := shadowPGFixture(t)
	ctx := context.Background()

	pool := mustPool(t, src)
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP TABLE zotero_collection_selections`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	rep, err := Shadow(ctx, ShadowOptions{SourceDSN: src, DSN: tgt})
	if err != nil {
		t.Fatalf("shadow: %v", err)
	}
	if rep.OK {
		t.Fatal("table absent at the source passed as green — the full-data-set gate is missing")
	}
	cs := findSurface(t, rep, "zotero_collection_selections")
	if cs.Unexpected != 1 || len(cs.Samples) == 0 || cs.Samples[0].Kind != "structural" {
		t.Fatalf("surface: %+v", cs)
	}
}

func mustPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
