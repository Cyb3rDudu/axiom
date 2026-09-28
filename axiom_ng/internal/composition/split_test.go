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
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/bindings"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
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

	storeBackend := contractsuite.NewFakeStore(nil)
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
}
