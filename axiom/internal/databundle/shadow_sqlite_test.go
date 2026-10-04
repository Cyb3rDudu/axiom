// shadow_sqlite_test.go — the DM08 (#317) SQLite leg: the shadow over
// an imported library.sqlite target. The legacy mirror namespace has
// no SQLite home (F12) — those surfaces report skipped with the scope
// note (counted, documented — not a deviation); the library_*
// namespace compares fully. The sonde mutates the imported file and
// must go red. PG-gated like the roundtrip legs (the legacy side of
// the pair is a PostgreSQL mirror).
package databundle

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func TestShadowSQLite(t *testing.T) {
	if os.Getenv("AXIOM_TEST_DATABASE_URL") == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set — PG-gated shadow tests skip")
	}
	src, cleanupSrc := scratchDB(t, "shlsrc")
	t.Cleanup(cleanupSrc)
	seedSource(t, src)

	ctx := context.Background()
	bundle := t.TempDir()
	if _, err := Export(ctx, "library", ExportOptions{DSN: src, Out: bundle, Build: "axiom v0.2.2-it (commit test, release build)"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "library.sqlite")
	if _, err := Import(ctx, ImportOptions{From: bundle, SQLitePath: dbPath}); err != nil {
		t.Fatalf("import: %v", err)
	}

	// green: skips on the legacy namespace, full compare on library_*
	rep, err := Shadow(ctx, ShadowOptions{SourceDSN: src, SQLitePath: dbPath})
	if err != nil {
		t.Fatalf("shadow: %v", err)
	}
	if !rep.OK {
		summary, _ := jsonMarshalIndent(rep.Tables)
		t.Fatalf("sqlite shadow not OK:\n%s", summary)
	}
	compared, skipped := 0, 0
	for _, s := range rep.Tables {
		switch s.Status {
		case "skipped":
			skipped++
			if s.Note == "" {
				t.Fatalf("table %s skipped without a scope note", s.Table)
			}
			if sqliteCarried(s.Table) {
				t.Fatalf("table %s is carried on SQLite but reported skipped", s.Table)
			}
		case "compared":
			compared++
			if !sqliteCarried(s.Table) {
				t.Fatalf("table %s compared although outside the SQLite namespace", s.Table)
			}
			if s.Unexpected != 0 || s.Normalized != 0 || s.MissingOnTarget != 0 || s.ExtraOnTarget != 0 {
				t.Fatalf("table %s: deviations on a clean import: %+v", s.Table, s)
			}
			if s.Equal != s.Compared {
				t.Fatalf("table %s: equal %d != compared %d", s.Table, s.Equal, s.Compared)
			}
		default:
			t.Fatalf("table %s: unexpected status %q", s.Table, s.Status)
		}
	}
	if compared == 0 || skipped == 0 {
		t.Fatalf("compared %d, skipped %d — namespace split not exercised", compared, skipped)
	}

	// sonde: a semantic deviation inside the imported file goes red
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE library_imports SET record_type = 'drifted' WHERE import_id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'`); err != nil {
		t.Fatal(err)
	}
	rep, err = Shadow(ctx, ShadowOptions{SourceDSN: src, SQLitePath: dbPath})
	if err != nil {
		t.Fatalf("shadow after sonde: %v", err)
	}
	if rep.OK {
		t.Fatal("injected deviation in the imported file did not fail the sqlite shadow")
	}
	li := findSurface(t, rep, "library_imports")
	if li.Unexpected != 1 || len(li.Samples) == 0 {
		t.Fatalf("library_imports: unexpected %d samples %d", li.Unexpected, len(li.Samples))
	}
	sawType := false
	for _, f := range li.Samples[0].Fields {
		if f.Column == "record_type" && f.Rule == "" {
			sawType = true
		}
	}
	if !sawType {
		t.Fatalf("sample lacks the injected record_type diff: %+v", li.Samples[0])
	}
}

// TestShadowSQLiteReadOnlyOpen — the shadow NEVER creates or migrates
// its target: a missing path fails loudly without minting a file, and
// a target file on an older/absent schema surfaces as structural red
// instead of being silently brought to head.
func TestShadowSQLiteReadOnlyOpen(t *testing.T) {
	if os.Getenv("AXIOM_TEST_DATABASE_URL") == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set — PG-gated shadow tests skip")
	}
	src, cleanupSrc := scratchDB(t, "shlro")
	t.Cleanup(cleanupSrc)
	seedSource(t, src)
	ctx := context.Background()

	// missing path: loud error, no file created
	missing := filepath.Join(t.TempDir(), "does-not-exist.sqlite")
	if _, err := Shadow(ctx, ShadowOptions{SourceDSN: src, SQLitePath: missing}); err == nil {
		t.Fatal("missing target file passed silently — the shadow must not create its evidence source")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing-file error does not name the problem: %v", err)
	}
	if _, serr := os.Stat(missing); !os.IsNotExist(serr) {
		t.Fatal("the failed shadow run left a file behind")
	}

	// schema-stand drift: an EMPTY file (older than any library schema)
	// must surface as structural red on every carried surface — never
	// be migrated to head by the act of comparing.
	empty := filepath.Join(t.TempDir(), "empty.sqlite")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Shadow(ctx, ShadowOptions{SourceDSN: src, SQLitePath: empty})
	if err != nil {
		t.Fatalf("shadow over the empty file errored: %v", err)
	}
	if rep.OK {
		t.Fatal("empty (pre-schema) target passed — the schema drift was invisible")
	}
	red := 0
	for _, s := range rep.Tables {
		if s.Status == "skipped" {
			continue
		}
		if s.Unexpected != 1 || len(s.Samples) == 0 || s.Samples[0].Kind != "structural" {
			t.Fatalf("carried surface %+v is not structural red on the pre-schema target", s)
		}
		red++
	}
	if red != 9 { // the library_* namespace (F12)
		t.Fatalf("structural reds: %d, want 9", red)
	}
	// and the file is STILL empty of schema — no migration ran
	db, err := sql.Open("sqlite", "file:"+empty+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var objects int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if objects != 0 {
		t.Fatalf("the read-only shadow minted %d schema objects into its target", objects)
	}
}

// TestShadowSQLiteSourceGateBeforeScope — a legacy table missing at
// the pull-point source is RED even on the SQLite leg, where that
// surface would otherwise be skipped by the F12 namespace rule: the
// full-data-set gate is target-engine-independent.
func TestShadowSQLiteSourceGateBeforeScope(t *testing.T) {
	if os.Getenv("AXIOM_TEST_DATABASE_URL") == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set — PG-gated shadow tests skip")
	}
	src, cleanupSrc := scratchDB(t, "shlsg")
	t.Cleanup(cleanupSrc)
	seedSource(t, src)
	ctx := context.Background()
	bundle := t.TempDir()
	if _, err := Export(ctx, "library", ExportOptions{DSN: src, Out: bundle, Build: "axiom v0.2.2-it (commit test, release build)"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "library.sqlite")
	if _, err := Import(ctx, ImportOptions{From: bundle, SQLitePath: dbPath}); err != nil {
		t.Fatalf("import: %v", err)
	}

	pool := mustPool(t, src)
	defer pool.Close()
	// zotero_collection_selections: legacy namespace (would be F12-
	// skipped on this leg), no rows, no dependants — the clean probe.
	if _, err := pool.Exec(ctx, `DROP TABLE zotero_collection_selections`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	rep, err := Shadow(ctx, ShadowOptions{SourceDSN: src, SQLitePath: dbPath})
	if err != nil {
		t.Fatalf("shadow: %v", err)
	}
	if rep.OK {
		t.Fatal("table absent at the source passed green on the SQLite leg — the pull-point gate must run before the F12 scope rule")
	}
	cs := findSurface(t, rep, "zotero_collection_selections")
	if cs.Status != "compared" || cs.Unexpected != 1 {
		t.Fatalf("selections surface: %+v — expected structural red, not a scope skip", cs)
	}
}
