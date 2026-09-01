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
// Direction ruling (corrected #233): the backfill enriches EPUB-active
// snapshots ONLY. A PDF-active document is refused whole — its sibling page
// map is circular — and PDF chunks (physical_only/blind) are unantastbar.
// The direction-proof test pins that: a PDF-active doc offered as target
// enriches NOTHING and names the reason (red under mutation: removing the
// planner guard makes it enrich and the test fail).
//
// Also proven here: enrichment (folio-less epub_cfi chunks of the ACTIVE
// snapshot gain derived_from_sibling print pages in ONE transaction, type
// and chapter untouched), whole-backfill refusal on a non-monotone
// candidate (nothing written), idempotency (second run no-op), dry-run
// (writes nothing), re-index shape (bulk _update only, doc id = chunk
// UUID, no delete/recreate).
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

// testdata book: 10 page-sections, print page == section ordinal; chunk
// texts replicate the deterministic folgNwM token scheme.
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

// fixture installs a source/document/attachment (kind = "epub"|"pdf") and an
// ACTIVE snapshot whose chunks are folio-less. EPUB chunks carry epub_cfi
// locators (page_source=none); PDF chunks page_span physical_only — the
// contract-real folio-less states of each format.
func fixture(t *testing.T, pool *pgxpool.Pool, key, kind, filePath string) []string {
	t.Helper()
	ctx := context.Background()
	contentType := "application/epub+zip"
	if kind == "pdf" {
		contentType = "application/pdf"
	}
	var srcID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id) VALUES ('https://lb.test/'||$1::text, 'users/0')
		RETURNING id::text`, key).Scan(&srcID); err != nil {
		t.Fatalf("source: %v", err)
	}
	var docID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, $2::text, 1, 'book', 'LB Book '||$2::text)
		RETURNING id::text`, srcID, key).Scan(&docID); err != nil {
		t.Fatalf("document: %v", err)
	}
	var attID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments
		  (source_id, document_id, zotero_key, zotero_version, parent_zotero_key,
		   link_mode, content_type, filename, local_path, content_hash)
		VALUES ($1, $2, $3::text, 1, $3::text, 'imported_file', $4, 'lb.'||$5::text, $6, $7)
		RETURNING id::text`, srcID, docID, key, contentType, kind, filePath, "lbhash"+key).Scan(&attID); err != nil {
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
		loc := fmt.Sprintf(`{"type":"epub_cfi","page_source":"none","chapter":3,
			"cfi_start":"epubcfi(/6/2!/4/%d)"}`, 2*i)
		if kind == "pdf" {
			// 0-based physical pages (contract §11): section i on physical i-1
			loc = fmt.Sprintf(`{"type":"page_span","page_source":"physical_only","chapter":3,
				"physical_page_start":%d,"physical_page_end":%d}`, i-1, i-1)
		}
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
				rd, _ := filepath.Abs(filepath.Join("..", "..", "axiom_ng_runner"))
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

	epub := testdataPath(t, "book.epub")
	chunks := fixture(t, pool, "LB1", "epub", epub)

	os_ := newOSStub(t)
	opts := Options{
		DocKey: "LB1", EpubPath: epub, Budget: 2 * time.Minute,
		Python: python, RunnerDir: runnerDir,
		OSBaseURL: os_.srv.URL,
		Logf:      func(string, ...any) {},
	}

	// --- 1. enrichment run (EPUB-active happy path) ---
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
	// type/chapter untouched.
	for i, id := range chunks {
		var loc []byte
		if err := pool.QueryRow(ctx,
			`SELECT locator FROM processing_chunks WHERE id=$1`, id).Scan(&loc); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		var l struct {
			Type       string `json:"type"`
			PageSource string `json:"page_source"`
			PageStart  *int   `json:"page_start"`
			PageEnd    *int   `json:"page_end"`
			Chapter    *int   `json:"chapter"`
		}
		_ = json.Unmarshal(loc, &l)
		if l.PageSource == DerivedFromSibling {
			if l.PageStart == nil || *l.PageStart != i+1 {
				t.Fatalf("chunk %d: derived page_start=%v want %d", i, l.PageStart, i+1)
			}
			if l.Chapter == nil || *l.Chapter != 3 {
				t.Fatalf("chunk %d: chapter must stay authoritative, got %v", i, l.Chapter)
			}
			if l.Type != "epub_cfi" {
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
	fixture(t, pool, "LB2", "epub", epub)
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
	var derived int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id
		JOIN zotero_documents d ON d.id = sn.document_id
		WHERE d.zotero_key='LB2' AND c.locator->>'page_source'=$1`, DerivedFromSibling).
		Scan(&derived); err != nil {
		t.Fatalf("refusal check: %v", err)
	}
	if derived != 0 {
		t.Fatalf("refusal must write nothing: %d chunks derived", derived)
	}

	// --- 4. dry-run writes nothing ---
	fixture(t, pool, "LB3", "epub", epub)
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
	var dried int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id
		JOIN zotero_documents d ON d.id = sn.document_id
		WHERE d.zotero_key='LB3' AND c.locator->>'page_source'=$1`, DerivedFromSibling).
		Scan(&dried); err != nil {
		t.Fatalf("dry check: %v", err)
	}
	if dried != 0 {
		t.Fatalf("dry-run must write nothing: %d chunks derived", dried)
	}
}

// TestBackfillDirectionPDFRefused is the direction ruling's proof
// (corrected #233): a PDF-ACTIVE document — even with only
// physical_only/blind chunks, i.e. maximally enrichment-hungry — is refused
// whole, with the direction reason, and NOTHING is written. Red under
// mutation: remove the planner's PDF guard (or the epub_cfi UPDATE
// predicate) and this test fails because the run would enrich.
func TestBackfillDirectionPDFRefused(t *testing.T) {
	pool := openBackfillDB(t)
	python, runnerDir := resolvePython(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM zotero_documents WHERE zotero_key LIKE 'LB4%'`); err != nil {
		t.Fatalf("clean: %v", err)
	}

	pdf := testdataPath(t, "book.pdf")
	fixture(t, pool, "LB4", "pdf", pdf)

	os_ := newOSStub(t)
	rep, err := Run(ctx, pool, Options{
		DocKey: "LB4", EpubPath: testdataPath(t, "book.epub"), Budget: 2 * time.Minute,
		Python: python, RunnerDir: runnerDir, OSBaseURL: os_.srv.URL,
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("pdf-direction run: %v", err)
	}
	if !rep.Refused {
		t.Fatalf("PDF-active document must be refused whole (direction ruling), got %+v", rep)
	}
	if rep.Updated != 0 || rep.Reindexed != 0 {
		t.Fatalf("PDF-active run must write nothing, got Updated=%d Reindexed=%d", rep.Updated, rep.Reindexed)
	}
	if !strings.Contains(rep.RefusedReason, "EPUB-active") {
		t.Fatalf("refusal must name the direction reason, got %q", rep.RefusedReason)
	}
	if os_.count() != 0 {
		t.Fatalf("PDF-active run must not touch the index, got %d actions", os_.count())
	}
	// every physical_only chunk untouched
	var untouched int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id
		JOIN zotero_documents d ON d.id = sn.document_id
		WHERE d.zotero_key='LB4' AND c.locator->>'page_source'='physical_only'`).
		Scan(&untouched); err != nil {
		t.Fatalf("direction check: %v", err)
	}
	if untouched != 10 {
		t.Fatalf("PDF chunks are unantastbar: %d/10 still physical_only (want 10)", untouched)
	}
}
