// roundtrip_sqlite_test.go — the PG-free bundle leg (DM03 #312 +
// DM04 #313): a crafted library bundle (the same bytes a PostgreSQL
// export produces — the codec is shared) imports into library.sqlite,
// verifies (counts, digests, FK invariants, PRAGMA integrity), retries
// without duplication, keeps the in-flight import verbatim, and the
// imported file then passes the Library repository CONTRACT SUITE
// (F12 engine-matrix evidence). Runs with no PostgreSQL anywhere.
package databundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/library"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/reposuite"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/sqlite"
)

// libCols mirrors the Library schema columns (both engines share the
// order). The import validates this catalog against the REAL schema —
// a drifted literal fails the test loudly.
func libCols(table string) []ColumnManifest {
	switch table {
	case "library_imports":
		return []ColumnManifest{
			{"import_id", TagUUID, false},
			{"idempotency_key", TagText, false},
			{"payload_hash", TagText, false},
			{"record_type", TagText, false},
			{"request_json", TagJSONB, false},
			{"status", TagText, false},
			{"staging_sha256", TagText, false},
			{"staging_size", TagInt64, false},
			{"media_type", TagText, false},
			{"failure_code", TagText, true},
			{"failure_message", TagText, true},
			{"record_provider_id", TagText, true},
			{"rendition_provider_id", TagText, true},
			{"collection_provider_id", TagText, true},
			{"record_id", TagText, true},
			{"rendition_id", TagText, true},
			{"revision_id", TagInt64, true},
			{"created_at", TagTimestamp, false},
			{"updated_at", TagTimestamp, false},
		}
	case "library_import_events":
		return []ColumnManifest{
			{"import_id", TagUUID, false},
			{"seq", TagInt64, false},
			{"kind", TagText, false},
			{"detail", TagJSONB, false},
			{"at", TagTimestamp, false},
		}
	case "library_import_steps":
		return []ColumnManifest{
			{"import_id", TagUUID, false},
			{"step", TagText, false},
			{"state", TagText, false},
			{"provider_ref", TagText, true},
			{"detail", TagJSONB, false},
			{"attempts", TagInt64, false},
			{"updated_at", TagTimestamp, false},
		}
	case "library_external_identifiers":
		return []ColumnManifest{
			{"kind", TagText, false},
			{"normalized_value", TagText, false},
			{"record_id", TagText, false},
			{"created_at", TagTimestamp, false},
		}
	case "library_metadata_provenance":
		return []ColumnManifest{
			{"id", TagInt64, false},
			{"import_id", TagUUID, false},
			{"field", TagText, false},
			{"source", TagText, false},
			{"resolver_version", TagText, false},
			{"confidence", TagFloat64, false},
			{"applied", TagBool, false},
			{"value", TagText, false},
			{"at", TagTimestamp, false},
		}
	case "library_source_revisions":
		return []ColumnManifest{
			{"source_id", TagText, false},
			{"record_id", TagText, false},
			{"rendition_id", TagText, false},
			{"revision_id", TagInt64, false},
			{"content_hash", TagText, false},
			{"media_type", TagText, false},
			{"bibliography", TagJSONB, false},
			{"locator_capabilities", TagJSONB, false},
			{"content_ticket", TagText, false},
			{"origin", TagText, false},
			{"created_at", TagTimestamp, false},
		}
	case "library_write_audit":
		return []ColumnManifest{
			{"id", TagInt64, false},
			{"scope", TagText, false},
			{"operation", TagText, false},
			{"anchor", TagText, false},
			{"provider_ref", TagText, false},
			{"outcome", TagText, false},
			{"readback", TagJSONB, false},
			{"at", TagTimestamp, false},
		}
	case "library_provider_anchors":
		return []ColumnManifest{
			{"scope", TagText, false},
			{"kind", TagText, false},
			{"anchor", TagText, false},
			{"provider_id", TagText, false},
			{"provider_version", TagInt64, false},
			{"created_at", TagTimestamp, false},
		}
	case "library_writer_leases":
		return []ColumnManifest{
			{"scope", TagText, false},
			{"owner", TagText, false},
			{"acquired_at", TagTimestamp, false},
			{"heartbeat_at", TagTimestamp, false},
		}
	}
	panic("unknown table " + table)
}

// ts is a canonical-timestamp literal for fixture rows.
func ts(minute int) time.Time {
	return time.Date(2026, 10, 5, 10, minute, 30, 123456000, time.UTC)
}

// craftLibraryBundle writes a complete library-component bundle
// (library_* tables) with two imports: one committed, one IN FLIGHT
// (awaiting_confirmation — the DM04 fixture that must survive
// verbatim).
func craftLibraryBundle(t *testing.T, dir string) *Manifest {
	t.Helper()
	committed := "11111111-1111-4111-8111-111111111111"
	inflight := "22222222-2222-4222-8222-222222222222"

	rows := map[string][][]any{
		"library_imports": {
			{committed, "probe-idem-1", "payload-1", "book", []byte(`{"idempotency_key":"probe-idem-1","record_type":"book"}`), "committed",
				"deadbeef" + strings.Repeat("0", 56), int64(1024), "application/pdf",
				nil, nil, "prov-rec-1", "prov-rend-1", "prov-coll-1", "rec-1", "rend-1", int64(7), ts(0), ts(2)},
			{inflight, "probe-idem-2", "payload-2", "journalArticle", []byte(`{"idempotency_key":"probe-idem-2","record_type":"journalArticle","metadata_hints":{"doi":"10.1/x"}}`), "awaiting_confirmation",
				"cafecafe" + strings.Repeat("0", 56), int64(2048), "application/pdf",
				nil, nil, nil, nil, nil, nil, nil, nil, ts(1), ts(3)},
		},
		"library_import_events": {
			{committed, int64(1), "state_entered", []byte(`{"status":"received"}`), ts(0)},
			{committed, int64(2), "provider_write", []byte(`{"step":"creating_record"}`), ts(1)},
			{committed, int64(3), "terminal", []byte(`{"status":"committed"}`), ts(2)},
			{inflight, int64(1), "state_entered", []byte(`{"status":"received"}`), ts(1)},
			{inflight, int64(2), "decision_offered", []byte(`{"candidates":2}`), ts(3)},
		},
		"library_import_steps": {
			{committed, "creating_record", "done", "prov-rec-1", []byte(`{}`), int64(1), ts(1)},
			{inflight, "resolving_metadata", "in_progress", nil, []byte(`{"ladder":"crossref"}`), int64(2), ts(3)},
		},
		"library_external_identifiers": {
			{"doi", "10.1/x", "rec-1", ts(2)},
		},
		"library_metadata_provenance": {
			{int64(1000), committed, "title", "document", "", 1.0, true, "Title From PDF", ts(0)},
			{int64(1001), committed, "doi", "crossref", "fake-v1", 0.92, false, "", ts(1)},
			{int64(1002), inflight, "title", "crossref", "fake-v1", 0.5, false, "Candidate A", ts(3)},
		},
		"library_source_revisions": {
			{"src-1", "rec-1", "rend-1", int64(7), "hash-1", "application/pdf",
				[]byte(`{"title":"Title From PDF","creators":[{"family":"Author"}]}`),
				[]byte(`{"page_labels":true}`), "ticket-1", "import", ts(2)},
		},
		"library_write_audit": {
			{int64(500), "zotero|https://api.zotero.org|users/0", "ensure_record", "probe-idem-1", "prov-rec-1", "created", []byte(`{"ok":true}`), ts(1)},
		},
		"library_provider_anchors": {
			{"zotero|https://api.zotero.org|users/0", "record", "probe-idem-1", "prov-rec-1", int64(41), ts(1)},
			{"zotero|https://api.zotero.org|users/0", "rendition", "prov-rec-1|deadbeef", "prov-rend-1", int64(42), ts(1)},
		},
		"library_writer_leases": {
			{"zotero|https://api.zotero.org|users/0", "prodhost:4242:nonce", ts(0), ts(3)},
		},
	}

	man := &Manifest{
		Format: FormatName, FormatVersion: FormatVersion, Component: ComponentLib,
		CreatedAt: "2026-10-05T10:04:00.000000Z",
		Source: SourceManifest{
			Build: "axiom v0.2.2-test (commit test, release build)", Engine: "PostgreSQL 16.9",
			ExportCutoff: "2026-10-05T10:04:00.000000Z",
			Migrations: map[string][]string{"library_schema_migrations": {"0001_library.sql", "0002_library_writer_lease.sql", "0003_library_collection_anchor.sql"}},
		},
	}
	for _, spec := range LibraryTables {
		if !spec.SQLite {
			continue
		}
		tableRows := rows[spec.Name]
		cols := libCols(spec.Name)
		refs := make([]ColumnRef, len(cols))
		for i, c := range cols {
			refs[i] = ColumnRef{Name: c.Name, Type: c.Type}
		}
		tableDir := filepath.Join(dir, "tables", spec.Name)
		if err := os.MkdirAll(tableDir, 0o755); err != nil {
			t.Fatal(err)
		}
		var all []byte
		for _, r := range tableRows {
			var line bytes.Buffer
			if err := EncodeRow(&line, refs, r); err != nil {
				t.Fatalf("%s: %v", spec.Name, err)
			}
			all = append(all, line.Bytes()...)
		}
		if err := os.WriteFile(filepath.Join(tableDir, "0000.jsonl"), all, 0o644); err != nil {
			t.Fatal(err)
		}
		// rows_sha256 over key-ordered rows: craft already in order
		tm := TableManifest{
			Name: spec.Name, Columns: cols, Key: spec.Keys,
			Count:   int64(len(tableRows)),
			Batches: []BatchManifest{{File: "tables/" + spec.Name + "/0000.jsonl", Count: int64(len(tableRows)), SHA256: sha256Hex(all)}},
		}
		tm.RowsSHA256 = rowsDigest(t, dir, &tm)
		man.Tables = append(man.Tables, tm)
	}
	if err := writeManifest(dir, man); err != nil {
		t.Fatal(err)
	}
	return man
}

// TestBundleSQLiteRoundtrip — the full PG-free evidence chain.
func TestBundleSQLiteRoundtrip(t *testing.T) {
	dir := t.TempDir()
	craftLibraryBundle(t, dir)
	path := filepath.Join(t.TempDir(), "library.sqlite")
	ctx := context.Background()

	// 1. import — every row lands, nothing skipped
	res, err := Import(ctx, ImportOptions{From: dir, SQLitePath: path})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, ti := range res.Tables {
		if ti.Skipped {
			t.Fatalf("library_* table %s skipped on the SQLite target", ti.Table)
		}
		if ti.Idempotent != 0 {
			t.Fatalf("fresh import idempotent-skipped rows in %s", ti.Table)
		}
	}
	inflightAfter := func(t *testing.T) (status string, events int64) {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.QueryRow(`SELECT status FROM library_imports WHERE idempotency_key='probe-idem-2'`).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM library_import_events WHERE import_id='22222222-2222-4222-8222-222222222222'`).Scan(&events); err != nil {
			t.Fatal(err)
		}
		return
	}

	// 2. verify — counts, digests, FK invariants, PRAGMA integrity
	vres, err := Verify(ctx, VerifyOptions{From: dir, SQLitePath: path})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vres.OK {
		for _, tv := range vres.Tables {
			if !tv.CountOK || !tv.DigestOK {
				t.Errorf("table %s: countOK=%v digestOK=%v (got %d want %d)", tv.Table, tv.CountOK, tv.DigestOK, tv.CountGot, tv.CountWant)
			}
		}
		t.Fatal("verify not OK")
	}
	if len(vres.Pragmas) != 2 || !strings.Contains(vres.Pragmas[0], "integrity_check") {
		t.Fatalf("pragma verdicts missing: %v", vres.Pragmas)
	}

	// 3. in-flight import survives VERBATIM — neither completed nor dropped
	status, events := inflightAfter(t)
	if status != "awaiting_confirmation" || events != 2 {
		t.Fatalf("in-flight import mutated: status=%q events=%d", status, events)
	}

	// 4. retry sonde — a re-import (merge mode, occupied target) never
	// duplicates: every row is an idempotent skip
	res2, err := Import(ctx, ImportOptions{From: dir, SQLitePath: path, Merge: true})
	if err != nil {
		t.Fatalf("retry import: %v", err)
	}
	for _, ti := range res2.Tables {
		if ti.Inserted != 0 || ti.Idempotent == 0 {
			t.Fatalf("retry duplicated rows in %s: %+v", ti.Table, ti)
		}
	}
	if status, events = inflightAfter(t); status != "awaiting_confirmation" || events != 2 {
		t.Fatalf("retry mutated in-flight import: %q %d", status, events)
	}

	// 5. non-empty guard without merge
	if _, err := Import(ctx, ImportOptions{From: dir, SQLitePath: path}); err == nil ||
		!strings.Contains(err.Error(), "explicit merge mode") {
		t.Fatalf("occupied target accepted without merge: %v", err)
	}

	// 6. digests STILL equal after the retry (nothing changed)
	vres2, err := Verify(ctx, VerifyOptions{From: dir, SQLitePath: path})
	if err != nil || !vres2.OK {
		t.Fatalf("post-retry verify: %v %v", vres2, err)
	}

	// 7. the AUTOINCREMENT continuation: next provenance insert must not
	// collide with the imported explicit ids (1000..1002)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO library_metadata_provenance
		(import_id, field, source, resolver_version, confidence, applied, value, at)
		VALUES ('11111111-1111-4111-8111-111111111111','x','user','',1,1,'','2026-10-05T10:05:00.000000Z')`); err != nil {
		t.Fatalf("post-import insert collides (sequence not advanced): %v", err)
	}
	var newID int64
	if err := db.QueryRow(`SELECT id FROM library_metadata_provenance WHERE field='x'`).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if newID <= 1002 {
		t.Fatalf("AUTOINCREMENT did not continue past imported ids: %d", newID)
	}
	db.Close()
}

// TestImportedSQLiteFilePassesContractSuite — the imported file is a
// real Library database: the F12 repository contract suite runs green
// over it (the "same contract suite green" acceptance of DM04).
func TestImportedSQLiteFilePassesContractSuite(t *testing.T) {
	dir := t.TempDir()
	craftLibraryBundle(t, dir)
	path := filepath.Join(t.TempDir(), "library.sqlite")
	if _, err := Import(context.Background(), ImportOptions{From: dir, SQLitePath: path}); err != nil {
		t.Fatalf("import: %v", err)
	}
	reposuite.Run(t, "sqlite-imported", func(t *testing.T) (library.Repository, func(), bool) {
		r, err := sqlite.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open imported file: %v", err)
		}
		return r, func() { _ = r.Close() }, true
	})
}

// TestUnknownEnumAbortsLoudOnImport — a bundle row carrying a status
// outside the declared vocabulary aborts the import visibly (DM03
// failure contract), even when manifest digests are made consistent
// (the enum vocabulary is validated independently of the hash chain).
func TestUnknownEnumAbortsLoudOnImport(t *testing.T) {
	dir := t.TempDir()
	craftLibraryBundle(t, dir)

	// Rewrite the library_imports batch with a bogus status, then mend
	// the manifest + sidecar so ONLY the enum teeth can catch it.
	batch := filepath.Join(dir, "tables", "library_imports", "0000.jsonl")
	raw, err := os.ReadFile(batch)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"status":"awaiting_confirmation"`, `"status":"bogus_terminal_state"`, 1))
	if err := os.WriteFile(batch, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	tm := man.Table("library_imports")
	tm.Batches[0].SHA256 = sha256Hex(raw)
	tm.RowsSHA256 = "recomputed-below"
	if err := writeManifest(dir, man); err != nil { // fresh sidecar pins the mend
		t.Fatal(err)
	}

	// The vocabulary teeth: declare the real F03 status vocabulary in
	// the manifest (as a source export would) and the tampered value
	// violates it at decode time.
	man2, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	tm2 := man2.Table("library_imports")
	tm2.Enums = map[string][]string{"status": {"received", "inspecting", "resolving_metadata", "awaiting_confirmation",
		"ensuring_collections", "creating_record", "uploading_rendition", "verifying", "committed",
		"retryable_failed", "terminal_failed"}}
	// recompute rows digest through the import's own codec over the mend
	tm2.RowsSHA256 = tm.Batches[0].SHA256 // not compared pre-import
	if err := writeManifest(dir, man2); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "library.sqlite")
	_, err = Import(context.Background(), ImportOptions{From: dir, SQLitePath: path})
	if err == nil || !strings.Contains(err.Error(), "bogus_terminal_state") {
		t.Fatalf("unknown enum accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "vocabulary") {
		t.Fatalf("enum error does not name the vocabulary: %v", err)
	}
}

// sha256Hex keeps the crafting terse.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

func rowsDigest(t *testing.T, dir string, tm *TableManifest) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(tm.Batches[0].File)))
	if err != nil {
		t.Fatal(err)
	}
	return sha256Hex(raw)
}
