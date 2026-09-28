// split_test.go — the F11 split-topology proofs at the composition level
// (#305): with the component URLs configured, the public routes answer
// through the HTTP bindings, and a killed backend surfaces the TYPED
// error class on the public surface with no topology detail in the
// body (the in-process twin of the three-process kill probe).
package composition

import (
	"bytes"
	"context"

	"encoding/json"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/bindings"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contractsuite"
)

// splitFixture is one fake component edge (backend) the api process
// binds to.
type splitFixture struct {
	srv    *httptest.Server
	logger *log.Logger
}

func TestSplitTopologyPublicSurfaceViaHTTPBindings(t *testing.T) {
	libBackend := contractsuite.NewFakeLibrary()
	libEdge := httptest.NewServer(bindings.LibraryInternalRoutes(libBackend, nil))
	defer libEdge.Close()

	// The public passage route validates chunk ids as UUIDs; the fake's
	// ids are chk-… — this alias presents one UUID-shaped chunk.
	uuidAlias := "2f0a4b1c-9d3e-4f5a-8b6c-7d8e9f0a1b2c"
	storeBackend := uuidAliasStore{inner: contractsuite.NewFakeStore(nil), alias: uuidAlias}
	storeEdge := httptest.NewServer(bindings.StoreInternalRoutes(storeBackend, nil, nil))

	cfg := config.Config{
		// api-only shape: no DB roles — the contract surfaces come from
		// the remote bindings, exactly the split api edge.
		LibraryURL: libEdge.URL,
		StoreURL:   storeEdge.URL,
		BindAddr:   "127.0.0.1",
		APIPort:    0, // ephemeral
	}
	root, err := Select(cfg, log.New(io.Discard, "", 0), Ports{}, RoleAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer root.Stop(context.Background())

	base := "http://" + root.Addr()

	// Library surface: a public import flies through the HTTP binding.
	importBody := func(key string) io.Reader {
		body, _ := json.Marshal(map[string]any{
			"idempotency_key": key,
			"record_type":     "book",
			"target":          map[string]any{"library_id": "users/0"},
			"metadata_hints":  map[string]any{"title": contractsuite.SeedBibliography.Title},
		})
		var mp bytes.Buffer
		mp.WriteString("--splitprobe\r\nContent-Disposition: form-data; name=\"request\"\r\n\r\n")
		mp.Write(body)
		mp.WriteString("\r\n--splitprobe\r\nContent-Disposition: form-data; name=\"file\"; filename=\"r.pdf\"\r\nContent-Type: application/pdf\r\n\r\n")
		mp.Write(contractsuite.SeedContent)
		mp.WriteString("\r\n--splitprobe--\r\n")
		return bytes.NewReader(mp.Bytes())
	}
	resp, err := http.Post(base+"/api/v1/library/imports", "multipart/form-data; boundary=splitprobe", importBody("split-1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		t.Fatalf("public import status %d: %s", resp.StatusCode, raw)
	}

	// Store surface: intake through the public route, then public search
	// answers through the HTTP binding with the public field names
	// (doc_id, not record_id).
	ireq, err := json.Marshal(map[string]any{
		"idempotency_key": "split-store-1",
		"revision":        contractsuite.SeedRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	aresp, err := http.Post(base+"/api/v1/store/ingest", "application/json", bytes.NewReader(ireq))
	if err != nil {
		t.Fatal(err)
	}
	araw, _ := io.ReadAll(aresp.Body)
	aresp.Body.Close()
	if aresp.StatusCode != http.StatusAccepted {
		t.Fatalf("public intake status %d: %s", aresp.StatusCode, araw)
	}

	sresp, err := http.Post(base+"/api/v1/search", "application/json",
		strings.NewReader(`{"query":"`+contractsuite.SeedToken+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	sraw, _ := io.ReadAll(sresp.Body)
	sresp.Body.Close()
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("public search status %d: %s", sresp.StatusCode, sraw)
	}
	if bytes.Contains(sraw, []byte(`"record_id"`)) {
		t.Fatalf("public search leaked the component-internal field name record_id: %s", sraw)
	}
	if !bytes.Contains(sraw, []byte(`"doc_id"`)) {
		t.Fatalf("public search lost the public field name doc_id: %s", sraw)
	}
	var sres struct {
		Hits []struct {
			ChunkID string `json:"chunk_id"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(sraw, &sres); err != nil {
		t.Fatal(err)
	}
	if len(sres.Hits) == 0 {
		t.Fatal("no hits to resolve a passage from")
	}

	// Store surface: public passage answers through the HTTP binding with
	// the public field names (attachment_id, not rendition_id) — the
	// split-mode passage surface witness (review round).
	presp, err := http.Get(base + "/api/v1/passage/" + sres.Hits[0].ChunkID)
	if err != nil {
		t.Fatal(err)
	}
	praw, _ := io.ReadAll(presp.Body)
	presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		t.Fatalf("public passage status %d: %s", presp.StatusCode, praw)
	}
	if bytes.Contains(praw, []byte(`"rendition_id"`)) {
		t.Fatalf("public passage leaked the component-internal field name rendition_id: %s", praw)
	}
	if !bytes.Contains(praw, []byte(`"attachment_id"`)) {
		t.Fatalf("public passage lost the public field name attachment_id: %s", praw)
	}

	// Public 400 mapping through the adapter: a blank query is
	// ErrBadRequest on the public route, not a 503 (review Minor-5).
	bresp, err := http.Post(base+"/api/v1/search", "application/json", strings.NewReader(`{"query":"   "}`))
	if err != nil {
		t.Fatal(err)
	}
	braw, _ := io.ReadAll(bresp.Body)
	bresp.Body.Close()
	if bresp.StatusCode != http.StatusBadRequest {
		t.Fatalf("blank query through the split surface: status %d, want 400 (body: %s)", bresp.StatusCode, braw)
	}

	// --- kill probe (in-process twin of the three-process sonde) ------
	// The Library surface answers with the TYPED envelope (its public
	// error shape is the contract one): killed backend → class
	// "unavailable" — the same class the local crash injection surfaces.
	libEdge.Close()

	var errBody struct {
		Error struct {
			Component string `json:"component"`
			Class     string `json:"class"`
			Message   string `json:"message"`
		} `json:"error"`
	}
	kresp, err := http.Post(base+"/api/v1/library/imports", "multipart/form-data; boundary=splitprobe", importBody("split-killed"))
	if err != nil {
		t.Fatal(err)
	}
	kraw, _ := io.ReadAll(kresp.Body)
	kresp.Body.Close()
	if kresp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("killed library backend: status %d, want 503 (body: %s)", kresp.StatusCode, kraw)
	}
	if err := json.Unmarshal(kraw, &errBody); err != nil {
		t.Fatalf("kill-probe body is not the typed envelope: %s (%v)", kraw, err)
	}
	if errBody.Error.Component != "library" || errBody.Error.Class != "unavailable" {
		t.Fatalf("kill-probe class = %s/%s, want library/unavailable — the same class the local crash injection surfaces", errBody.Error.Component, errBody.Error.Class)
	}
	// Leak sonde: the public answer must not name the component edge
	// (host, port, dial detail) — topology is not client data.
	for _, leak := range []string{"127.0.0.1", "localhost", "dial", "http://"} {
		if strings.Contains(string(kraw), leak) {
			t.Fatalf("public error body leaks topology detail %q: %s", leak, kraw)
		}
	}

	// The search surface's degradation shape is the F01-frozen legacy
	// body — identical bytes whether the failure is local (search stack
	// error) or split (killed store process): parity by identical
	// degradation, and leak-free the same way.
	storeEdge.Close()
	skresp, err := http.Post(base+"/api/v1/search", "application/json",
		strings.NewReader(`{"query":"`+contractsuite.SeedToken+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	skraw, _ := io.ReadAll(skresp.Body)
	skresp.Body.Close()
	if skresp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("killed store backend: status %d, want 503 (body: %s)", skresp.StatusCode, skraw)
	}
	if string(skraw) != "{\"error\":\"search unavailable\"}\n" {
		t.Fatalf("killed store backend: body %q — must be the F01-frozen degradation shape, identical to the local failure", skraw)
	}
	// The passage route degrades the same way (its F01-frozen shape).
	pkresp, err := http.Get(base + "/api/v1/passage/" + sres.Hits[0].ChunkID)
	if err != nil {
		t.Fatal(err)
	}
	pkraw, _ := io.ReadAll(pkresp.Body)
	pkresp.Body.Close()
	if pkresp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("killed store backend, passage: status %d, want 503 (body: %s)", pkresp.StatusCode, pkraw)
	}
	if string(pkraw) != "{\"error\":\"passage unavailable\"}\n" {
		t.Fatalf("killed store backend, passage: body %q — must be the frozen degradation shape", pkraw)
	}
}

// uuidAliasStore rewrites the first hit's chunk id to a UUID and answers
// GetPassage for it (the public passage route's isUUID gate).
type uuidAliasStore struct {
	inner store.Store
	alias string
}

func (u uuidAliasStore) IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error) {
	return u.inner.IngestRevision(ctx, req)
}

func (u uuidAliasStore) Search(ctx context.Context, req store.SearchRequest) (store.SearchResult, error) {
	res, err := u.inner.Search(ctx, req)
	if err != nil {
		return res, err
	}
	if len(res.Hits) > 0 {
		res.Hits[0].ChunkID = u.alias
	}
	return res, nil
}

func (u uuidAliasStore) GetPassage(ctx context.Context, ref store.PassageRef) (store.Passage, error) {
	inner, err := u.inner.Search(ctx, store.SearchRequest{Query: contractsuite.SeedToken})
	if err != nil || len(inner.Hits) == 0 {
		return store.Passage{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassNotFound, "no passage")
	}
	return u.inner.GetPassage(ctx, store.PassageRef{ChunkID: inner.Hits[0].ChunkID})
}

// TestInternalEdgeLifecycleGuards — the loud-failure contract of the
// internal edges (review round): an occupied address fails the component
// start loudly, and a requested library edge without a wired Library
// service is refused as the misconfig it is (never a nil-deref per
// request).
func TestInternalEdgeLifecycleGuards(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	logger := log.New(io.Discard, "", 0)
	r := &Root{logger: logger}
	var edge internalEdge

	// Bind collision: loud start failure.
	if err := r.serveInternalEdge(occupied.Addr().String(), http.NotFoundHandler(), "probe", &edge); err == nil {
		t.Fatal("edge bind over an occupied address succeeded — must be a loud start failure")
		closeInternalEdge(&edge, logger)
	}

	// Nil-service guard: the library edge without a Library service is
	// refused (the store-role start's guard, exercised through Select).
	cfg := config.Config{
		DatabaseURL:            "probe-unset", // never dialed: Select fails earlier
		InternalLibraryAddr:    occupied.Addr().String(),
		LibraryImportProviders: "", // no Library service wired
		BindAddr:               "127.0.0.1",
		APIPort:                0,
	}
	if _, err := Select(cfg, logger, Ports{}, RoleStore, RoleSync); err == nil {
		t.Fatal("library edge over a nil service passed Select — the misconfig guard did not fire")
	} else if !strings.Contains(err.Error(), "no Library service to serve it") {
		t.Fatalf("guard failed with the wrong diagnosis: %v", err)
	}
}
