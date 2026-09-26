package server

// #285 review follow-up: the orphan-ack OPERATOR SURFACE — the HTTP field
// name (orphan_resolved), the decode-to-repo wiring, and the status codes
// — driven through the REAL handler and the REAL guard SQL. The guard
// logic itself is pinned by TestRequeueOrphanGuardIT (repo suite); this
// test pins the wiring: a dropped or renamed field must fail HERE, not in
// production. Gated like the other DB suites (AXIOM_TEST_DATABASE_URL).
import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/repair"

	"github.com/go-chi/chi/v5"
)

// dsnDatabaseName extracts the database segment of a DSN (repo-package
// convention, mirrored here for the guard below).
func dsnDatabaseName(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
			if q := strings.IndexAny(rest, "?#"); q >= 0 {
				rest = rest[:q]
			}
			return rest
		}
	}
	return dsn
}

func TestIT_RequeueOrphanAckSurface(t *testing.T) {
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping requeue orphan surface IT")
	}
	// DSN guard (repo-package convention): a misconfigured DSN must NEVER
	// run these INSERTs against a non-test database.
	if !strings.HasSuffix(dsnDatabaseName(dsn), "_test") {
		t.Fatalf("refusing to run against non-test database %q (must end in _test)", dsnDatabaseName(dsn))
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rstore := repair.NewStore(d.Pool())

	// minimal seed: source → document → attachment, then a case walked to
	// failed WITH an unresolved orphan audit (the ambiguous-create residue)
	sfx := time.Now().UnixNano()
	orphanKey := fmt.Sprintf("RQORPH%d", sfx)
	var srcID, docID, attID string
	if err := d.Pool().QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ($1, 'lib-rq', 'test-server') RETURNING id::text`,
		"https://requeue-orphan-it.local/"+time.Now().Format("150405.000000000")).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool().QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1, $2, 1, 'book', 'Requeue Orphan IT') RETURNING id::text`,
		srcID, "RQDOC"+time.Now().Format("150405")).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	ch := fmt.Sprintf("requeue-orphan-hash-%d", sfx)
	if err := d.Pool().QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path, content_hash)
		VALUES ($1, $2, $3, 1, 'P', 'imported_file', 'application/pdf', 'x.pdf', '/tmp/x.pdf', $4)
		RETURNING id::text`, srcID, docID, "RQATT"+time.Now().Format("150405"), &ch).Scan(&attID); err != nil {
		t.Fatal(err)
	}
	c, _, err := rstore.CreateRepairCase(ctx, attID, docID, "reparierbar", []byte(`{}`))
	if err != nil || c == nil {
		t.Fatalf("CreateRepairCase: %v %v", c, err)
	}
	if err := rstore.QueueRepairCase(ctx, c.ID, "reparierbar", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := rstore.ClaimRepairCase(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := rstore.AuditWrite(ctx, c.ID, attID, "create_attachment_orphan",
		map[string]any{"new_zotero_key": orphanKey, "filename": "Autor - 2020 - Titel.pdf"}); err != nil {
		t.Fatal(err)
	}
	if err := rstore.MarkRepairFailed(ctx, c.ID, "zotero create: upload 502"); err != nil {
		t.Fatal(err)
	}

	s := &Server{repairStore: rstore} // requeue touches nothing else
	r := chi.NewRouter()
	r.Post("/api/repair/cases/{id}/requeue", s.handleRepairRequeue)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/repair/cases/"+c.ID+"/requeue", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	// blind requeue → 409 naming the orphan key
	rec := post(`{"reason": "retry"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), orphanKey) {
		t.Fatalf("blind requeue must 409 naming the key, got %d: %s", rec.Code, rec.Body.String())
	}
	// wrong ack → 409
	rec = post(`{"reason": "retry", "orphan_resolved": "WRONG"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("wrong ack must 409, got %d: %s", rec.Code, rec.Body.String())
	}
	// the operator surface: correct key in orphan_resolved → 200, case queued
	rec = post(`{"reason": "orphan gelöscht", "orphan_resolved": "` + orphanKey + `"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct ack must requeue (200), got %d: %s", rec.Code, rec.Body.String())
	}
	got, err := rstore.OpenRepairCase(ctx, attID)
	if err != nil || got == nil || got.Status != repair.RepairQueued {
		t.Fatalf("case must be queued after the ack, got %+v %v", got, err)
	}
}
