// inventory_test.go — API surface inventory, derived from the chi router
// (#295 Ziel 5). The route list is NOT handwritten: the test builds the
// real server (all conditionally-registered route groups wired) and walks
// the router with chi.Walk. Response classes are the only annotated part
// (not machine-derivable without invoking every handler); routes, patterns
// and methods come from the router itself.
//
// Drift semantics: a PR that adds/renames/removes a route changes the
// generated inventory → the fixture compare goes red → the PR must update
// fixtures/api_inventory.txt deliberately (visible in review).
// TestInventoryDummyRouteMutates proves a route change moves the output.
package baseline

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/server"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
	"github.com/go-chi/chi/v5"
)

// responseClass annotates a route pattern with its response shape. Routes
// are derived from the router; this map carries the one human-authored
// field. An unknown pattern renders as UNANNOTATED and fails the frozen
// compare loudly (the fixture would carry the marker).
var responseClass = map[string]string{
	"/api/health":      "json healthResponse",
	"/api/zotero/sync": "json sync.Result",
	"/api/ingest/jobs": "json []repo.Job",
	"/api/ingest/documents/{documentID}/force-rebuild": "json repo.Job (202)",
	"/api/zotero/selection":                            "json selection (GET/PUT)",
	"/api/zotero/selection/resolved":                   "json resolved selection",
	"/api/zotero/documents":                            "json document listing",
	"/api/search":                                      "json search.Response",
	"/api/passage/{id}":                                "json passage",
	"/api/passage/{id}/page":                           "image/png",
	"/api/kg/entities":                                 "json []entity",
	"/api/kg/entities/{id}/neighbors":                  "json neighbors",
	"/api/kg/relations":                                "json relations",
	"/api/kg/consolidate":                              "json consolidation report",
	"/api/ws":                                          "websocket (events/runners topics)",
	"/api/runners/live":                                "json runner live view",
	"/api/processor/source/{jobID}":                    "binary source artifact",
	"/api/repair/queue":                                "json repair queue items",
	"/api/repair/cases":                                "json repair cases listing",
	"/api/repair/cases/{id}/claim":                     "json repair case",
	"/api/repair/cases/{id}/requeue":                   "json ack",
	"/api/repair/cases/{id}/verdict":                   "json verdict result",
	"/api/repair/custody":                              "json custody record",
	"/api/repair/docs/{documentKey}/locator-stats":     "json locator stats",
}

// buildFullServer wires every conditionally-registered route group so the
// inventory shows the complete freeze surface (repair + consolidation are
// nil-gated in Handler(); their handlers are never invoked here — repo and
// write client are non-nil placeholders for route registration only).
func buildFullServer() *server.Server {
	srv := server.New("127.0.0.1:0", log.New(os.Stderr, "", 0))
	srv.SetRepairAPI(repo.New(nil),
		zotero.NewWriteClient("http://127.0.0.1:1", "", "baseline-inventory"), "")
	srv.SetConsolidateService(noopConsolidator{})
	return srv
}

type noopConsolidator struct{}

func (noopConsolidator) ConsolidateEntitiesReport(context.Context) (repo.ConsolidationReport, error) {
	return repo.ConsolidationReport{}, nil
}

// generateInventory walks the real router: one line per method+pattern
// (sorted; chi.Walk order is registration order), plus the response class.
// The runner (:8112) surface is appended as a fixed block — a separate
// FastAPI process frozen at the same generation; its route list was
// derived from its router (fastapi app.routes) at freeze time and is
// pinned here so the inventory covers both halves of the API surface.
func generateInventory(srv *server.Server) string {
	return generateInventoryFrom(srv.Handler())
}

func generateInventoryFrom(h http.Handler) string {
	mux, ok := h.(*chi.Mux)
	if !ok {
		panic("server handler is not the raw chi mux")
	}
	var rows []string
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		class, known := responseClass[route]
		if !known {
			class = "UNANNOTATED"
		}
		rows = append(rows, fmt.Sprintf("%-6s %-48s %s", method, route, class))
		return nil
	})
	if err != nil {
		panic(err) // walk cannot fail with the always-nil closure
	}
	sort.Strings(rows)
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r)
		b.WriteByte('\n')
	}
	b.WriteString("\n# runner (axiom_ng_runner FastAPI, :8112) — same freeze generation\n")
	for _, r := range []string{
		"GET    /v1/health                                        json health",
		"GET    /v1/capabilities                                  json Capabilities",
		"POST   /v1/process                                       202 json job accepted",
		"GET    /v1/jobs/{job_id}                                 json JobStatus",
		"GET    /v1/jobs/{job_id}/result                          json processing result",
		"GET    /v1/jobs/{job_id}/artifacts/{artifact_ref}        binary artifact",
		"POST   /v1/jobs/{job_id}/cancel                          json ack",
		"POST   /v1/jobs/{job_id}/ack                             json ack",
		"POST   /v1/embed                                         json embeddings",
		"POST   /v1/rerank                                        json rerank result",
		"POST   /v1/pdf/preflight                                 json PreflightReport",
	} {
		b.WriteString(r)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestInventoryFrozen(t *testing.T) {
	got := generateInventory(buildFullServer())
	if writeActualOrFixture(t, "api_inventory.txt", []byte(got), fixtureUpdate()) {
		return
	}
	if want := readFixture(t, "api_inventory.txt"); !bytes.Equal(want, []byte(got)) {
		t.Fatalf("API inventory drift:\n--- frozen ---\n%s\n+++ actual +++\n%s", want, got)
	}
}

// TestInventoryDummyRouteMutates — DoD proof: adding a route MUST change
// the generated inventory, or the inventory gate could not catch surface
// changes. The extra route is registered on a real chi mux carrying the
// same fully-wired server handler (Mount /) plus one dummy POST route.
func TestInventoryDummyRouteMutates(t *testing.T) {
	base := generateInventory(buildFullServer())

	mux := chi.NewMux()
	mux.Mount("/", buildFullServer().Handler())
	mux.Post("/api/dummy/route", func(http.ResponseWriter, *http.Request) {})
	withDummy := generateInventoryFrom(mux)

	if withDummy == base {
		t.Fatal("adding a dummy route did not change the generated inventory — the gate has no teeth")
	}
	if !strings.Contains(withDummy, "/api/dummy/route") {
		t.Fatal("dummy route missing from generated inventory")
	}
}
