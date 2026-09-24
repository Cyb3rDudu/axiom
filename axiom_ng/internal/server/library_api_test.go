// library_api_test.go — the F06 import HTTP contract over httptest with
// the REAL LibraryService (fake providers, scratch DB — gated like the
// other DB suites): 202/status/confirm/retry, idempotency replay 200 +
// payload mismatch 409, magic-byte rejection, provenance in the status
// body. The dev-env live twin lives in library_api_live_test.go.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	axlibrary "github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	"github.com/jackc/pgx/v5/pgxpool"
)

// libraryTestDB provisions the library-only scratch DB (same convention
// as internal/library's IT harness, independent copy for the server
// package).
func libraryTestDB(t *testing.T) (*axlibrary.Store, func()) {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping library API IT")
	}
	base := dbOfDSN(dsn)
	if !strings.HasSuffix(base, "_test") {
		t.Fatalf("refusing to run against non-_test database %q", base)
	}
	dbName := strings.TrimSuffix(base, "_test") + fmt.Sprintf("_libsrv%d_test", os.Getpid())
	ctx := context.Background()
	admin := mustPool(t, ctx, dsn)
	_, _ = admin.Exec(ctx, fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid<>pg_backend_pid()`, dbName))
	_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	admin.Close()
	pool := mustPool(t, ctx, withDBDSN(dsn, dbName))
	st := axlibrary.NewStore(pool)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("library migrate: %v", err)
	}
	return st, func() {
		pool.Close()
		if a := mustPool(t, context.Background(), dsn); a != nil {
			_, _ = a.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
			a.Close()
		}
	}
}

func mustPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	return pool
}

func dbOfDSN(dsn string) string {
	name := dsn
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, "?"); i >= 0 {
		name = name[:i]
	}
	return name
}

func withDBDSN(dsn, newDB string) string {
	i := strings.LastIndex(dsn, "/")
	head, tail := dsn[:i+1], dsn[i+1:]
	if j := strings.Index(tail, "?"); j >= 0 {
		return head + newDB + "?" + tail[j+1:]
	}
	return head + newDB
}

// newLibraryTestServer builds the server with the real service over fake
// providers and returns its base URL.
func newLibraryTestServer(t *testing.T) (*httptest.Server, *axlibrary.FakeProvider) {
	t.Helper()
	st, cleanup := libraryTestDB(t)
	t.Cleanup(cleanup)
	prov := axlibrary.NewFakeProvider()
	svc := axlibrary.NewService(axlibrary.Config{
		SourceID: "src-library-test", Provider: "fake", LibraryID: "users/0",
	}, st, axlibrary.NewStaging(t.TempDir()), axlibrary.Ports{
		Catalog:     prov,
		Records:     prov,
		Renditions:  prov,
		Collections: prov,
		Resolvers: []axlibrary.BibliographicResolver{
			axlibrary.NewFakeResolver("crossref", "fake-v1", axlibrary.StandardCrossrefFixtures()),
			axlibrary.NewFakeResolver("open_library", "fake-v1", axlibrary.StandardOpenLibraryFixtures()),
		},
		Documents: axlibrary.FakeDocumentInspector{},
	})
	srv := New("127.0.0.1:0", testLogger())
	srv.SetLibraryAPI(svc)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, prov
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// postImport issues the multipart intake.
func postImport(t *testing.T, base string, reqJSON string, filename string, content []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("request", reqJSON); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(base+"/api/v1/library/imports", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func importReqJSON(key, title, doi string) string {
	b, _ := json.Marshal(library.ImportRequest{
		IdempotencyKey: key,
		RecordType:     "book",
		Target:         library.ImportTarget{LibraryID: "users/0"},
		MetadataHints:  library.MetadataHints{Title: title, DOI: doi},
	})
	return string(b)
}

func TestLibraryImportHTTPContract(t *testing.T) {
	ts, prov := newLibraryTestServer(t)

	pdf := []byte("%PDF-1.4\nHTTP Contract Fixture.\n%AXIOM-LANG: en\n\nBody.")

	// 202 + status_url + committed via status route.
	code, body := postImport(t, ts.URL, importReqJSON("http-1", "HTTP Contract Fixture", ""), "contract.pdf", pdf)
	if code != http.StatusAccepted {
		t.Fatalf("fresh intake = %d, want 202 (%v)", code, body)
	}
	importID, _ := body["import_id"].(string)
	if importID == "" || body["status_url"] != "/api/v1/library/imports/"+importID {
		t.Fatalf("202 body malformed: %v", body)
	}
	op := getStatus(t, ts.URL, importID)
	if op["status"] != "committed" {
		t.Fatalf("status = %v, want committed (%v)", op["status"], op)
	}

	// Replay: 200 + the SAME import_id.
	code, body = postImport(t, ts.URL, importReqJSON("http-1", "HTTP Contract Fixture", ""), "contract.pdf", pdf)
	if code != http.StatusOK {
		t.Fatalf("replay = %d, want 200", code)
	}
	if id, _ := body["import_id"].(string); id != importID {
		t.Fatalf("replay diverged: %v vs %v", id, importID)
	}
	if recs, _, _, _ := prov.Snapshot(); recs != 1 {
		t.Fatalf("replay doubled provider records: %d", recs)
	}

	// Payload mismatch under the same key: 409.
	code, body = postImport(t, ts.URL, importReqJSON("http-1", "A Different Title", ""), "contract.pdf", pdf)
	if code != http.StatusConflict {
		t.Fatalf("payload mismatch = %d, want 409 (%v)", code, body)
	}

	// Magic bytes: a renamed non-PDF file is InvalidArgument (400).
	code, body = postImport(t, ts.URL, importReqJSON("http-magic", "Some Title", ""), "evil.pdf", []byte("<html>not a pdf</html>"))
	if code != http.StatusBadRequest {
		t.Fatalf("foreign magic bytes = %d, want 400 (%v)", code, body)
	}

	// Unknown import: 404; unwired shape: 404 (route registered, no service).
	resp, err := http.Get(ts.URL + "/api/v1/library/imports/imp-void")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown import = %d, want 404", resp.StatusCode)
	}
}

// TestLibraryImportConfirmRetryHTTP — the ambiguous fixture over HTTP:
// awaiting_confirmation, confirm route completes the CHOSEN candidate;
// retry route refuses a non-retryable-failed import.
func TestLibraryImportConfirmRetryHTTP(t *testing.T) {
	ts, _ := newLibraryTestServer(t)
	pdf := []byte("%PDF-1.4\nNetwork Effects intro.\n%AXIOM-LANG: en\n\nBody.")

	code, body := postImport(t, ts.URL, importReqJSON("http-amb", axlibrary.StandardLadderFixtures.AmbiguousTitle, ""), "amb.pdf", pdf)
	if code != http.StatusAccepted {
		t.Fatalf("intake = %d (%v)", code, body)
	}
	importID, _ := body["import_id"].(string)
	op := getStatus(t, ts.URL, importID)
	if op["status"] != "awaiting_confirmation" {
		t.Fatalf("status = %v, want awaiting_confirmation", op["status"])
	}

	// Confirm the second candidate (deliberate non-first choice).
	decisions, _ := op["decisions"].([]any)
	if len(decisions) != 1 {
		t.Fatalf("decisions: %v", op["decisions"])
	}
	dec := decisions[0].(map[string]any)
	cands, _ := dec["candidates"].([]any)
	if len(cands) != 2 {
		t.Fatalf("candidates: %v", dec["candidates"])
	}
	chosen := cands[1].(map[string]any)
	confirmReq, _ := json.Marshal(map[string]string{
		"decision_id":  dec["decision_id"].(string),
		"candidate_id": chosen["candidate_id"].(string),
	})
	resp, err := http.Post(ts.URL+"/api/v1/library/imports/"+importID+"/confirm", "application/json", bytes.NewReader(confirmReq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var confirmed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&confirmed)
	if resp.StatusCode != http.StatusOK || confirmed["status"] != "committed" {
		t.Fatalf("confirm = %d %v", resp.StatusCode, confirmed)
	}

	// Retry on a committed import: 409 (only retry-fähige steps continue).
	resp2, err := http.Post(ts.URL+"/api/v1/library/imports/"+importID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("retry on committed = %d, want 409", resp2.StatusCode)
	}
}

// TestLibraryUnwiredRoutes404 — the nil-service shape: routes exist but
// answer 404 (sourceSecret pattern).
func TestLibraryUnwiredRoutes404(t *testing.T) {
	srv := New("127.0.0.1:0", testLogger())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	post404 := func(path string) {
		t.Helper()
		resp, err := http.Post(ts.URL+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s unwired = %d, want 404", path, resp.StatusCode)
		}
	}
	post404("/api/v1/library/imports")
	post404("/api/v1/library/imports/xyz/confirm")
	post404("/api/v1/library/imports/xyz/retry")
	resp, err := http.Get(ts.URL + "/api/v1/library/imports/xyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET status unwired = %d, want 404", resp.StatusCode)
	}
}

func getStatus(t *testing.T, base, importID string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/library/imports/" + importID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var op map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&op)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status route = %d (%v)", resp.StatusCode, op)
	}
	return op
}
