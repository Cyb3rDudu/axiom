// #279 manual custody tests: the full protocol via repair.Apply +
// ManualDeps against a fake Zotero write server (hermetic — no DB, no live
// Zotero), the abort→re-run resume path, and the 404-tolerant delete.
package repair

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// fakeWriteZotero implements the write surface: version-guarded DELETE of
// the broken key, 3-phase create of the healed attachment. deleteStatus
// forces the DELETE result (0 = 204 default); 404 is the resume case.
type fakeWriteZotero struct {
	mu           sync.Mutex
	brokenKey    string
	newKey       string
	deleteStatus int
	deletes      []string         // keys that got a SUCCESSFUL delete
	versions     map[string]int64 // key -> current version (deleted keys absent)
	createdFile  []byte           // uploaded bytes
	createdName  string           // uploaded filename
	failCreate   bool
	failUpload   bool // phase 2 (bytes) 500s → orphan-cleanup path
}

func newFakeWrite(brokenKey string) *fakeWriteZotero {
	return &fakeWriteZotero{
		brokenKey: brokenKey, newKey: "NEW1",
		deleteStatus: 0, versions: map[string]int64{brokenKey: 3},
	}
}

func (f *fakeWriteZotero) server(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	item := func(key string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if key != f.brokenKey && key != f.newKey {
			return false
		}
		if _, ok := f.versions[key]; !ok {
			return false // deleted items 404 (GET probe included)
		}
		return true
	}
	handle := func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/users/0/items/")
		key := strings.SplitN(p, "/", 2)[0]
		switch {
		case r.Method == http.MethodGet && item(key):
			f.mu.Lock()
			v := f.versions[key]
			f.mu.Unlock()
			w.Header().Set("Last-Modified-Version", itoa(v))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			f.mu.Lock()
			st := f.deleteStatus
			f.mu.Unlock()
			if st == 0 {
				st = http.StatusNoContent
			}
			if st == http.StatusNoContent {
				if !item(key) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				f.mu.Lock()
				f.deletes = append(f.deletes, key)
				delete(f.versions, key)
				f.mu.Unlock()
			}
			w.WriteHeader(st)
		case r.Method == http.MethodPost && r.URL.Path == "/api/users/0/items":
			f.mu.Lock()
			fail := f.failCreate
			f.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`{"successful":{"0":{"key":"` + f.newKey + `"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/file"):
			// authorize: {url, uploadKey}; register: upload=<key> -> 204
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			body := string(buf[:n])
			if strings.HasPrefix(body, "upload=") {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			// authorize answers {url, uploadKey} with a LOCAL upload URL
			// (phase 2 goes to the same fake host); register (upload=<key>)
			// 204s above.
			w.Write([]byte(`{"url":"` + srv.URL + `/upload", "uploadKey":"uk-1"}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload"):
			// phase 2: the bytes (multipart — capture raw, assert contains)
			f.mu.Lock()
			fail := f.failUpload
			f.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			buf := make([]byte, 1<<20)
			n, _ := r.Body.Read(buf)
			f.mu.Lock()
			f.createdFile = buf[:n]
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	mux.HandleFunc("/", handle)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func itoa(v int64) string {
	if v == 0 {
		return "1"
	}
	return string(rune('0' + v))
}

func TestManualCustodyFullProtocol(t *testing.T) {
	root := t.TempDir()
	orig := filepath.Join(root, "orig.pdf")
	original := []byte("broken original")
	if err := os.WriteFile(orig, original, 0o644); err != nil {
		t.Fatal(err)
	}
	fw := newFakeWrite("BROKEN1")
	srv := fw.server(t)
	wc := zotero.NewWriteClient(srv.URL, "srv", "key")

	rec := &ManualRecord{
		AttachmentKey: "BROKEN1", DocumentKey: "PARENT1",
		Reason: "manual repair after fixer HALT", OriginalPath: orig,
		ContentType: "application/pdf", CreatedAt: ManualNow(),
	}
	deps := &ManualDeps{Write: wc, Root: root, Record: rec, RunID: ManualRunID("BROKEN1")}
	res, err := Apply(context.Background(), deps, root, ApplyCase{
		CaseID: "manual-BROKEN1", AttachmentKey: "BROKEN1", DocumentKey: "PARENT1",
		Title: "Nachhaltiges Personalmanagement", Year: 2022, Publisher: "Springer",
		SrcPath: orig, ContentType: "application/pdf",
	}, []byte("healed bytes"))
	if err != nil {
		t.Fatal(err)
	}

	// custody: original in the quarantine area
	qb, err := os.ReadFile(res.Quarantine)
	if err != nil || string(qb) != string(original) {
		t.Fatalf("quarantine must hold the original bytes: %v %q", err, string(qb))
	}
	// old item deleted (version-guarded — the fake asserted the header)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if len(fw.deletes) != 1 || fw.deletes[0] != "BROKEN1" {
		t.Fatalf("broken item must be deleted once, got %v", fw.deletes)
	}
	if !strings.Contains(string(fw.createdFile), "healed bytes") {
		t.Fatalf("healed bytes must be uploaded, got %q", string(fw.createdFile))
	}

	// record: original path, key, reason, date — and the step sequence
	persisted, err := LoadManualRecord(root, "BROKEN1")
	if err != nil || persisted == nil {
		t.Fatalf("record must persist: %v %v", persisted, err)
	}
	if persisted.OriginalPath != orig || persisted.AttachmentKey != "BROKEN1" ||
		persisted.Reason == "" || persisted.CreatedAt == "" {
		t.Fatalf("record must list original path, key, reason, date: %+v", persisted)
	}
	if persisted.Status != "healed" || persisted.NewAttachmentKey != "NEW1" {
		t.Fatalf("record must be healed with the new key: %+v", persisted)
	}
	wantSteps := []string{"quarantine", "delete_attachment", "create_attachment", "healed"}
	if len(persisted.Steps) != len(wantSteps) {
		t.Fatalf("steps: %+v", persisted.Steps)
	}
	for i, want := range wantSteps {
		if persisted.Steps[i].Action != want {
			t.Fatalf("step %d = %s, want %s (custody order): %+v", i, persisted.Steps[i].Action, want, persisted.Steps)
		}
	}
	if persisted.Filename == "" || !strings.HasSuffix(persisted.Filename, ".pdf") {
		t.Fatalf("schema filename must be recorded: %q", persisted.Filename)
	}
}

// TestManualCustodyAbortAfterQuarantineThenRerunCompletes — the #279 DoD:
// run 1 dies at the delete (gateway error) AFTER quarantine; the record
// shows quarantine done, status failed. Run 2: the old item is already
// gone from Zotero (404) — resume-success — and the protocol completes.
func TestManualCustodyAbortAfterQuarantineThenRerunCompletes(t *testing.T) {
	root := t.TempDir()
	orig := filepath.Join(root, "orig.pdf")
	os.WriteFile(orig, []byte("original"), 0o644)
	fw := newFakeWrite("BROKEN1")
	fw.deleteStatus = http.StatusInternalServerError // run 1: gateway dies
	srv := fw.server(t)
	wc := zotero.NewWriteClient(srv.URL, "srv", "key")

	caseArgs := ApplyCase{
		CaseID: "manual-BROKEN1", AttachmentKey: "BROKEN1", DocumentKey: "P1",
		Title: "T", Year: 2020, SrcPath: orig, ContentType: "application/pdf",
	}
	rec := &ManualRecord{AttachmentKey: "BROKEN1", DocumentKey: "P1",
		Reason: "abort test", OriginalPath: orig, ContentType: "application/pdf", CreatedAt: ManualNow()}
	deps := &ManualDeps{Write: wc, Root: root, Record: rec, RunID: ManualRunID("BROKEN1")}
	if _, err := Apply(context.Background(), deps, root, caseArgs, []byte("healed")); err == nil {
		t.Fatal("run 1 must fail at the delete")
	}
	rec1, err := LoadManualRecord(root, "BROKEN1")
	if err != nil || rec1.Status != "failed" {
		t.Fatalf("aborted run must record status failed: %+v %v", rec1, err)
	}
	if len(rec1.Steps) == 0 || rec1.Steps[0].Action != "quarantine" {
		t.Fatalf("aborted run must show the completed quarantine step: %+v", rec1.Steps)
	}
	last := rec1.Steps[len(rec1.Steps)-1].Action
	if last != "failed" {
		t.Fatalf("last step of an aborted run is the failure record, got %s", last)
	}

	// run 2: the gateway now answers the delete with 404 (the item was
	// already deleted before the first run errored — the operator re-ran
	// after the outage; either way the item is GONE and that is done).
	fw.mu.Lock()
	fw.deleteStatus = http.StatusNotFound
	delete(fw.versions, "BROKEN1")
	fw.mu.Unlock()
	deps2 := &ManualDeps{Write: wc, Root: root, Record: rec1, RunID: ManualRunID("BROKEN1")}
	res, err := Apply(context.Background(), deps2, root, caseArgs, []byte("healed"))
	if err != nil {
		t.Fatalf("re-run must complete: %v", err)
	}
	rec2, _ := LoadManualRecord(root, "BROKEN1")
	if rec2.Status != "healed" || rec2.NewAttachmentKey != "NEW1" {
		t.Fatalf("re-run must heal: %+v", rec2)
	}
	// the record keeps BOTH runs (audit trail), quarantine ran twice
	// (custody-conservative: a re-run quarantines again, never trusts the
	// first copy implicitly)
	quarantines := 0
	for _, s := range rec2.Steps {
		if s.Action == "quarantine" {
			quarantines++
		}
	}
	if quarantines != 2 {
		t.Fatalf("both runs must have quarantined, got %d", quarantines)
	}
	if _, err := os.Stat(res.Quarantine); err != nil {
		t.Fatalf("second quarantine copy must exist: %v", err)
	}
}

// TestManualDelete404ToleranceNotBlanket — only 404 is resume-success; a
// version conflict (412, concurrent modification) still fails the run.
func TestManualDelete404ToleranceNotBlanket(t *testing.T) {
	for _, tc := range []struct {
		status int
		ok     bool
	}{
		{http.StatusNotFound, true},
		{http.StatusPreconditionFailed, false},
		{http.StatusInternalServerError, false},
	} {
		root := t.TempDir()
		fw := newFakeWrite("K1")
		fw.deleteStatus = tc.status
		srv := fw.server(t)
		wc := zotero.NewWriteClient(srv.URL, "srv", "key")
		deps := &ManualDeps{Write: wc, Root: root,
			Record: &ManualRecord{AttachmentKey: "K1"}, RunID: "r"}
		err := deps.DeleteAttachment("K1")
		if tc.ok && err != nil {
			t.Fatalf("404 must be resume-success, got %v", err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("status %d must NOT be swallowed", tc.status)
		}
	}
}

// TestManualCustodyAbortAtCreateThenRerunCompletes — the create-phase
// abort matrix (complement to the delete-phase abort above): run 1 dies
// AT the create (gateway 500 on the item POST) with quarantine + delete
// already done; the record holds NO new key yet. Run 2: create recovered
// → the protocol completes and heals. (The variant where the create
// PARTIALLY succeeded server-side is what the endpoint's
// ambiguous-create 409 guard exists for — that key-on-record refusal is
// pinned at the endpoint level in the custody IT.)
func TestManualCustodyAbortAtCreateThenRerunCompletes(t *testing.T) {
	root := t.TempDir()
	orig := filepath.Join(root, "orig.pdf")
	os.WriteFile(orig, []byte("original"), 0o644)
	fw := newFakeWrite("BROKEN1")
	fw.failCreate = true // run 1: the item POST dies
	srv := fw.server(t)
	wc := zotero.NewWriteClient(srv.URL, "srv", "key")

	caseArgs := ApplyCase{
		CaseID: "manual-BROKEN1", AttachmentKey: "BROKEN1", DocumentKey: "P1",
		Title: "T", Year: 2020, SrcPath: orig, ContentType: "application/pdf",
	}
	rec := &ManualRecord{AttachmentKey: "BROKEN1", DocumentKey: "P1",
		Reason: "create-abort test", OriginalPath: orig, ContentType: "application/pdf", CreatedAt: ManualNow()}
	deps := &ManualDeps{Write: wc, Root: root, Record: rec, RunID: ManualRunID("BROKEN1")}
	if _, err := Apply(context.Background(), deps, root, caseArgs, []byte("healed")); err == nil {
		t.Fatal("run 1 must fail at the create")
	}
	rec1, err := LoadManualRecord(root, "BROKEN1")
	if err != nil || rec1.Status != "failed" {
		t.Fatalf("aborted run must record status failed: %+v %v", rec1, err)
	}
	if rec1.NewAttachmentKey != "" {
		t.Fatalf("a failed item POST must not record a new key: %+v", rec1)
	}
	// quarantine AND delete completed before the create died
	actions := []string{}
	for _, s := range rec1.Steps {
		actions = append(actions, s.Action)
	}
	if len(actions) < 2 || actions[0] != "quarantine" || actions[1] != "delete_attachment" {
		t.Fatalf("steps before the create failure: %v", actions)
	}

	// run 2: create recovered; the old item is already gone (deleted in
	// run 1) — the delete 404s and that is resume-success
	fw.mu.Lock()
	fw.failCreate = false
	fw.mu.Unlock()
	deps2 := &ManualDeps{Write: wc, Root: root, Record: rec1, RunID: ManualRunID("BROKEN1")}
	if _, err := Apply(context.Background(), deps2, root, caseArgs, []byte("healed")); err != nil {
		t.Fatalf("re-run must complete: %v", err)
	}
	rec2, _ := LoadManualRecord(root, "BROKEN1")
	if rec2.Status != "healed" || rec2.NewAttachmentKey != "NEW1" {
		t.Fatalf("re-run must heal with the new key: %+v", rec2)
	}
	quarantines := 0
	for _, s := range rec2.Steps {
		if s.Action == "quarantine" {
			quarantines++
		}
	}
	if quarantines != 2 {
		t.Fatalf("both runs must have quarantined, got %d", quarantines)
	}
}

// TestManualCustodyOrphanKeyReachesRecord — the review-MAJOR pin: when the
// write gateway mints the item but the upload fails AND the best-effort
// cleanup delete fails too, CreateAttachmentWithFile returns (key, err).
// Apply discards the key on its error path — ManualDeps must lift it onto
// the record (step + NewAttachmentKey, durable before the failure return),
// because the endpoint's ambiguous-create guard refuses re-runs on exactly
// that field. Without the lift, the runbook's "nach 502 erneut aufrufen"
// mints a second sibling.
func TestManualCustodyOrphanKeyReachesRecord(t *testing.T) {
	root := t.TempDir()
	orig := filepath.Join(root, "orig.pdf")
	os.WriteFile(orig, []byte("original"), 0o644)
	fw := newFakeWrite("BROKEN1")
	fw.failUpload = true // item minted, upload dies
	// NEW1 stays absent from versions on purpose: the orphan-cleanup GET
	// 404s → cleanup delete fails → the client returns (NEW1, err)
	srv := fw.server(t)
	wc := zotero.NewWriteClient(srv.URL, "srv", "key")

	rec := &ManualRecord{AttachmentKey: "BROKEN1", DocumentKey: "P1",
		Reason: "orphan test", OriginalPath: orig, ContentType: "application/pdf", CreatedAt: ManualNow()}
	deps := &ManualDeps{Write: wc, Root: root, Record: rec, RunID: ManualRunID("BROKEN1")}
	_, err := Apply(context.Background(), deps, root, ApplyCase{
		CaseID: "manual-BROKEN1", AttachmentKey: "BROKEN1", DocumentKey: "P1",
		Title: "T", Year: 2020, SrcPath: orig, ContentType: "application/pdf",
	}, []byte("healed"))
	if err == nil {
		t.Fatal("run must fail at the upload")
	}

	persisted, perr := LoadManualRecord(root, "BROKEN1")
	if perr != nil || persisted == nil {
		t.Fatalf("record must persist: %v %v", persisted, perr)
	}
	if persisted.Status != "failed" {
		t.Fatalf("status must be failed: %+v", persisted)
	}
	if persisted.NewAttachmentKey != "NEW1" {
		t.Fatalf("the orphan key must reach the record durably (guard basis), got %q", persisted.NewAttachmentKey)
	}
	found := false
	for _, s := range persisted.Steps {
		if s.Action == "create_attachment_orphan" {
			found = true
			if s.Detail["new_zotero_key"] != "NEW1" {
				t.Fatalf("orphan step must name the key: %+v", s.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("record must carry a create_attachment_orphan step: %+v", persisted.Steps)
	}
	// the failure step is named after the orphan step (audit order)
	last := persisted.Steps[len(persisted.Steps)-1].Action
	if last != "failed" {
		t.Fatalf("last step must be the failure record, got %s", last)
	}
}
