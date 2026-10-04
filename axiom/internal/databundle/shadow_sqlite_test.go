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
