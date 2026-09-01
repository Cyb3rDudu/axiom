// #233 locator-backfill integration tests: REAL Postgres (cloned throwaway
// test database, the repo IT convention) + the REAL Python alignment engine
// over the committed synthetic testdata pair, + an OpenSearch stub that
// proves the re-index is a bulk _update on the existing chunk docs.
//
// Gates (skip, never fail, when the environment lacks the piece):
//   - AXIOM_TEST_DATABASE_URL unset   -> skip (unit-only environments)
//   - no runner venv python           -> skip (Go-only CI; the alignment
//     core has its own Python suite in axiom_ng_runner/tests)
//
// Proven here (the workorder's evidence items 2-4):
//   - enrichment: folio-less (page_source=none) chunks of the ACTIVE
//     snapshot gain derived_from_sibling print pages in ONE transaction,
//     chapter/physical fields untouched
//   - refusal: a non-monotone candidate page map refuses the WHOLE backfill
//     and writes nothing
//   - idempotency: a second run over the unchanged document is a no-op
//   - dry-run: writes nothing
//   - re-index: only update actions, doc id = chunk UUID, no delete/recreate
package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

var backfillTestDatabaseName = fmt.Sprintf("axiom_ng_backfill_%d_test", os.Getpid())

func openBackfillDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping backfill integration test")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	ctx := context.Background()
	maintDSN := cloneDSN(u, "postgres")
	mp, err := pgxpool.New(ctx, maintDSN)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	var exists bool
	if err := mp.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`,
		backfillTestDatabaseName).Scan(&exists); err != nil {
		t.Fatalf("check db exists: %v", err)
	}
	if !exists {
		if _, err := mp.Exec(ctx, `CREATE DATABASE `+backfillTestDatabaseName); err != nil {
			t.Fatalf("create backfill test db: %v", err)
		}
	}
	mp.Close()
	d, err := db.Open(ctx, cloneDSN(u, backfillTestDatabaseName))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(d.Close)
	return d.Pool()
}

func cloneDSN(u *url.URL, dbname string) string {
	cp := *u
	cp.Path = "/" + dbname
	return cp.String()
}

// testdata book: 10 page-sections, print page == physical page; chunk texts
// replicate the deterministic folgNwM token scheme.
func sectionText(n int) string {
	words := make([]string, 130)
	for m := range words {
		words[m] = fmt.Sprintf("folg%dw%d", n, m)
	}
	return strings.Join(words, " ")
}

func testdataPath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("testdata", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("testdata %s: %v", name, err)
	}
	abs, _ := filepath.Abs(p)
	return abs
}

// fixture installs a source/document/pdf attachment/ACTIVE snapshot with
// folio-less chunks (one per page-section) and returns the doc key + chunk
// ids in page order.
func fixture(t *testing.T, pool *pgxpool.Pool, key, pdfPath string) []string {
	t.Helper()
	ctx := context.Background()
	var srcID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id) VALUES ('https://lb.test/'||$1::text, 'users/0')
		RETURNING id::text`, key).Scan(&srcID); err != nil {
		t.Fatalf("source: %v", err)
	}
	var docID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, $2, 1, 'book', 'LB Book '||$2::text)
		RETURNING id::text`, srcID, key).Scan(&docID); err != nil {
		t.Fatalf("document: %v", err)
	}
	var attID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments
		  (source_id, document_id, zotero_key, zotero_version, parent_zotero_key,
		   link_mode, content_type, filename, local_path, content_hash)
		VALUES ($1, $2, $3, 1, $3, 'imported_file', 'application/pdf', 'lb.pdf', $4, $5)
		RETURNING id::text`, srcID, docID, key, pdfPath, "lbhash"+key).Scan(&attID); err != nil {
		t.Fatalf("attachment: %v", err)
	}
	var snapID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots
		  (attachment_id, content_hash, processor_name, processor_version,
		   profile_hash, document_id, profile, active)
		VALUES ($1, $2, 'test', '1', $3, $4, 'default', true)
		RETURNING id::text`, attID, "lbhash"+key, "ph"+key, docID).Scan(&snapID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ids := make([]string, 10)
	for i := 1; i <= 10; i++ {
		loc := fmt.Sprintf(`{"type":"page_span","page_source":"none","chapter":3,
			"physical_page_start":%d,"physical_page_end":%d}`, i, i)
		if err := pool.QueryRow(ctx, `
			INSERT INTO processing_chunks (snapshot_id, chunk_index, text, locator)
			VALUES ($1, $2, $3, $4) RETURNING id::text`,
			snapID, i-1, sectionText(i), loc).Scan(&ids[i-1]); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	return ids
}

// osStub fakes the OpenSearch _bulk endpoint and records every action.
type osStub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	actions []map[string]any
	docs    []map[string]any
}

func newOSStub(t *testing.T) *osStub {
	t.Helper()
	s := &osStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		var items []map[string]any
		s.mu.Lock()
		for i := 0; i+1 < len(lines); i += 2 {
			var action map[string]any
			var doc map[string]any
			_ = json.Unmarshal([]byte(lines[i]), &action)
			_ = json.Unmarshal([]byte(lines[i+1]), &doc)
			s.actions = append(s.actions, action)
			s.docs = append(s.docs, doc)
			items = append(items, map[string]any{"update": map[string]any{}})
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": false, "items": items})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *osStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.actions)
}

func resolvePython(t *testing.T) (string, string) {
	t.Helper()
	for _, c := range []string{
		os.Getenv("AXIOM_RUNNER_PYTHON"),
		filepath.Join("..", "..", "axiom_ng_runner", ".venv", "bin", "python"),
	} {
		if c == "" {
			continue
		}
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				rd, _ := filepath.Abs(filepath.Join("..", "..", "..", "axiom_ng_runner"))
				return abs, rd
			}
		}
	}
	t.Skip("no runner venv python (set AXIOM_RUNNER_PYTHON); skipping engine-backed IT")
	return "", ""
}

func TestBackfillEnrichRefuseIdempotent(t *testing.T) {
	pool := openBackfillDB(t)
	python, runnerDir := resolvePython(t)
	ctx := context.Background()
	// clean slate for our fixtures
	if _, err := pool.Exec(ctx, `DELETE FROM zotero_documents WHERE zotero_key LIKE 'LB%'`); err != nil {
		t.Fatalf("clean: %v", err)
	}

	pdf := testdataPath(t, "book.pdf")
	epub := testdataPath(t, "book.epub")
	chunks := fixture(t, pool, "LB1", pdf)

	os_ := newOSStub(t)
	opts := Options{
		DocKey: "LB1", EpubPath: epub, Budget: 2 * time.Minute,
		Python: python, RunnerDir: runnerDir,
		OSBaseURL: os_.srv.URL,
		Logf:      func(string, ...any) {},
	}

	// --- 1. enrichment run ---
	rep, err := Run(ctx, pool, opts)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if rep.Refused {
		t.Fatalf("run 1 refused: %s", rep.RefusedReason)
	}
	if rep.Updated == 0 || rep.Updated > 10 {
		t.Fatalf("run 1: unexpected Updated=%d", rep.Updated)
	}
	if rep.Reindexed != rep.Updated {
		t.Fatalf("run 1: Reindexed=%d != Updated=%d", rep.Reindexed, rep.Updated)
	}

	// DB state: enriched locators carry derived_from_sibling + print pages,
	// chapter/physical untouched.
	for i, id := range chunks {
		var loc []byte
		if err := pool.QueryRow(ctx,
			`SELECT locator FROM processing_chunks WHERE id=$1`, id).Scan(&loc); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		var l struct {
			Type           string `json:"type"`
			PageSource     string `json:"page_source"`
			PageStart      *int   `json:"page_start"`
			PageEnd        *int   `json:"page_end"`
			PageLabelStart string `json:"page_label_start"`
			Chapter        *int   `json:"chapter"`
			PhysStart      *int   `json:"physical_page_start"`
		}
		_ = json.Unmarshal(loc, &l)
		if l.PageSource == "derived_from_sibling" {
			if l.PageStart == nil || *l.PageStart != i+1 {
				t.Fatalf("chunk %d: derived page_start=%v want %d", i, l.PageStart, i+1)
			}
			if l.PageLabelStart != fmt.Sprint(i+1) {
				t.Fatalf("chunk %d: page_label_start=%q (LocatorView renders this)", i, l.PageLabelStart)
			}
			if l.Chapter == nil || *l.Chapter != 3 {
				t.Fatalf("chunk %d: chapter must stay authoritative, got %v", i, l.Chapter)
			}
			if l.PhysStart == nil || *l.PhysStart != i+1 {
				t.Fatalf("chunk %d: physical_page_start must be preserved", i)
			}
			if l.Type != "page_span" {
				t.Fatalf("chunk %d: type must be untouched", i)
			}
		}
	}

	// OS stub: ONLY update actions on the chunks index, id = chunk UUID.
	if os_.count() != rep.Updated {
		t.Fatalf("os actions=%d != updated=%d", os_.count(), rep.Updated)
	}
	byID := map[string]bool{}
	for _, id := range chunks {
		byID[id] = true
	}
	for i, a := range os_.actions {
		upd, _ := a["update"].(map[string]any)
		if upd == nil || upd["_index"] != IndexName {
			t.Fatalf("action %d: not an update on %s: %v", i, IndexName, a)
		}
		if !byID[fmt.Sprint(upd["_id"])] {
			t.Fatalf("action %d: doc id %v is not an enriched chunk UUID (ids must stay stable)", i, upd["_id"])
		}
		doc := os_.docs[i]["doc"].(map[string]any)
		if _, ok := doc["locator"]; !ok {
			t.Fatalf("action %d: update doc lacks locator", i)
		}
	}

	// --- 2. idempotency: second run is a no-op ---
	rep2, err := Run(ctx, pool, opts)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if rep2.Updated != 0 || rep2.Reindexed != 0 {
		t.Fatalf("run 2 must be a no-op, got Updated=%d Reindexed=%d", rep2.Updated, rep2.Reindexed)
	}

	// --- 3. refusal: non-monotone candidate refuses the whole backfill ---
	chunksB := fixture(t, pool, "LB2", pdf)
	_ = chunksB
	repR, err := Run(ctx, pool, Options{
		DocKey: "LB2", EpubPath: testdataPath(t, "poisoned.epub"), Budget: 2 * time.Minute,
		Python: python, RunnerDir: runnerDir, OSBaseURL: os_.srv.URL,
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("refusal run: %v", err)
	}
	if !repR.Refused {
		t.Fatalf("poisoned candidate must refuse the whole backfill, got %+v", repR.Plan)
	}
	var none int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id
		JOIN zotero_documents d ON d.id = sn.document_id
		WHERE d.zotero_key='LB2' AND c.locator->>'page_source'='none'`).
		Scan(&none); err != nil {
		t.Fatalf("refusal check: %v", err)
	}
	if none != 10 {
		t.Fatalf("refusal must write nothing: %d/10 chunks still none (want 10)", none)
	}

	// --- 4. dry-run writes nothing ---
	chunksC := fixture(t, pool, "LB3", pdf)
	_ = chunksC
	repD, err := Run(ctx, pool, Options{
		DocKey: "LB3", EpubPath: epub, DryRun: true, Budget: 2 * time.Minute,
		Python: python, RunnerDir: runnerDir, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if repD.Updated != 0 || repD.Reindexed != 0 {
		t.Fatalf("dry-run must not write")
	}
	var stillNone int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id
		JOIN zotero_documents d ON d.id = sn.document_id
		WHERE d.zotero_key='LB3' AND c.locator->>'page_source'='none'`).
		Scan(&stillNone); err != nil {
		t.Fatalf("dry check: %v", err)
	}
	if stillNone != 10 {
		t.Fatalf("dry-run must leave all 10 chunks untouched, got %d", stillNone)
	}
}
