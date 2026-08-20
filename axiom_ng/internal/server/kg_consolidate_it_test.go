package server

// #197 IT: POST /api/kg/consolidate against a REAL test database — the
// endpoint must run the proven ConsolidateEntities merge, answer with the
// merge numbers (merged, duplicate forms before/after), and be a NO-OP on
// the second call. Mutates tables, so (unlike the read-only kg_it_test)
// it refuses any DSN whose database name does not end in "_test".
//
// Run with:
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../axiom_consol_test?sslmode=disable \
//   go test ./internal/server/ -run TestIT_KGConsolidate -v

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5/pgxpool"
)

func consolidateITServer(t *testing.T) (*Server, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping consolidation IT")
	}
	// Hard guard (same convention as the repo package): this test
	// TRUNCATES processing tables — only a *_test database is acceptable.
	name := consDSNDatabase(dsn)
	if !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to run against non-test database %q (must end in _test)", name)
	}
	d, err := db.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := New(":0", nil)
	s.SetConsolidateService(repo.New(d.Pool()))
	return s, d.Pool()
}

// consDSNDatabase extracts the database name from a DSN (local copy of
// the repo test helper — test files are not importable across packages).
func consDSNDatabase(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
			if k := strings.Index(rest, "?"); k >= 0 {
				rest = rest[:k]
			}
			return rest
		}
	}
	return ""
}

// consSeedSnapshot seeds source -> document -> attachment -> ACTIVE snapshot
// and returns the snapshot id. Idempotent (ON CONFLICT DO NOTHING + SELECT):
// the persistent test DB carries earlier runs' rows — the snapshot itself
// is re-created per run (processing tables are truncated up front).
func consSeedSnapshot(t *testing.T, pool *pgxpool.Pool, attKey string) string {
	t.Helper()
	ctx := t.Context()
	var srcID, docID, attID, snapID string
	if _, err := pool.Exec(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ('https://zotero.test/' || $1, 'users/0', $1)
		ON CONFLICT (base_url, library_id) DO NOTHING`, attKey); err != nil {
		t.Fatalf("seed source %s: %v", attKey, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM zotero_sources WHERE base_url = 'https://zotero.test/' || $1`, attKey).Scan(&srcID); err != nil {
		t.Fatalf("lookup source %s: %v", attKey, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1::uuid, $2, 1, 'book', $2 || ' Buch')
		ON CONFLICT (source_id, zotero_key) DO NOTHING`, srcID, "DOC"+attKey); err != nil {
		t.Fatalf("seed document %s: %v", attKey, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM zotero_documents WHERE source_id=$1::uuid AND zotero_key=$2`,
		srcID, "DOC"+attKey).Scan(&docID); err != nil {
		t.Fatalf("lookup document %s: %v", attKey, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, preferred)
		VALUES ($1::uuid, $2::uuid, $3, 1, $4, 'linked_file', 'application/pdf', $3 || '.pdf', true)
		ON CONFLICT (source_id, zotero_key) DO NOTHING`, srcID, docID, attKey, "DOC"+attKey); err != nil {
		t.Fatalf("seed attachment %s: %v", attKey, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM zotero_attachments WHERE source_id=$1::uuid AND zotero_key=$2`,
		srcID, attKey).Scan(&attID); err != nil {
		t.Fatalf("lookup attachment %s: %v", attKey, err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1::uuid, $2, 'test', '1', 'p1', $3::uuid, '{}', true)
		RETURNING id::text`, attID, attKey, docID).Scan(&snapID); err != nil {
		t.Fatalf("seed snapshot %s: %v", attKey, err)
	}
	return snapID
}

// consSeedEntity seeds an entity with nChunks mentions into a snapshot.
func consSeedEntity(t *testing.T, pool *pgxpool.Pool, snapID, ref, form string, nChunks int) string {
	t.Helper()
	ctx := t.Context()
	var entID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form)
		VALUES ($1::uuid, $2, $3, $3) RETURNING id::text`, snapID, ref, form).Scan(&entID); err != nil {
		t.Fatalf("seed entity %s: %v", form, err)
	}
	for i := 0; i < nChunks; i++ {
		var cID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO processing_chunks (snapshot_id, chunk_index, text, token_count)
			VALUES ($1::uuid,
			        (SELECT coalesce(max(chunk_index), -1) + 1 FROM processing_chunks WHERE snapshot_id = $1::uuid),
			        $2, 10) RETURNING id::text`, snapID, form+" inhalt").Scan(&cID); err != nil {
			t.Fatalf("seed chunk for %s: %v", form, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
			VALUES ($1::uuid, $2::uuid, 0, 1)`, entID, cID); err != nil {
			t.Fatalf("seed mention %s: %v", form, err)
		}
	}
	return entID
}

func postConsolidate(t *testing.T, s *Server) (int, repo.ConsolidationReport) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/kg/consolidate", nil))
	var rep repo.ConsolidationReport
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("bad report body: %s", rec.Body.String())
		}
	}
	return rec.Code, rep
}

func TestIT_KGConsolidateEndpointIdempotent(t *testing.T) {
	s, pool := consolidateITServer(t)
	ctx := t.Context()

	// Isolate the graph tables (guarded *_test DB only — see helper).
	if _, err := pool.Exec(ctx, `
		TRUNCATE processing_entity_relationships, processing_entity_mentions,
		         processing_entities, processing_chunks, processing_snapshots
		CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	snapA := consSeedSnapshot(t, pool, "KGCONEA")
	snapB := consSeedSnapshot(t, pool, "KGCONEB")
	// Same canonical form across two documents (3 vs 5 chunks — B survives)
	// plus a same-snapshot duplicate with a verbatim span on A's first chunk
	// (the c1e0e82 live-blocker class): merged must be 2.
	consSeedEntity(t, pool, snapA, "de-a", "deutschland", 3)
	consSeedEntity(t, pool, snapB, "de-b", "deutschland", 5)
	var dupID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form)
		VALUES ($1::uuid, 'de-dup', 'deutschland', 'deutschland') RETURNING id::text`,
		snapA).Scan(&dupID); err != nil {
		t.Fatalf("seed same-snapshot duplicate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
		SELECT $1::uuid, chunk_id, start_char, end_char
		FROM processing_entity_mentions m
		JOIN processing_entities e ON e.id = m.entity_id
		WHERE e.snapshot_id = $2::uuid AND e.ref = 'de-a' LIMIT 1`, dupID, snapA); err != nil {
		t.Fatalf("seed verbatim duplicate span: %v", err)
	}
	// A distinct form that must stay untouched.
	consSeedEntity(t, pool, snapA, "nh", "nachhaltigkeit", 2)

	// Run 1: the merge executes and the numbers come back.
	code, rep := postConsolidate(t, s)
	if code != http.StatusOK {
		t.Fatalf("POST #1: want 200, got %d", code)
	}
	if rep.Merged != 2 || rep.DuplicateFormsBefore != 1 || rep.DuplicateFormsAfter != 0 {
		t.Fatalf("POST #1 report: want merged=2 duplicate_forms 1->0, got %+v", rep)
	}

	// DB truth: exactly ONE active deutschland entity with 3+5=8 distinct
	// chunks (the duplicate's verbatim span was skipped, not doubled).
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id
		WHERE e.canonical_form='deutschland' AND s.active`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("one active deutschland entity must remain, got %d", n)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT m.chunk_id) FROM processing_entity_mentions m
		JOIN processing_entities e ON e.id = m.entity_id
		WHERE e.canonical_form='deutschland' AND EXISTS (
		  SELECT 1 FROM processing_snapshots s WHERE s.id = e.snapshot_id AND s.active)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("survivor must hold 8 distinct chunks, got %d", n)
	}

	// Run 2: NO-OP — merged=0, nothing duplicated before or after.
	code2, rep2 := postConsolidate(t, s)
	if code2 != http.StatusOK {
		t.Fatalf("POST #2: want 200, got %d", code2)
	}
	if rep2 != (repo.ConsolidationReport{}) {
		t.Fatalf("POST #2 must be a no-op (zero report), got %+v", rep2)
	}
}
