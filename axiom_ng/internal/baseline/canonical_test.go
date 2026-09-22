// canonical_test.go — working-tree canonical identity golden (#296, ADR
// 0001 §5). Unlike the live probes (which witness the frozen v0.1.18
// release bits and can never move), this golden witnesses the CURRENT
// working tree: it derives the identity block from the real /api/health
// handler (same buildFullServer as the inventory walk) and compares it
// against fixtures/canonical_identity.json.
//
// Fields taken up by this baseline extension (exactly these, no general
// "allow additional fields" relaxation anywhere): canonical_name,
// service_class, component_roles (RAG side) — the deprecations counter
// map is pinned as empty until F05/F08/F10 wire legacy entrypoints.
// The runner-side identity (axiom-compute-worker block in
// /v1/capabilities) is witnessed by the runner's own suite
// (tests/test_contract.py::test_capabilities_canonical_identity) — the
// Go suite cannot import the Python model, and a hand-pinned copy would
// rot.
package baseline

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/server"
)

// canonicalIdentity is the projection of /api/health carrying ONLY the
// #296 identity fields (the build banner is commit-dependent, checks are
// state — neither belongs in a frozen fixture). Decoding the full
// response into this struct means a dropped or renamed handler field
// shows up as a zero value and goes red.
type canonicalIdentity struct {
	CanonicalName  string         `json:"canonical_name"`
	ServiceClass   string         `json:"service_class"`
	ComponentRoles []string       `json:"component_roles"`
	Deprecations   map[string]int `json:"deprecations"`
}

// identityOf derives the identity block from the REAL working-tree
// handler (never from constants — the gate must witness the response).
func identityOf(srv *server.Server) canonicalIdentity {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		panic("health status " + http.StatusText(rec.Code))
	}
	var id canonicalIdentity
	if err := json.Unmarshal(rec.Body.Bytes(), &id); err != nil {
		panic(err)
	}
	return id
}

// TestCanonicalIdentityFrozen — the additive canonical fields (ADR 0001)
// are locked: any change to name, class, roles or the deprecations shape
// goes red until BASELINE_UPDATE=1 lands in the same PR.
func TestCanonicalIdentityFrozen(t *testing.T) {
	goldenCompare(t, "canonical_identity.json", identityOf(buildFullServer()))
}

// TestCanonicalIdentityMutates — in-suite mutation sonde (the
// TestInventoryDummyRouteMutates pattern): removing one canonical field
// from the response MUST change the canonicalized snapshot, i.e. the
// frozen compare above could not still pass. The external probe (delete
// the field in server.go, run the suite) exercises the same path.
func TestCanonicalIdentityMutates(t *testing.T) {
	got := identityOf(buildFullServer())
	mutated := got
	mutated.CanonicalName = "" // as if the handler dropped the field
	b1, err := canonicalJSON(got)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := canonicalJSON(mutated)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) == string(b2) {
		t.Fatal("dropping a canonical field did not change the snapshot — the identity gate has no teeth")
	}
}
