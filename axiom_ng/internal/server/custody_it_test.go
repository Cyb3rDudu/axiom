// custody_it_test.go — #279 DoD integration proof: the manual custody
// endpoint against the REAL database (projection + sync), a fake Zotero
// write server, and the REAL quarantine filesystem. Pins: full protocol
// (original quarantined, old item deleted, healed uploaded), idempotent
// refusal after success, abort-after-quarantine → re-run completes, and
// the healed attachment landing PREFERRED after the next sync.
//
// Gated like the other DB suites: AXIOM_TEST_DATABASE_URL (scratch DB).
package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	axiomsync "github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// custSource is a mutable canonical source: the IT mutates the item set
// between syncs (attachment deleted + healed sibling created by the
// custody endpoint) and forces full snapshots so reconcile-by-absence
// removes the old attachment from the projection.
type custSource struct {
	mu      sync.Mutex
	baseURL string
	items   []zotero.CanonicalItem
	version int64
}

func (c *custSource) ServerID() string { return "cust-it" }
func (c *custSource) ListCanonicalItems(since int64) (zotero.CanonicalBatch, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return zotero.CanonicalBatch{FullSnapshot: true, Items: c.items, NewVersion: c.version}, nil
}
func (c *custSource) ListCanonicalCollections() ([]zotero.CanonicalCollection, error) {
	return nil, nil
}
func (c *custSource) set(items []zotero.CanonicalItem, version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items, c.version = items, version
}

// custItem builds a canonical item envelope (book or attachment).
func custItem(key, itemType, parent string, data map[string]any, enclosure string) zotero.CanonicalItem {
	m := map[string]any{"key": key, "version": 1, "itemType": itemType}
	if parent != "" {
		m["parentItem"] = parent
	}
	for k, v := range data {
		m[k] = v
	}
	env := map[string]any{"key": key, "version": 1, "data": m}
	if enclosure != "" {
		env["links"] = map[string]any{"enclosure": map[string]any{"href": "file://" + enclosure}}
	}
	envB, _ := json.Marshal(env)
	dataB, _ := json.Marshal(m)
	return zotero.CanonicalItem{Key: key, Version: 1, ItemType: itemType, ParentKey: parent, Envelope: envB, Data: dataB}
}

// custWriteZotero: the fake local Zotero WRITE surface for the custody
// mutations (version-guarded delete + 3-phase create). failDelete forces
// the DELETE status (0 = 204; 404 = item already gone — the resume case).
type custWriteZotero struct {
	mu         sync.Mutex
	failDelete int
	newKey     string
	versions   map[string]int64
	deletes    []string
	uploaded   string
}

func (f *custWriteZotero) server(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/users/0/items/")
		key := strings.SplitN(p, "/", 2)[0]
		switch {
		case r.Method == http.MethodGet:
			v, ok := f.versions[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Last-Modified-Version", jsonNumber(v))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			if _, ok := f.versions[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			st := f.failDelete
			if st == 0 {
				st = http.StatusNoContent
				f.deletes = append(f.deletes, key)
				delete(f.versions, key)
			}
			w.WriteHeader(st)
		case r.Method == http.MethodPost && r.URL.Path == "/api/users/0/items":
			w.Write([]byte(`{"successful":{"0":{"key":"` + f.newKey + `"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/file"):
			b, _ := io.ReadAll(r.Body)
			if strings.HasPrefix(string(b), "upload=") {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Write([]byte(`{"url":"` + srv.URL + `/upload", "uploadKey":"uk"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/upload"):
			b, _ := io.ReadAll(r.Body)
			f.uploaded = string(b)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonNumber(v int64) string { return strconv.FormatInt(v, 10) }

// custITEnv seeds book BOOK1 + broken attachment ATT1 (local file), syncs,
// and returns the server + write fake + source for mutation.
func custITEnv(t *testing.T) (*Server, *repo.Repo, *custWriteZotero, *custSource, string, string, string) {
	t.Helper()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL not set; skipping custody IT")
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

	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.pdf")
	healed := filepath.Join(dir, "healed.pdf")
	os.WriteFile(broken, []byte("broken original bytes"), 0o644)
	os.WriteFile(healed, []byte("healed bytes"), 0o644)
	qroot := filepath.Join(dir, "quarantine")

	src := &custSource{baseURL: "http://cust-it.local"}
	src.set([]zotero.CanonicalItem{
		custItem("BOOK1", "book", "", map[string]any{
			"title": "Nachhaltiges Personalmanagement",
			"creators": []map[string]string{{"firstName": "Adrian", "lastName": "Geursen", "creatorType": "author"}},
			"date":   "2022",
		}, ""),
		custItem("ATT1", "attachment", "BOOK1", map[string]any{
			"contentType": "application/pdf", "filename": "broken.pdf",
		}, broken),
	}, 5)

	rep := repo.New(d.Pool())
	if _, err := axiomsync.New(src, rep, src.baseURL, "users/0", log.Default()).Run(ctx, nil); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool().Exec(ctx, `DELETE FROM zotero_sources WHERE base_url=$1`, src.baseURL)
	})

	fw := &custWriteZotero{newKey: "HEALED1", versions: map[string]int64{"ATT1": 2}}
	s := New(":0", nil)
	s.SetRepairAPI(rep, zotero.NewWriteClient(fw.server(t).URL, "srv", "key"), qroot)
	return s, rep, fw, src, broken, healed, qroot
}

func postCustody(s *Server, key, reason, healedPath string) *httptest.ResponseRecorder {
	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("attachment_key", key)
	_ = mw.WriteField("reason", reason)
	f, _ := os.Open(healedPath)
	defer f.Close()
	fw, _ := mw.CreateFormFile("healed_file", filepath.Base(healedPath))
	_, _ = io.Copy(fw, f)
	_ = mw.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/repair/custody", strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	s.ServeHTTP(rec, req)
	return rec
}

func TestIT_CustodyFullProtocolHealedPreferredAfterSync(t *testing.T) {
	s, rep, fw, src, _, healed, qroot := custITEnv(t)
	ctx := context.Background()

	rec := postCustody(s, "ATT1", "manuelle Reparatur nach Fixer-HALT (Geursen-Fall)", healed)
	if rec.Code != http.StatusOK {
		t.Fatalf("custody call: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		NewAttachmentKey string `json:"new_attachment_key"`
		QuarantinePath   string `json:"quarantine_path"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.NewAttachmentKey != "HEALED1" {
		t.Fatalf("healed key: %+v %s", body, rec.Body.String())
	}

	// custody proofs on the filesystem: original quarantined, record written
	qb, err := os.ReadFile(body.QuarantinePath)
	if err != nil || string(qb) != "broken original bytes" {
		t.Fatalf("quarantine must hold the original: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(qroot, "manual", "ATT1.json"))
	if err != nil {
		t.Fatalf("custody record: %v", err)
	}
	var recd map[string]any
	_ = json.Unmarshal(raw, &recd)
	for _, want := range []string{"attachment_key", "reason", "original_path", "created_at", "steps"} {
		if _, ok := recd[want]; !ok {
			t.Fatalf("record missing %q: %s", want, raw)
		}
	}
	fw.mu.Lock()
	deletes := append([]string{}, fw.deletes...)
	uploaded := fw.uploaded
	fw.mu.Unlock()
	if len(deletes) != 1 || deletes[0] != "ATT1" {
		t.Fatalf("old item must be deleted once: %v", deletes)
	}
	if !strings.Contains(uploaded, "healed bytes") {
		t.Fatalf("healed bytes not uploaded: %q", uploaded)
	}

	// idempotence: a second call is refused with 409 — no duplicate sibling
	rec2 := postCustody(s, "ATT1", "versehentlicher zweiter Versuch", healed)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second custody call must 409, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// the improvised Geursen state cannot recur: after sync, the healed
	// attachment is the ONLY active one and PREFERRED
	src.set([]zotero.CanonicalItem{
		custItem("BOOK1", "book", "", map[string]any{
			"title": "Nachhaltiges Personalmanagement",
			"creators": []map[string]string{{"firstName": "Adrian", "lastName": "Geursen", "creatorType": "author"}},
			"date":   "2022",
		}, ""),
		custItem("HEALED1", "attachment", "BOOK1", map[string]any{
			"contentType": "application/pdf", "filename": "Geursen - 2022 - Nachhaltiges Personalmanagement.pdf",
		}, healed),
	}, 6)
	if _, err := axiomsync.New(src, rep, src.baseURL, "users/0", log.Default()).Run(ctx, nil); err != nil {
		t.Fatalf("post-custody sync: %v", err)
	}
	var pref struct {
		ZoteroKey string
		Deleted   bool
	}
	err = rep.Pool().QueryRow(ctx, `
		SELECT a.zotero_key, a.deleted FROM zotero_attachments a
		WHERE a.parent_zotero_key='BOOK1' AND a.preferred=true`).Scan(&pref.ZoteroKey, &pref.Deleted)
	if err != nil || pref.ZoteroKey != "HEALED1" || pref.Deleted {
		t.Fatalf("healed attachment must be the preferred active one: %+v %v", pref, err)
	}
	var oldDeleted bool
	if err := rep.Pool().QueryRow(ctx, `
		SELECT deleted FROM zotero_attachments WHERE zotero_key='ATT1'`).Scan(&oldDeleted); err != nil || !oldDeleted {
		t.Fatalf("old attachment must be deleted after sync: %v %v", oldDeleted, err)
	}
}

// TestIT_CustodyAbortAfterQuarantineRerunCompletes — the #279 DoD resume
// proof at the endpoint level: run 1 dies at the delete (write gateway
// error) with the quarantine already on disk; the re-run completes.
func TestIT_CustodyAbortAfterQuarantineRerunCompletes(t *testing.T) {
	s, _, fw, _, _, healed, qroot := custITEnv(t)

	fw.mu.Lock()
	fw.failDelete = http.StatusInternalServerError // run 1: gateway dies at the delete
	fw.mu.Unlock()
	rec := postCustody(s, "ATT1", "Abbruch-Test", healed)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("run 1 must fail at the delete (502), got %d: %s", rec.Code, rec.Body.String())
	}
	// the aborted run's report: quarantine done, protocol failed
	raw, err := os.ReadFile(filepath.Join(qroot, "manual", "ATT1.json"))
	if err != nil {
		t.Fatalf("aborted run must leave a record: %v", err)
	}
	var recd struct {
		Status string `json:"status"`
		Steps  []struct {
			Action string `json:"action"`
		} `json:"steps"`
	}
	_ = json.Unmarshal(raw, &recd)
	if recd.Status != "failed" || len(recd.Steps) == 0 || recd.Steps[0].Action != "quarantine" {
		t.Fatalf("aborted record must show quarantine done + failed: %s", raw)
	}

	// run 2: gateway recovered; the item is still there (run 1 never
	// deleted it) — a normal delete completes the protocol
	fw.mu.Lock()
	fw.failDelete = 0
	fw.mu.Unlock()
	rec2 := postCustody(s, "ATT1", "Abbruch-Test — zweiter Lauf", healed)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-run must complete, got %d: %s", rec2.Code, rec2.Body.String())
	}
	raw2, _ := os.ReadFile(filepath.Join(qroot, "manual", "ATT1.json"))
	var recd2 struct {
		Status string `json:"status"`
		Steps  []struct {
			Action string `json:"action"`
		} `json:"steps"`
	}
	_ = json.Unmarshal(raw2, &recd2)
	if recd2.Status != "healed" {
		t.Fatalf("re-run must heal: %s", raw2)
	}
	// both runs' steps are on the record (audit trail across the abort)
	quarantines := 0
	for _, st := range recd2.Steps {
		if st.Action == "quarantine" {
			quarantines++
		}
	}
	if quarantines != 2 {
		t.Fatalf("record must show both runs' quarantine steps: %s", raw2)
	}
}

// TestIT_CustodyGuards — the trust-boundary guards fire BEFORE any
// mutation: unknown key 404, missing reason 400, empty healed file 400,
// unwired write client 503.
func TestIT_CustodyGuards(t *testing.T) {
	s, rep, fw, _, _, healed, qroot := custITEnv(t)

	// unknown key
	rec := postCustody(s, "NOPE1", "grund", healed)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key must 404, got %d", rec.Code)
	}
	// missing reason (empty string)
	rec = postCustody(s, "ATT1", "", healed)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing reason must 400, got %d", rec.Code)
	}
	// empty healed file
	empty := filepath.Join(t.TempDir(), "leer.pdf")
	os.WriteFile(empty, nil, 0o644)
	rec = postCustody(s, "ATT1", "grund", empty)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty healed file must 400, got %d", rec.Code)
	}
	// unwired write client: 503 before anything else
	s2 := New(":0", nil)
	s2.SetRepairAPI(rep, nil, qroot)
	rec = postCustody(s2, "ATT1", "grund", healed)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired write client must 503, got %d: %s", rec.Code, rec.Body.String())
	}
	// nothing mutated by the guard failures
	fw.mu.Lock()
	deletes := len(fw.deletes)
	fw.mu.Unlock()
	if deletes != 0 {
		t.Fatalf("guards must fire before any mutation, saw %d deletes", deletes)
	}
	if _, err := os.Stat(filepath.Join(qroot, "manual", "ATT1.json")); !os.IsNotExist(err) {
		t.Fatalf("no record must exist after guard-only calls")
	}
}

// TestIT_CustodyAmbiguousCreateRefused — a record with a create key but no
// terminal status is the ambiguous-resume state (the upload may have
// succeeded server-side): the endpoint refuses with 409 and a DIFFERENT
// error than the healed case, before any mutation (review W1).
func TestIT_CustodyAmbiguousCreateRefused(t *testing.T) {
	s, _, fw, _, _, healed, qroot := custITEnv(t)

	stale := map[string]any{
		"attachment_key": "ATT1", "status": "failed", "new_attachment_key": "HEALED1",
		"steps": []map[string]any{{"action": "quarantine"}, {"action": "delete_attachment"}, {"action": "failed"}},
	}
	raw, _ := json.Marshal(stale)
	os.MkdirAll(filepath.Join(qroot, "manual"), 0o755)
	os.WriteFile(filepath.Join(qroot, "manual", "ATT1.json"), raw, 0o644)

	rec := postCustody(s, "ATT1", "re-run nach Create-Abbruch", healed)
	if rec.Code != http.StatusConflict {
		t.Fatalf("ambiguous create must 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "HEALED1") {
		t.Fatalf("refusal must name the recorded create key: %s", rec.Body.String())
	}
	fw.mu.Lock()
	deletes := len(fw.deletes)
	uploaded := fw.uploaded
	fw.mu.Unlock()
	if deletes != 0 || uploaded != "" {
		t.Fatalf("ambiguous-create refusal must fire before any mutation: %d deletes, uploaded %q", deletes, uploaded)
	}
}
