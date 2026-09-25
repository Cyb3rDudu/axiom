// provider_test.go — the adapter battery against a fake Zotero local API
// (F07, #301): port behavior, idempotency anchors, versioned writes, the
// typed 412 mapping, readback, and the 1:1 mutation↔audit-row contract.
// The live Zotero run is the env-gated IT (zotero_it_test.go).
package zoteroprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
)

// ---------------------------------------------------------------------------
// stub ProviderStore (anchor + audit capture; lease semantics asserted in
// the library package's ITs against real Postgres)

type stubStore struct {
	mu         sync.Mutex
	anchors    map[string]string // kind|anchor -> provider id
	audit      []library.WriteAuditRow
	leaseOwner string
	refuseNew  bool // refuse a second acquirer
	renewFails bool
	released   bool
}

func newStubStore() *stubStore { return &stubStore{anchors: map[string]string{}} }

func (s *stubStore) AcquireWriterLease(_ context.Context, scope, owner string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leaseOwner == "" {
		s.leaseOwner = owner
		return nil
	}
	if s.refuseNew || s.leaseOwner != owner {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassConflict,
			&library.WriterLeaseConflict{Scope: scope, Owner: s.leaseOwner, HeartbeatAge: time.Second},
			"single-writer guard (stub)")
	}
	return nil
}

func (s *stubStore) RenewWriterLease(_ context.Context, _, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renewFails || s.leaseOwner != owner {
		return errors.New("lease lost")
	}
	return nil
}

func (s *stubStore) ReleaseWriterLease(_ context.Context, _, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leaseOwner == owner {
		s.released = true
	}
	return nil
}

func (s *stubStore) AppendWriteAudit(_ context.Context, r library.WriteAuditRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, r)
	return nil
}

func (s *stubStore) LookupProviderAnchor(_ context.Context, _, kind, anchor string) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.anchors[kind+"|"+anchor]; ok {
		return id, 1, nil
	}
	return "", 0, errAnchorAbsent
}

func (s *stubStore) PutProviderAnchor(_ context.Context, _, kind, anchor, providerID string, _ int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.anchors[kind+"|"+anchor]; ok {
		return cur, nil
	}
	s.anchors[kind+"|"+anchor] = providerID
	return providerID, nil
}

func (s *stubStore) EvictProviderAnchor(_ context.Context, _, kind, anchor, providerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.anchors[kind+"|"+anchor]; ok && cur == providerID {
		delete(s.anchors, kind+"|"+anchor)
	}
	return nil
}

func (s *stubStore) audits() []library.WriteAuditRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]library.WriteAuditRow(nil), s.audit...)
}

// ---------------------------------------------------------------------------
// fake Zotero local API

type zotCollection struct {
	Key      string
	Name     string
	Parent   string
	Version  int64
	ParentJS json.RawMessage // raw parentCollection for readback fidelity
}

type fakeLibrary struct {
	mu              sync.Mutex
	t               *testing.T
	srv             *httptest.Server
	storageDir      string
	items           map[string]*zotItem
	rawItems        map[string]json.RawMessage // wire view: unknown fields preserved across PUTs
	itemOrder       []string
	collections     map[string]*zotCollection
	files           map[string][]byte
	pending         map[string][]byte // uploadKey -> bytes
	nextKey         int
	puts            int
	putVersions     []string // If-Unmodified-Since-Version values seen
	force412        bool     // one-shot: the next versioned PUT gets a 412
	ignoreTagFilter bool     // the ?tag= filter is ignored (server misbehavior sonde)
	nUploads        int
	deleted         []string
}

func newFakeLibrary(t *testing.T) *fakeLibrary {
	t.Helper()
	f := &fakeLibrary{
		t:           t,
		storageDir:  t.TempDir(),
		items:       map[string]*zotItem{},
		rawItems:    map[string]json.RawMessage{},
		collections: map[string]*zotCollection{},
		files:       map[string][]byte{},
		pending:     map[string][]byte{},
		nextKey:     1,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Zotero-Server-ID", "fake-zotero-lib")
		f.route(w, r)
	}))
	f.srv = srv
	t.Cleanup(srv.Close)
	return f
}

func (f *fakeLibrary) key() string {
	f.nextKey++
	return fmt.Sprintf("FAKE%04d", f.nextKey)
}

func (f *fakeLibrary) addItem(it zotItem) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := it.Key
	if k == "" {
		k = f.key()
		it.Key = k
	}
	it.Version = 1
	cp := it
	f.items[k] = &cp
	f.rawItems[k] = mustJSON(cp)
	f.itemOrder = append(f.itemOrder, k)
	return k
}

func (f *fakeLibrary) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	// --- health / server id (the probe hits "/api" or "/api/")
	case (path == "/api" || path == "/api/") && r.Method == http.MethodGet:
		w.WriteHeader(http.StatusOK)

	// --- items listing (paginated, optional tag filter)
	case path == "/api/users/0/items" && r.Method == http.MethodGet:
		limit := intVal(r.URL.Query().Get("limit"), 100)
		start := intVal(r.URL.Query().Get("start"), 0)
		tag := r.URL.Query().Get("tag")
		var out []map[string]any
		for _, k := range f.itemOrder {
			it := f.items[k]
			if tag != "" && !f.ignoreTagFilter && !itemHasTag(it, tag) {
				continue
			}
			if start > 0 {
				start--
				continue
			}
			out = append(out, f.envelope(it))
			if len(out) >= limit {
				break
			}
		}
		writeJSON(w, out)

	// --- single item
	case strings.HasPrefix(path, "/api/users/0/items/") && r.Method == http.MethodGet && !strings.HasSuffix(path, "/file"):
		k := strings.TrimPrefix(path, "/api/users/0/items/")
		it, ok := f.items[k]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Last-Modified-Version", strconv.FormatInt(it.Version, 10))
		writeJSON(w, f.envelope(it))

	// --- item update (versioned)
	case strings.HasPrefix(path, "/api/users/0/items/") && r.Method == http.MethodPut:
		k := strings.TrimPrefix(path, "/api/users/0/items/")
		it, ok := f.items[k]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		guard := r.Header.Get("If-Unmodified-Since-Version")
		f.puts++
		f.putVersions = append(f.putVersions, guard)
		if f.force412 || guard != strconv.FormatInt(it.Version, 10) {
			f.force412 = false
			w.WriteHeader(http.StatusPreconditionFailed)
			w.Write([]byte(`{"failed":{}}`))
			return
		}
		body, berr := io.ReadAll(r.Body)
		if berr != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var next zotItem
		if err := json.Unmarshal(body, &next); err != nil {
			http.Error(w, "bad item", http.StatusBadRequest)
			return
		}
		next.Key, next.Version = k, it.Version+1
		f.items[k] = &next
		// The wire view keeps UNKNOWN fields the server never modeled —
		// the PUT body is stored verbatim (replace semantics on both views).
		f.rawItems[k] = json.RawMessage(body)
		w.Header().Set("Last-Modified-Version", strconv.FormatInt(next.Version, 10))
		w.WriteHeader(http.StatusNoContent)

	// --- item create
	case path == "/api/users/0/items" && r.Method == http.MethodPost:
		var batch []zotItem
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil || len(batch) == 0 {
			http.Error(w, "bad batch", http.StatusBadRequest)
			return
		}
		k := f.addItemRaw(batch[0])
		w.Write([]byte(fmt.Sprintf(`{"successful":{"0":{"key":%q,"data":%s}},"unchanged":{},"failed":{}}`,
			k, mustJSON(f.items[k]))))

	// --- item delete (versioned)
	case strings.HasPrefix(path, "/api/users/0/items/") && r.Method == http.MethodDelete:
		k := strings.TrimPrefix(path, "/api/users/0/items/")
		it, ok := f.items[k]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("If-Unmodified-Since-Version") != strconv.FormatInt(it.Version, 10) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		delete(f.items, k)
		delete(f.rawItems, k)
		f.removeFromOrder(k)
		f.deleted = append(f.deleted, k)
		w.WriteHeader(http.StatusNoContent)

	// --- file flow: authorize (form) / register (upload=)
	case strings.HasSuffix(path, "/file") && r.Method == http.MethodPost:
		k := strings.TrimSuffix(strings.TrimPrefix(path, "/api/users/0/items/"), "/file")
		if _, ok := f.items[k]; !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		if strings.HasPrefix(body, "upload=") {
			upKey := strings.TrimPrefix(body, "upload=")
			f.files[k] = f.pending[upKey]
			_ = os.WriteFile(filepath.Join(f.storageDir, k), f.pending[upKey], 0o644)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// authorize
		f.nUploads++
		upKey := fmt.Sprintf("UP%d", f.nUploads)
		w.Write([]byte(fmt.Sprintf(`{"exists":0,"url":%q,"uploadKey":%q}`, f.srv.URL+"/upload/"+upKey, upKey)))

	// --- file download (readback)
	case strings.HasSuffix(path, "/file") && r.Method == http.MethodGet:
		k := strings.TrimSuffix(strings.TrimPrefix(path, "/api/users/0/items/"), "/file")
		content, ok := f.files[k]
		if !ok {
			http.Error(w, "no file", http.StatusNotFound)
			return
		}
		w.Write(content)

	// --- upload target (phase 2)
	case strings.HasPrefix(path, "/upload/") && r.Method == http.MethodPost:
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				http.Error(w, "multipart", http.StatusBadRequest)
				return
			}
			if part.FormName() == "file" {
				b, _ := io.ReadAll(part)
				f.pending[strings.TrimPrefix(path, "/upload/")] = b
			}
		}
		w.WriteHeader(http.StatusCreated)

	// --- collections listing
	case path == "/api/users/0/collections" && r.Method == http.MethodGet:
		var out []map[string]any
		for _, c := range f.collections {
			out = append(out, map[string]any{
				"key": c.Key, "version": c.Version,
				"data": map[string]any{"key": c.Key, "name": c.Name, "parentCollection": rawParent(c)},
			})
		}
		writeJSON(w, out)

	// --- collection create
	case path == "/api/users/0/collections" && r.Method == http.MethodPost:
		var batch []struct {
			Name             string          `json:"name"`
			ParentCollection json.RawMessage `json:"parentCollection"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil || len(batch) == 0 {
			http.Error(w, "bad collection", http.StatusBadRequest)
			return
		}
		k := f.key()
		parent := ""
		if len(batch[0].ParentCollection) > 0 && batch[0].ParentCollection[0] == '"' {
			_ = json.Unmarshal(batch[0].ParentCollection, &parent)
		}
		f.collections[k] = &zotCollection{Key: k, Name: batch[0].Name, Parent: parent, Version: 1, ParentJS: batch[0].ParentCollection}
		w.Write([]byte(fmt.Sprintf(`{"successful":{"0":{"key":%q}}}`, k)))

	// --- single collection
	case strings.HasPrefix(path, "/api/users/0/collections/") && r.Method == http.MethodGet:
		k := strings.TrimPrefix(path, "/api/users/0/collections/")
		c, ok := f.collections[k]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"key": c.Key, "version": c.Version,
			"data": map[string]any{"key": c.Key, "name": c.Name, "parentCollection": rawParent(c)}})

	default:
		http.Error(w, "fake: unsupported "+r.Method+" "+path, http.StatusNotImplemented)
		f.t.Errorf("fake zotero: unsupported %s %s", r.Method, path)
	}
}

// deleteItem simulates an EXTERNAL deletion (item gone from every view).
func (f *fakeLibrary) deleteItem(k string) {
	delete(f.items, k)
	delete(f.rawItems, k)
	delete(f.files, k)
	f.removeFromOrder(k)
	f.deleted = append(f.deleted, k)
}

func (f *fakeLibrary) removeFromOrder(k string) {
	for i, cur := range f.itemOrder {
		if cur == k {
			f.itemOrder = append(f.itemOrder[:i], f.itemOrder[i+1:]...)
			return
		}
	}
}

func (f *fakeLibrary) addItemRaw(it zotItem) string {
	k := f.key()
	it.Key = k
	it.Version = 1
	cp := it
	f.items[k] = &cp
	f.rawItems[k] = mustJSON(cp)
	f.itemOrder = append(f.itemOrder, k)
	return k
}

func rawParent(c *zotCollection) any {
	if len(c.ParentJS) > 0 {
		return json.RawMessage(c.ParentJS)
	}
	return false
}

func itemHasTag(it *zotItem, tag string) bool {
	for _, t := range it.Tags {
		if t.Tag == tag {
			return true
		}
	}
	return false
}

// envelope renders the full item envelope. Attachments carry the local-API
// enclosure shape: an href pointing at the fake's REAL storage file (the
// file readback dereferences it — the /file GET redirects there in the
// local API).
func (f *fakeLibrary) envelope(it *zotItem) map[string]any {
	href := ""
	if it.ItemType == "attachment" {
		href = "file://" + filepath.Join(f.storageDir, it.Key)
	}
	data := any(it)
	if raw, ok := f.rawItems[it.Key]; ok && len(raw) > 0 {
		data = json.RawMessage(raw) // verbatim: unknown fields included
	}
	return map[string]any{"key": it.Key, "version": it.Version, "data": data,
		"links": map[string]any{"enclosure": map[string]any{"href": href}}}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func intVal(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// ---------------------------------------------------------------------------
// harness

type harness struct {
	fake  *fakeLibrary
	store *stubStore
	prov  *Provider
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeLibrary(t)
	store := newStubStore()
	prov, err := New(context.Background(), Options{
		BaseURL: fake.srv.URL + "/api", LibraryID: "users/0",
		APIKey: "test-key", Store: store, Owner: "test-owner",
	})
	if err != nil {
		t.Fatalf("provider New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close() })
	return &harness{fake: fake, store: store, prov: prov}
}

func staged(t *testing.T, content []byte) (path, sha string) {
	t.Helper()
	dir := t.TempDir()
	sum := sha256hex(content)
	p := filepath.Join(dir, sum)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, sum
}

// ---------------------------------------------------------------------------
// the battery

func TestCatalogFullPaginationAndMembershipView(t *testing.T) {
	h := newHarness(t)
	// 250 documents — the catalog walk must follow every page.
	for i := 0; i < 250; i++ {
		h.fake.addItem(zotItem{ItemType: "book", Title: fmt.Sprintf("Book %d", i)})
	}
	col := &zotCollection{Key: "COLL1", Name: "Shelf", Version: 1}
	h.fake.collections["COLL1"] = col
	member := h.fake.addItem(zotItem{ItemType: "book", Title: "Member", Collections: []string{"COLL1"}})

	seen := 0
	var memberRec *library.CatalogRecord
	token := ""
	for {
		page, err := h.prov.ListRecords(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		for i := range page.Records {
			seen++
			if page.Records[i].ProviderRecordID == member {
				cp := page.Records[i]
				memberRec = &cp
			}
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	if seen != 251 {
		t.Fatalf("catalog walk saw %d records, want 251", seen)
	}
	if memberRec == nil || len(memberRec.Collections) != 1 || memberRec.Collections[0] != "COLL1" {
		t.Fatalf("membership not observable in the catalog: %+v", memberRec)
	}
	if memberRec.Year != nil {
		t.Fatalf("no year set, got %+v", memberRec)
	}
}

func TestEnsureRecordCreateReadbackAuditAndIdempotency(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	y := 2021
	draft := library.RecordDraft{
		ExternalKey: "imp-key-1", RecordType: "book", Title: "The Verified Work",
		Authors: []library.Creator{{FirstName: "Ada", LastName: "Example", CreatorType: "author"}},
		Year:    &y, Publisher: "Press", Language: "en",
		DOI: "10.5555/ABC", ISBN: "9783161484100",
		URL: "https://example.org/work", AccessDate: "2026-01-02T10:00:00Z",
	}
	k1, err := h.prov.EnsureRecord(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == "" {
		t.Fatal("empty provider id")
	}
	// What Zotero persisted: item type, title, DOI/ISBN, web provenance.
	it := h.fake.items[k1]
	if it.ItemType != "book" || it.Title != "The Verified Work" || it.DOI != "10.5555/abc" {
		t.Fatalf("persisted item diverges: %+v", it)
	}
	if it.URL != draft.URL || it.AccessDate != draft.AccessDate {
		t.Fatalf("web provenance not persisted: %+v", it)
	}
	if !itemHasTag(it, "axiom-imp:imp-key-1") {
		t.Fatalf("anchor tag missing: %+v", it.Tags)
	}

	// Idempotent second ensure: same id, audit says reused, still ONE item.
	k2, err := h.prov.EnsureRecord(ctx, draft)
	if err != nil || k2 != k1 {
		t.Fatalf("re-ensure: %q %v (want %q)", k2, err, k1)
	}
	if n := len(h.fake.items); n != 1 {
		t.Fatalf("re-ensure created a second item: %d items", n)
	}
	rows := h.store.audits()
	if len(rows) != 2 || rows[0].Outcome != "created" || rows[1].Outcome != "reused" {
		t.Fatalf("audit trail = %v", rows)
	}

	// Diverged title on the anchored record → versioned update + readback.
	draft.Title = "The Verified Work (2nd ed.)"
	k3, err := h.prov.EnsureRecord(ctx, draft)
	if err != nil || k3 != k1 {
		t.Fatalf("update ensure: %q %v", k3, err)
	}
	if h.fake.items[k1].Title != draft.Title {
		t.Fatalf("title update not persisted: %+v", h.fake.items[k1])
	}
	if h.fake.puts != 1 {
		t.Fatalf("update must be exactly one versioned PUT, saw %d", h.fake.puts)
	}
	if vs := h.fake.putVersions[0]; vs != "1" {
		t.Fatalf("PUT must carry the observed version guard, saw %q", vs)
	}
	rows = h.store.audits()
	if len(rows) != 3 || rows[2].Outcome != "changed" {
		t.Fatalf("audit trail after update = %v", rows)
	}
}

func TestEnsureRecordCrashRecoveryByTagSearch(t *testing.T) {
	h := newHarness(t)
	// The crash window: the Zotero write landed, the anchor ledger did not.
	h.fake.addItem(zotItem{
		ItemType: "book", Title: "The Lost ACK",
		Tags: []zotTag{{Tag: "axiom-imp:imp-crash-1"}},
	})
	k, err := h.prov.EnsureRecord(context.Background(), library.RecordDraft{
		ExternalKey: "imp-crash-1", RecordType: "book", Title: "The Lost ACK",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(h.fake.items); n != 1 {
		t.Fatalf("tag recovery must not create a duplicate: %d items", n)
	}
	rows := h.store.audits()
	if len(rows) != 1 || rows[0].Outcome != "reused" {
		t.Fatalf("audit after tag recovery = %v", rows)
	}
	if id, _ := h.store.anchors["record|imp-crash-1"]; id != k {
		t.Fatalf("anchor not healed: %q vs %q", id, k)
	}
}

func TestMembershipVersionConflictIsTypedRetryable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.fake.addItem(zotItem{ItemType: "book", Title: "Contested"})
	h.fake.collections["COLL1"] = &zotCollection{Key: "COLL1", Name: "Shelf", Version: 1}

	h.fake.mu.Lock()
	h.fake.force412 = true // a concurrent writer wins the version race
	h.fake.mu.Unlock()

	err := h.prov.EnsureMembership(ctx, rec, "COLL1")
	var vc *VersionConflictError
	if !errors.As(err, &vc) {
		t.Fatalf("412 must surface as *VersionConflictError, got %v", err)
	}
	class, ok := contracterr.ClassOf(err)
	if !ok || class != contracterr.ClassUnavailable || !class.Retryable() {
		t.Fatalf("version conflict must be retryable (unavailable class), got class=%v retryable=%v", class, err)
	}
	// Readback proof: the concurrent writer's value stands untouched —
	// no membership of ours landed.
	if got := h.fake.items[rec].Collections; len(got) != 0 {
		t.Fatalf("nothing of ours may land on a 412, collections=%v", got)
	}
	// Retry against the fresh state succeeds (the guard read re-fetches).
	h.fake.mu.Lock()
	h.fake.items[rec].Version = 99 // the concurrent writer's bump
	h.fake.mu.Unlock()
	if err := h.prov.EnsureMembership(ctx, rec, "COLL1"); err != nil {
		t.Fatalf("retry after conflict: %v", err)
	}
	if len(h.fake.items[rec].Collections) != 1 || h.fake.items[rec].Collections[0] != "COLL1" {
		t.Fatalf("membership not applied after retry: %+v", h.fake.items[rec].Collections)
	}
}

func TestEnsureRenditionThreePhaseUploadIdempotencyAudit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.fake.addItem(zotItem{ItemType: "book", Title: "Carrier"})
	content := []byte("%PDF-1.4 fake but stable")
	path, sha := staged(t, content)

	a1, err := h.prov.EnsureRendition(ctx, library.RenditionDraft{
		ParentProviderID: rec, ContentHash: sha, MediaType: "application/pdf",
		Filename: "Example - 2021 - The Work.pdf", StagingPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Readback already proved retrievability; assert the stored state.
	if got := h.fake.files[a1]; string(got) != string(content) {
		t.Fatalf("file bytes not stored: %q", got)
	}
	att := h.fake.items[a1]
	if att.ParentItem != rec || att.Filename != "Example - 2021 - The Work.pdf" {
		t.Fatalf("attachment item wrong: %+v", att)
	}
	if !itemHasTag(att, "axiom-sha256:"+sha) {
		t.Fatalf("sha tag missing: %+v", att.Tags)
	}

	// Hash mismatch refusal: the staged file must match its declared hash.
	if _, err := h.prov.EnsureRendition(ctx, library.RenditionDraft{
		ParentProviderID: rec, ContentHash: strings.Repeat("0", 64), MediaType: "application/pdf",
		Filename: "x.pdf", StagingPath: path,
	}); !contractClassIs(err, contracterr.ClassConflict) {
		t.Fatalf("declared-hash mismatch must be a conflict, got %v", err)
	}

	// Idempotent re-ensure: anchor hit, no second upload.
	nUp := h.fake.nUploads
	a2, err := h.prov.EnsureRendition(ctx, library.RenditionDraft{
		ParentProviderID: rec, ContentHash: sha, MediaType: "application/pdf",
		Filename: "Example - 2021 - The Work.pdf", StagingPath: path,
	})
	if err != nil || a2 != a1 {
		t.Fatalf("re-ensure: %q %v (want %q)", a2, err, a1)
	}
	if h.fake.nUploads != nUp {
		t.Fatalf("idempotent re-ensure must not upload again (%d → %d)", nUp, h.fake.nUploads)
	}
	rows := h.store.audits()
	if len(rows) != 2 || rows[0].Outcome != "created" || rows[1].Outcome != "reused" {
		t.Fatalf("rendition audit trail = %v", rows)
	}

	// The catalog exposes the rendition with OUR content hash (the sha tag).
	h.prov.invalidate()
	token := ""
	found := false
	for {
		page, err := h.prov.ListRecords(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Records {
			if r.ProviderRecordID != rec {
				continue
			}
			for _, rd := range r.Renditions {
				if rd.ProviderAttachmentID == a1 {
					found = rd.ContentHash == sha
				}
			}
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	if !found {
		t.Fatal("catalog does not expose the imported rendition with its sha256")
	}
}

func TestResolvePathParentFirstConflictsAndCreation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Absent + createMissing=false → NotFound (a typo never grows a tree).
	if _, err := h.prov.ResolvePath(ctx, []string{"Axiom", "Missing"}, false); !contractClassIs(err, contracterr.ClassNotFound) {
		t.Fatalf("absent segment: %v, want not_found", err)
	}

	// Parent-first creation with readback.
	id, err := h.prov.ResolvePath(ctx, []string{"Axiom", "F07", "Deep"}, true)
	if err != nil {
		t.Fatal(err)
	}
	c := h.fake.collections[id]
	if c.Name != "Deep" || c.Parent == "" {
		t.Fatalf("deepest collection wrong: %+v", c)
	}
	grandparent := h.fake.collections[c.Parent]
	if grandparent.Name != "F07" || grandparent.Parent == "" {
		t.Fatalf("middle collection wrong: %+v", grandparent)
	}
	if h.fake.collections[grandparent.Parent].Name != "Axiom" {
		t.Fatalf("root collection wrong: %+v", h.fake.collections[grandparent.Parent])
	}
	// Resolution is stable (idempotent over the existing tree).
	id2, err := h.prov.ResolvePath(ctx, []string{"Axiom", "F07", "Deep"}, true)
	if err != nil || id2 != id {
		t.Fatalf("re-resolve: %q %v (want %q)", id2, err, id)
	}

	// Same-named siblings under one parent → conflict, never a pick.
	h.fake.collections["SIB1"] = &zotCollection{Key: "SIB1", Name: "Twins", Version: 1}
	h.fake.collections["SIB2"] = &zotCollection{Key: "SIB2", Name: "Twins", Version: 1}
	_, err = h.prov.ResolvePath(ctx, []string{"Twins"}, false)
	if !errors.Is(err, library.ErrProviderConflict) {
		t.Fatalf("same-named siblings must be a provider conflict, got %v", err)
	}
}

func TestReadOnlyProviderAndLeaseRefusal(t *testing.T) {
	fake := newFakeLibrary(t)

	// Read-only construction (no API key): no lease, write ports honest.
	store := newStubStore()
	ro, err := New(context.Background(), Options{
		BaseURL: fake.srv.URL + "/api", Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.EnsureRecord(context.Background(), library.RecordDraft{
		ExternalKey: "k", RecordType: "book", Title: "T",
	}); !contractClassIs(err, contracterr.ClassUnavailable) {
		t.Fatalf("read-only EnsureRecord: %v, want unavailable", err)
	}
	if _, err := ro.ResolvePath(context.Background(), []string{"A"}, true); !contractClassIs(err, contracterr.ClassUnavailable) {
		t.Fatalf("read-only ResolvePath: %v, want unavailable", err)
	}
	if store.leaseOwner != "" {
		t.Fatal("read-only construction must not take a lease")
	}

	// Second live writer: refused AT START (the single-writer declaration).
	one, err := New(context.Background(), Options{
		BaseURL: fake.srv.URL + "/api", APIKey: "k", Store: store, Owner: "writer-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	store.mu.Lock()
	store.refuseNew = true
	store.mu.Unlock()
	if _, err := New(context.Background(), Options{
		BaseURL: fake.srv.URL + "/api", APIKey: "k", Store: store, Owner: "writer-2",
	}); !contractClassIs(err, contracterr.ClassConflict) {
		t.Fatalf("second writer at start: %v, want conflict", err)
	}

	// Graceful release frees the scope.
	if err := one.Close(); err != nil {
		t.Fatal(err)
	}
	if !store.released {
		t.Fatal("Close must release the lease")
	}
}

func contractClassIs(err error, class contracterr.Class) bool {
	got, ok := contracterr.ClassOf(err)
	return ok && got == class
}

// --- auto-review regression battery (#301 review pass) ---

// C1: a page token carries the snapshot generation — an invalidation
// (own mutation) between pages must fail the walk LOUDLY, never silently
// serve page 2 from a different snapshot (a dedup scan would skip records).
func TestCatalogWalkFailsLoudlyWhenSnapshotChanges(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 120; i++ { // > one catalog page
		h.fake.addItem(zotItem{ItemType: "book", Title: fmt.Sprintf("B%d", i)})
	}
	ctx := context.Background()
	p1, err := h.prov.ListRecords(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if p1.NextPageToken == "" {
		t.Fatal("fixture must produce a second page")
	}
	h.prov.invalidate() // an own mutation lands between the pages
	_, err = h.prov.ListRecords(ctx, p1.NextPageToken)
	if !contractClassIs(err, contracterr.ClassConflict) {
		t.Fatalf("stale-generation token must fail loudly (conflict), got %v", err)
	}
	if !strings.Contains(fmt.Sprint(err), "restart the scan") {
		t.Fatalf("diagnosis must tell the caller to restart: %v", err)
	}
	// A garbage token stays an InvalidArgument, not a conflict.
	if _, err := h.prov.ListRecords(ctx, "nonsense"); !contractClassIs(err, contracterr.ClassInvalidArgument) {
		t.Fatalf("bad token shape = %v, want invalid_argument", err)
	}
	// Restarting the walk from scratch works against the fresh snapshot.
	p2, err := h.prov.ListRecords(ctx, "")
	if err != nil || len(p2.Records) != 100 {
		t.Fatalf("restart: %d records, %v", len(p2.Records), err)
	}
}

// W1: a lost lease (renewal failed) must STOP the writer — write ports
// refuse with Conflict, not just stop renewing.
func TestLostLeaseRefusesWrites(t *testing.T) {
	fake := newFakeLibrary(t)
	store := newStubStore()
	prov, err := New(context.Background(), Options{
		BaseURL: fake.srv.URL + "/api", APIKey: "k", Store: store, Owner: "w",
		LeaseTTL: 120 * time.Millisecond, // heartbeat ticker fires at ttl/3
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	store.mu.Lock()
	store.renewFails = true
	store.mu.Unlock()
	ctx := context.Background()
	draft := library.RecordDraft{ExternalKey: "ll-1", RecordType: "book", Title: "T"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err = prov.EnsureRecord(ctx, draft)
		if contractClassIs(err, contracterr.ClassConflict) {
			break // refusal observed
		}
		if err != nil {
			t.Fatalf("pre-loss ensure must succeed (anchor reuse), got %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("lease-lost refusal never fired within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(fmt.Sprint(err), "lease lost") {
		t.Fatalf("refusal must name the lost lease: %v", err)
	}
}

// W2: a rendition anchor whose attachment VANISHED (deleted externally)
// must heal — evict the stale row, re-upload fresh, return the LIVE id;
// never return the dead anchor id with an orphaned fresh upload.
func TestEnsureRenditionHealsVanishedAnchor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.fake.addItem(zotItem{ItemType: "book", Title: "Carrier"})
	content := []byte("%PDF-1.4 heal me")
	path, sha := staged(t, content)
	draft := library.RenditionDraft{
		ParentProviderID: rec, ContentHash: sha, MediaType: "application/pdf",
		Filename: "Heal - 2021 - Me.pdf", StagingPath: path,
	}
	a1, err := h.prov.EnsureRendition(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	// External deletion: the attachment item is gone, the ledger row stays.
	h.fake.mu.Lock()
	h.fake.deleteItem(a1)
	h.fake.mu.Unlock()

	nUp := h.fake.nUploads
	a2, err := h.prov.EnsureRendition(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	if a2 == a1 {
		t.Fatal("must return the fresh live id, not the dead anchor id")
	}
	if h.fake.nUploads != nUp+1 {
		t.Fatalf("healing must re-upload exactly once (%d → %d)", nUp, h.fake.nUploads)
	}
	if got := h.fake.files[a2]; string(got) != string(content) {
		t.Fatalf("fresh attachment must carry the staged bytes: %q", got)
	}
	if id, _ := h.store.anchors["rendition|"+rec+"|"+sha]; id != a2 {
		t.Fatalf("anchor must be healed to the fresh id: %q vs %q", id, a2)
	}
	rows := h.store.audits()
	last := rows[len(rows)-1]
	if last.Operation != "ensure_rendition" || last.Outcome != "created" {
		t.Fatalf("healed ensure must audit as created, got %+v", last)
	}
	// And the healed anchor is the fast path again.
	nUp = h.fake.nUploads
	a3, err := h.prov.EnsureRendition(ctx, draft)
	if err != nil || a3 != a2 || h.fake.nUploads != nUp {
		t.Fatalf("post-heal idempotency broken: %q %v", a3, err)
	}
}

// W3: tag verification is EXACT. A server that ignores the ?tag= filter
// and returns a decoy carrying a DIFFERENT axiom-imp tag must not be
// adopted — the anchor tag is the identity, a prefix is not.
func TestTagVerificationRejectsForeignAnchorTags(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	decoy := h.fake.addItem(zotItem{
		ItemType: "book", Title: "Someone Else's Import",
		Tags: []zotTag{{Tag: "axiom-imp:someone-elses-key"}},
	})
	h.fake.mu.Lock()
	h.fake.ignoreTagFilter = true // the server ignores the tag filter
	h.fake.mu.Unlock()

	k, err := h.prov.EnsureRecord(ctx, library.RecordDraft{
		ExternalKey: "imp-mine", RecordType: "book", Title: "Mine",
	})
	if err != nil {
		t.Fatal(err)
	}
	if k == decoy {
		t.Fatal("must not adopt a decoy whose axiom-imp tag names a different anchor")
	}
	if !itemHasTag(h.fake.items[k], "axiom-imp:imp-mine") {
		t.Fatalf("fresh item must carry OUR anchor tag: %+v", h.fake.items[k].Tags)
	}
	if n := len(h.fake.items); n != 2 {
		t.Fatalf("fixture drift: %d items", n)
	}
}

// W5: versioned PUTs overlay the RAW item data — unmodeled fields
// (abstractNote, extra, …) survive both the membership update and the
// diverged-record update. A typed struct round-trip would strip them.
func TestVersionedWritesPreserveUnmodeledFields(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.fake.collections["COLL1"] = &zotCollection{Key: "COLL1", Name: "Shelf", Version: 1}

	// Membership path.
	rec := h.fake.addItem(zotItem{ItemType: "book", Title: "Keep Fields"})
	injectExtra(t, h.fake, rec, "membership")
	if err := h.prov.EnsureMembership(ctx, rec, "COLL1"); err != nil {
		t.Fatal(err)
	}
	if v := extraOf(t, h.fake, rec); v != "membership" {
		t.Fatalf("membership PUT stripped the unmodeled field: %q", v)
	}
	if len(h.fake.items[rec].Collections) != 1 || h.fake.items[rec].Collections[0] != "COLL1" {
		t.Fatalf("membership not applied: %+v", h.fake.items[rec].Collections)
	}

	// Diverged-record update path (anchor hit + versioned update).
	k2, err := h.prov.EnsureRecord(ctx, library.RecordDraft{
		ExternalKey: "imp-keep", RecordType: "book", Title: "Keep Fields",
	})
	if err != nil || k2 == "" {
		t.Fatal(err)
	}
	injectExtra(t, h.fake, k2, "record-update")
	if _, err := h.prov.EnsureRecord(ctx, library.RecordDraft{
		ExternalKey: "imp-keep", RecordType: "book", Title: "Keep Fields (3rd)",
	}); err != nil {
		t.Fatal(err)
	}
	if v := extraOf(t, h.fake, k2); v != "record-update" {
		t.Fatalf("record update PUT stripped the unmodeled field: %q", v)
	}
}

// injectExtra adds an unmodeled field to an item's wire view (what a
// richer Zotero item would carry and the adapter does not model).
func injectExtra(t *testing.T, f *fakeLibrary, key, val string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(f.rawItems[key], &m); err != nil {
		t.Fatalf("raw view undecodable: %v", err)
	}
	m["abstractNote"] = val
	f.rawItems[key] = mustJSON(m)
}

func extraOf(t *testing.T, f *fakeLibrary, key string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(f.rawItems[key], &m); err != nil {
		t.Fatalf("raw view undecodable: %v", err)
	}
	s, _ := m["abstractNote"].(string)
	return s
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
