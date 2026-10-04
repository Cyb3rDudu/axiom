// roundtrip_pg_it_test.go — the PostgreSQL evidence chain (DM03 #312 +
// DM04 #313), gated on AXIOM_TEST_DATABASE_URL (scratch-DB convention,
// same as the pglib suite): PG→bundle→PG identity, PG→bundle→SQLite
// type portability, retry never duplicates, digest-mismatch isolation
// (bit-flip), unknown-enum teeth against the target's own vocabulary,
// in-flight import survives verbatim, and the import runs under the
// DM07 DML-only runtime role (axiom_library, incl. sequence resync).
package databundle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/pglib"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scratchDB creates a fresh scratch database (name derived from the
// test), migrated core+library, and returns its DSN + a cleanup func.
func scratchDB(t *testing.T, tag string) (string, func()) {
	t.Helper()
	admin := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set — PG-gated bundle tests skip")
	}
	base := dsnDatabase(admin)
	if !strings.HasSuffix(base, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", base)
	}
	name := fmt.Sprintf("%s_bundle_%s_%d_test", strings.TrimSuffix(base, "_test"), tag, os.Getpid())
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			t.Fatalf("scratch db name %q is not a safe identifier", name)
		}
	}
	ctx := context.Background()
	ap, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	if _, err := ap.Exec(ctx, fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, name)); err != nil {
		t.Fatalf("kill connections: %v", err)
	}
	if _, err := ap.Exec(ctx, `DROP DATABASE IF EXISTS `+name); err != nil {
		t.Fatalf("drop old scratch: %v", err)
	}
	if _, err := ap.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	dsn := dsnWithDatabase(admin, name)
	cleanup := func() {
		if _, err := ap.Exec(ctx, fmt.Sprintf(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, name)); err == nil {
			_, _ = ap.Exec(ctx, `DROP DATABASE IF EXISTS `+name)
		}
		ap.Close()
	}
	core, err := db.Open(ctx, dsn)
	if err != nil {
		cleanup()
		t.Fatalf("open scratch: %v", err)
	}
	if err := core.Migrate(ctx); err != nil {
		core.Close()
		cleanup()
		t.Fatalf("core migrate: %v", err)
	}
	if err := pglib.Migrate(ctx, core.Pool()); err != nil {
		core.Close()
		cleanup()
		t.Fatalf("library migrate: %v", err)
	}
	core.Close()
	return dsn, cleanup
}

func dsnDatabase(dsn string) string {
	name := dsn
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '?'); i >= 0 {
		name = name[:i]
	}
	return name
}

func dsnWithDatabase(dsn, newDB string) string {
	i := strings.LastIndexByte(dsn, '/')
	base := dsn[:i+1]
	rest := dsn[i+1:]
	if j := strings.IndexAny(rest, "?;"); j >= 0 {
		return base + newDB + rest[j:]
	}
	return base + newDB
}

// seedSource writes a full-surface fixture set: every library table
// with NULLs, µs timestamps, permuted-key jsonb, enum values, an
// IN-FLIGHT import (awaiting_confirmation), and explicit bigserial ids.
func seedSource(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	seed := `
INSERT INTO zotero_sources (id, base_url, library_id, server_id, schema_version, last_modified_version, canonical_last_modified_version, last_sync_at, created_at, updated_at)
VALUES ('0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e', 'https://api.zotero.org', 'users/0', 'srv-1', 5, 181, 44,
        '2026-10-05 09:00:00.000001+00', '2026-10-01 08:00:00.000002+00', '2026-10-05 09:00:00.000003+00');

INSERT INTO zotero_items (id, source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data, deleted, synced_at, created_at, updated_at)
VALUES ('11111111-1111-4111-8111-111111111111', '0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e', 'ITEM1', 9, 'journalArticle', NULL,
        '{"zotero-key":"ITEM1","data":{"title":"Permuted Keys Probe","creators":[{"creatorType":"author","name":"A"}]}}',
        '{"title":"Permuted Keys Probe","creators":[{"creatorType":"author","name":"A"}]}',
        false, '2026-10-05 09:00:00.000004+00', '2026-10-01 08:00:00.000005+00', '2026-10-05 09:00:00.000006+00'),
       ('33333333-3333-4333-8333-333333333333', '0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e', 'ATTACH1', 10, 'attachment', 'ITEM1',
        '{"data":{"contentType":"application/pdf","filename":"probe.pdf"}}', '{"contentType":"application/pdf"}',
        false, '2026-10-05 09:00:00.000007+00', '2026-10-01 08:00:00.000008+00', '2026-10-05 09:00:00.000009+00');

INSERT INTO zotero_collections (id, source_id, zotero_key, name, parent_key, raw_envelope, deleted, synced_at, created_at, updated_at)
VALUES ('44444444-4444-4444-8444-444444444444', '0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e', 'COLL1', 'Probe Folder', NULL,
        '{"data":{"key":"COLL1","name":"Probe Folder"}}', false,
        '2026-10-05 09:00:00.000010+00', '2026-10-01 08:00:00.000011+00', '2026-10-05 09:00:00.000012+00');

INSERT INTO zotero_item_collections (item_id, collection_id)
VALUES ('11111111-1111-4111-8111-111111111111', '44444444-4444-4444-8444-444444444444');

INSERT INTO zotero_documents (id, source_id, zotero_key, zotero_version, item_type, title, creators, abstract_note, publication_year, publication_date, publisher, isbn, doi, url, language, metadata, tags, collections, deleted, canonical_item_id, created_at, updated_at)
VALUES ('55555555-5555-4555-8555-555555555555', '0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e', 'DOC1', 3, 'journalArticle', 'Permuted Keys Probe',
        '[{"creatorType":"author","name":"A"}]', NULL, 2026, '2026-03-01', NULL, NULL, '10.1/x', NULL, 'en',
        '{}', '[]', '["COLL1"]', false, '11111111-1111-4111-8111-111111111111',
        '2026-10-01 08:00:00.000013+00', '2026-10-05 09:00:00.000014+00');

INSERT INTO zotero_attachments (id, source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode, content_type, filename, file_uri, local_path, content_hash, file_size, mtime_ms, preferred, deleted, canonical_item_id, repair_attempts, created_at, updated_at)
VALUES ('66666666-6666-4666-8666-666666666666', '0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e', '55555555-5555-4555-8555-555555555555', 'ATTACH1', 10, 'DOC1',
        'imported_file', 'application/pdf', 'probe.pdf', NULL, NULL, 'deadbeef01', 123456, 1759000000000, true, false,
        '33333333-3333-4333-8333-333333333333', 1, '2026-10-01 08:00:00.000015+00', '2026-10-05 09:00:00.000016+00');

INSERT INTO zotero_selections (document_id, mode, updated_at)
VALUES ('55555555-5555-4555-8555-555555555555', 'included', '2026-10-05 09:00:00.000017+00');

INSERT INTO zotero_collection_selections (collection_key, mode, updated_at)
VALUES ('COLL1', 'excluded', '2026-10-05 09:00:00.000018+00');

INSERT INTO repair_cases (id, attachment_id, document_id, status, attempts, suspicion_class, analysis, plan, plan_version, verify_score, verify_contradictions, verdict, blocked_reason, created_at, updated_at)
VALUES ('77777777-7777-4777-8777-777777777777', '66666666-6666-4666-8666-666666666666', '55555555-5555-4555-8555-555555555555',
        'healed', 1, '🔴 reparierbar', '{"folio_runs":[[1,12]]}', '{"labels":[]}', 2, 0.85, 0, 'auto_apply', '',
        '2026-10-02 08:00:00.000019+00', '2026-10-02 09:00:00.000020+00'),
       ('88888888-8888-4888-8888-888888888888', '66666666-6666-4666-8666-666666666666', NULL,
        'blocked_for_dudu', 2, '🔴 8-Sprung', '{"folio_runs":[[1,4],[9,12]]}', '{"labels":[]}', 1, 0.10, 3, 'blocked', 'blocked_for_dudu',
        '2026-10-03 08:00:00.000021+00', '2026-10-03 09:00:00.000022+00');

INSERT INTO zotero_write_audit (id, case_id, attachment_id, action, detail, created_at)
VALUES ('99999999-9999-4999-8999-999999999999', '77777777-7777-4777-8777-777777777777', '66666666-6666-4666-8666-666666666666',
        'quarantine', '{"path":"/quarantine/probe.pdf"}', '2026-10-02 09:00:00.000023+00');

INSERT INTO library_imports (import_id, idempotency_key, payload_hash, record_type, request_json, status, staging_sha256, staging_size, media_type, failure_code, failure_message, record_provider_id, rendition_provider_id, collection_provider_id, record_id, rendition_id, revision_id, created_at, updated_at)
VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'it-idem-1', 'it-payload-1', 'book',
        '{"idempotency_key":"it-idem-1","record_type":"book"}', 'committed',
        'feedface' || repeat('0', 56), 4096, 'application/pdf',
        NULL, NULL, 'prov-rec-1', 'prov-rend-1', 'prov-coll-1', 'rec-1', 'rend-1', 7,
        '2026-10-05 10:00:00.000024+00', '2026-10-05 10:00:02.000025+00'),
       ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'it-idem-2', 'it-payload-2', 'journalArticle',
        '{"idempotency_key":"it-idem-2","metadata_hints":{"doi":"10.1/x"}}', 'awaiting_confirmation',
        'cafecafe' || repeat('0', 56), 8192, 'application/pdf',
        NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
        '2026-10-05 10:00:01.000026+00', '2026-10-05 10:00:03.000027+00');

INSERT INTO library_import_events (import_id, seq, kind, detail, at)
VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 1, 'state_entered', '{"status":"received"}', '2026-10-05 10:00:00.000028+00'),
       ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 2, 'terminal', '{"status":"committed"}', '2026-10-05 10:00:02.000029+00'),
       ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 1, 'state_entered', '{"status":"received"}', '2026-10-05 10:00:01.000030+00'),
       ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 2, 'decision_offered', '{"candidates":2}', '2026-10-05 10:00:03.000031+00');

INSERT INTO library_import_steps (import_id, step, state, provider_ref, detail, attempts, updated_at)
VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'creating_record', 'done', 'prov-rec-1', '{}', 1, '2026-10-05 10:00:01.000032+00'),
       ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'resolving_metadata', 'in_progress', NULL, '{"ladder":"crossref"}', 2, '2026-10-05 10:00:03.000033+00');

INSERT INTO library_external_identifiers (kind, normalized_value, record_id, created_at)
VALUES ('doi', '10.1/x', 'rec-1', '2026-10-05 10:00:02.000034+00');

INSERT INTO library_metadata_provenance (id, import_id, field, source, resolver_version, confidence, applied, value, at)
VALUES (2001, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'title', 'document', '', 1.0, true, 'Title From PDF', '2026-10-05 10:00:00.000035+00'),
       (2002, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 'doi', 'crossref', 'v9', 0.92, false, '', '2026-10-05 10:00:01.000036+00'),
       (2003, 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'title', 'crossref', 'v9', 0.5, false, 'Candidate A', '2026-10-05 10:00:03.000037+00');

INSERT INTO library_source_revisions (source_id, record_id, rendition_id, revision_id, content_hash, media_type, bibliography, locator_capabilities, content_ticket, origin, created_at)
VALUES ('src-it-1', 'rec-1', 'rend-1', 7, 'hash-it-1', 'application/pdf',
        '{"title":"Title From PDF","creators":[{"family":"Author"}]}', '{"page_labels":true}', 'ticket-it-1', 'import',
        '2026-10-05 10:00:02.000038+00');

INSERT INTO library_write_audit (id, scope, operation, anchor, provider_ref, outcome, readback, at)
VALUES (700, 'zotero|https://api.zotero.org|users/0', 'ensure_record', 'it-idem-1', 'prov-rec-1', 'created', '{"ok":true}', '2026-10-05 10:00:01.000039+00');

INSERT INTO library_provider_anchors (scope, kind, anchor, provider_id, provider_version, created_at)
VALUES ('zotero|https://api.zotero.org|users/0', 'record', 'it-idem-1', 'prov-rec-1', 41, '2026-10-05 10:00:01.000040+00'),
       ('zotero|https://api.zotero.org|users/0', 'rendition', 'prov-rec-1|deadbeef', 'prov-rend-1', 42, '2026-10-05 10:00:01.000041+00');

INSERT INTO library_writer_leases (scope, owner, acquired_at, heartbeat_at)
VALUES ('zotero|https://api.zotero.org|users/0', 'proditest:1:nonce', '2026-10-05 10:00:00.000042+00', '2026-10-05 10:00:03.000043+00');
`
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestBundlePostgres — the full PG evidence chain, in order.
func TestBundlePostgres(t *testing.T) {
	src, cleanupSrc := scratchDB(t, "src")
	defer cleanupSrc()
	seedSource(t, src)
	ctx := context.Background()

	// --- export ---
	bundle := t.TempDir()
	eres, err := Export(ctx, "library", ExportOptions{DSN: src, Out: bundle, Build: "axiom v0.2.2-it (commit test, release build)"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(eres.Manifest.Warnings) != 0 {
		t.Fatalf("seeded source produced warnings: %v", eres.Manifest.Warnings)
	}
	if eres.Manifest.Source.ExportCutoff == "" || eres.Manifest.Source.Engine == "" {
		t.Fatal("manifest lacks cutoff/engine provenance")
	}
	if _, ok := eres.Manifest.Source.Migrations["schema_migrations"]; !ok {
		t.Fatal("manifest lacks the core migration ledger")
	}
	// every table carried, with digests
	if len(eres.Manifest.Tables) != len(LibraryTables) {
		t.Fatalf("bundle carries %d tables, want %d", len(eres.Manifest.Tables), len(LibraryTables))
	}
	byTable := map[string]*TableManifest{}
	for i := range eres.Manifest.Tables {
		tm := &eres.Manifest.Tables[i]
		byTable[tm.Name] = tm
		if tm.RowsSHA256 == "" || len(tm.Batches) == 0 {
			t.Fatalf("table %s lacks digests/batches", tm.Name)
		}
	}
	// the enum vocabulary from pg_enum rode along
	rc := byTable["repair_cases"]
	if len(rc.Enums["status"]) == 0 || !containsStr(rc.Enums["status"], "blocked_for_dudu") {
		t.Fatalf("repair_cases.status vocabulary missing: %v", rc.Enums)
	}
	// status vocabularies from the component spec rode along
	li := byTable["library_imports"]
	if !containsStr(li.Enums["status"], "awaiting_confirmation") {
		t.Fatalf("library_imports.status vocabulary missing: %v", li.Enums)
	}

	// --- import into a fresh PG target ---
	tgt, cleanupTgt := scratchDB(t, "tgt")
	defer cleanupTgt()
	ires, err := Import(ctx, ImportOptions{From: bundle, DSN: tgt})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, ti := range ires.Tables {
		if ti.Skipped || ti.Inserted == 0 || ti.Idempotent != 0 {
			t.Fatalf("table %s: %+v", ti.Table, ti)
		}
	}

	// --- verify: counts, digests, FK invariants ---
	vres, err := Verify(ctx, VerifyOptions{From: bundle, DSN: tgt})
	if err != nil || !vres.OK {
		for _, tv := range vres.Tables {
			if !tv.CountOK || !tv.DigestOK {
				t.Errorf("table %s: countOK=%v digestOK=%v", tv.Table, tv.CountOK, tv.DigestOK)
			}
		}
		t.Fatalf("verify: %v (ok=%v)", err, vres.OK)
	}
	for _, fk := range vres.FKs {
		if fk.Orphans != 0 {
			t.Errorf("FK %s.%s: %d orphans", fk.Table, fk.Constraint, fk.Orphans)
		}
	}

	// --- PG→PG roundtrip IDENTITY: re-export the target, digests equal ---
	bundle2 := t.TempDir()
	eres2, err := Export(ctx, "library", ExportOptions{DSN: tgt, Out: bundle2})
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if len(eres2.Manifest.Tables) != len(eres.Manifest.Tables) {
		t.Fatalf("re-export table count differs")
	}
	for _, tm2 := range eres2.Manifest.Tables {
		tm1 := byTable[tm2.Name]
		if tm1 == nil {
			t.Fatalf("table %s appeared on re-export", tm2.Name)
		}
		if tm1.Count != tm2.Count || tm1.RowsSHA256 != tm2.RowsSHA256 {
			t.Fatalf("table %s NOT identical: counts %d/%d digests %s/%s",
				tm2.Name, tm1.Count, tm2.Count, tm1.RowsSHA256, tm2.RowsSHA256)
		}
	}

	// --- in-flight import survived VERBATIM ---
	pool, err := pgxpool.New(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	var status string
	var evCount int
	if err := pool.QueryRow(ctx,
		`SELECT status FROM library_imports WHERE idempotency_key='it-idem-2'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM library_import_events e JOIN library_imports i ON i.import_id=e.import_id WHERE i.idempotency_key='it-idem-2'`).Scan(&evCount); err != nil {
		t.Fatal(err)
	}
	if status != "awaiting_confirmation" || evCount != 2 {
		t.Fatalf("in-flight import mutated: status=%q events=%d", status, evCount)
	}

	// --- sequence resync: the next bigserial insert must not collide ---
	if _, err := pool.Exec(ctx, `INSERT INTO library_metadata_provenance
		(import_id, field, source, resolver_version, confidence, applied, value, at)
		VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','x','user','',1,true,'','2026-10-05T10:05:00.000000Z'::timestamptz)`); err != nil {
		t.Fatalf("post-import insert collides (sequence not resynced): %v", err)
	}
	var newID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM library_metadata_provenance WHERE field='x'`).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if newID <= 2003 {
		t.Fatalf("sequence behind imported ids: %d", newID)
	}
	// undo the probe so the retry sonde below counts cleanly
	if _, err := pool.Exec(ctx, `DELETE FROM library_metadata_provenance WHERE field='x'`); err != nil {
		t.Fatal(err)
	}

	// --- retry sonde: re-import (merge) NEVER duplicates ---
	ires2, err := Import(ctx, ImportOptions{From: bundle, DSN: tgt, Merge: true})
	if err != nil {
		t.Fatalf("retry import: %v", err)
	}
	for _, ti := range ires2.Tables {
		if ti.Inserted != 0 || ti.Idempotent == 0 {
			t.Fatalf("retry duplicated rows in %s: %+v", ti.Table, ti)
		}
	}
	if vres2, err := Verify(ctx, VerifyOptions{From: bundle, DSN: tgt}); err != nil || !vres2.OK {
		t.Fatalf("post-retry verify: %v %v", vres2, err)
	}

	// --- occupied target without merge is refused ---
	if _, err := Import(ctx, ImportOptions{From: bundle, DSN: tgt}); err == nil ||
		!strings.Contains(err.Error(), "explicit merge mode") {
		t.Fatalf("occupied target accepted: %v", err)
	}

	// --- merge conflict: a diverged target row aborts loudly ---
	if _, err := pool.Exec(ctx, `UPDATE zotero_documents SET title='DIVERGED' WHERE zotero_key='DOC1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, ImportOptions{From: bundle, DSN: tgt, Merge: true}); err == nil ||
		!strings.Contains(err.Error(), "merge conflict") {
		t.Fatalf("diverged row not loud: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE zotero_documents SET title='Permuted Keys Probe' WHERE zotero_key='DOC1'`); err != nil {
		t.Fatal(err)
	}

	// --- bit-flip on a batch file: digest mismatch isolates + aborts ---
	tampered := t.TempDir()
	copyBundle(t, bundle, tampered)
	zd := filepath.Join(tampered, "tables", "zotero_documents", "0000.jsonl")
	raw, err := os.ReadFile(zd)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0x01
	if err := os.WriteFile(zd, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	tgt2, cleanupTgt2 := scratchDB(t, "tamper")
	defer cleanupTgt2()
	if _, err := Import(ctx, ImportOptions{From: tampered, DSN: tgt2}); err == nil ||
		!strings.Contains(err.Error(), "DIGEST MISMATCH") {
		t.Fatalf("bit-flip accepted: %v", err)
	}
	// the isolated batch left NOTHING in the target (zotero_sources was
	// applied before zotero_documents — dependency order — but the
	// tampered table itself must stay empty)
	tp, _ := pgxpool.New(ctx, tgt2)
	var docCount int
	if err := tp.QueryRow(ctx, `SELECT count(*) FROM zotero_documents`).Scan(&docCount); err != nil {
		t.Fatal(err)
	}
	if docCount != 0 {
		t.Fatalf("tampered batch partially applied: %d rows", docCount)
	}
	tp.Close()

	// --- unknown enum: consistent-digest tamper, caught by the TARGET's
	// own enum vocabulary (pg_enum) — and by the manifest vocabulary at
	// decode time even before the engine sees it.
	enumBundle := t.TempDir()
	copyBundle(t, bundle, enumBundle)
	rb := filepath.Join(enumBundle, "tables", "repair_cases", "0000.jsonl")
	raw, err = os.ReadFile(rb)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"healed"`, `"exploded"`, 1))
	if err := os.WriteFile(rb, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := LoadManifest(enumBundle)
	if err != nil {
		t.Fatal(err)
	}
	// mend manifest+sidecar so ONLY the vocabulary teeth can catch it:
	// drop the manifest's declared vocab (as-if-tampered) — the TARGET
	// pg_enum check must still refuse.
	tm := man.Table("repair_cases")
	tm.Batches[0].SHA256 = sha256Hex(raw)
	tm.Enums = nil
	if err := writeManifest(enumBundle, man); err != nil {
		t.Fatal(err)
	}
	tgt3, cleanupTgt3 := scratchDB(t, "enum")
	defer cleanupTgt3()
	_, err = Import(ctx, ImportOptions{From: enumBundle, DSN: tgt3})
	if err == nil || !strings.Contains(err.Error(), "vocabulary") {
		t.Fatalf("unknown enum accepted: %v", err)
	}

	// --- PG→SQLite type portability: same bundle into library.sqlite ---
	sqlitePath := filepath.Join(t.TempDir(), "library.sqlite")
	sres, err := Import(ctx, ImportOptions{From: bundle, SQLitePath: sqlitePath})
	if err != nil {
		t.Fatalf("sqlite import: %v", err)
	}
	skipped := 0
	for _, ti := range sres.Tables {
		if ti.Skipped {
			skipped++
		}
	}
	if skipped != 10 { // the legacy mirror tables have no SQLite home
		t.Fatalf("expected 10 legacy skips, got %d", skipped)
	}
	svres, err := Verify(ctx, VerifyOptions{From: bundle, SQLitePath: sqlitePath})
	if err != nil || !svres.OK {
		for _, tv := range svres.Tables {
			if !tv.CountOK || !tv.DigestOK {
				t.Errorf("sqlite table %s: countOK=%v digestOK=%v", tv.Table, tv.CountOK, tv.DigestOK)
			}
		}
		t.Fatalf("sqlite verify: %v ok=%v", err, svres.OK)
	}
	if len(svres.Pragmas) != 2 {
		t.Fatalf("sqlite pragma verdicts: %v", svres.Pragmas)
	}
	pool.Close()
}

// TestImportUnderDMLRole — the import runs against the DM07 runtime
// role (axiom_library): DML only, zero DDL, sequence resync included.
// Same pre-existence discipline as the DM07 drill: cluster-global roles
// are foreign state; refuse (or fail under AXIOM_REQUIRE_DRILL=1).
func TestImportUnderDMLRole(t *testing.T) {
	admin := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set — PG-gated bundle tests skip")
	}
	ctx := context.Background()
	ap, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	var preexist int
	if err := ap.QueryRow(ctx,
		`SELECT count(*) FROM pg_roles WHERE rolname IN ('axiom_library','axiom_store')`).Scan(&preexist); err != nil {
		t.Fatal(err)
	}
	if preexist > 0 {
		if os.Getenv("AXIOM_REQUIRE_DRILL") == "1" {
			t.Fatalf("drill required but roles pre-exist on this cluster (%d) — disposable mirror needed", preexist)
		}
		t.Skipf("axiom_library/axiom_store already exist on this cluster (%d) — refusing to touch foreign roles", preexist)
	}

	src, cleanupSrc := scratchDB(t, "rolesrc")
	defer cleanupSrc()
	seedSource(t, src)
	bundle := t.TempDir()
	if _, err := Export(ctx, "library", ExportOptions{DSN: src, Out: bundle}); err != nil {
		t.Fatalf("export: %v", err)
	}

	tgt, cleanupTgt := scratchDB(t, "roletgt")
	defer cleanupTgt()

	// roles + grants + passwords (the DM07 file, applied ON THE TARGET
	// DATABASE — grants are per-database — twice, idempotent)
	rolesSQL, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "postgres", "roles.sql"))
	if err != nil {
		t.Fatalf("read roles.sql: %v", err)
	}
	ttp, err := pgxpool.New(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := ttp.Exec(ctx, string(rolesSQL)); err != nil {
			t.Fatalf("roles.sql apply #%d: %v", i+1, err)
		}
	}
	ttp.Close()
	const libPW = "dm04-bundle-drill"
	if _, err := ap.Exec(ctx, fmt.Sprintf(`ALTER ROLE axiom_library PASSWORD '%s'`, libPW)); err != nil {
		t.Skipf("cannot set drill password (needs superuser — CI runs it): %v", err)
	}
	defer func() {
		// grants live on the target DB (dropped only AFTER this defer —
		// LIFO): DROP OWNED first, or DROP ROLE fails on dependents
		// (the leak the second drill run surfaced as a skip).
		if tp, err := pgxpool.New(ctx, tgt); err == nil {
			_, _ = tp.Exec(ctx, `DROP OWNED BY axiom_library, axiom_store`)
			tp.Close()
		}
		_, _ = ap.Exec(ctx, `DROP ROLE IF EXISTS axiom_store`)
		_, _ = ap.Exec(ctx, `DROP ROLE IF EXISTS axiom_library`)
	}()

	// the import under the DML-only role: everything green, sequence
	// resync included (UPDATE on sequences granted for DM04).
	roleDSN := dsnWithCredentials(tgt, "axiom_library", libPW)
	if _, err := Import(ctx, ImportOptions{From: bundle, DSN: roleDSN}); err != nil {
		t.Fatalf("import under axiom_library: %v", err)
	}
	if vres, err := Verify(ctx, VerifyOptions{From: bundle, DSN: roleDSN}); err != nil || !vres.OK {
		t.Fatalf("verify under axiom_library: %v ok=%v", err, vres.OK)
	}
}

// dsnWithCredentials swaps user/password in a DSN.
func dsnWithCredentials(dsn, user, pass string) string {
	i := strings.Index(dsn, "://")
	rest := dsn[i+3:]
	if j := strings.IndexByte(rest, '@'); j >= 0 {
		return dsn[:i+3] + user + ":" + pass + "@" + rest[j+1:]
	}
	return dsn
}

// copyBundle duplicates a bundle directory.
func copyBundle(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyBundle(t, s, d)
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
