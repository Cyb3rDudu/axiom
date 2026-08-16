package search

// R4 (#134) query-runner role switching: AXIOM_QUERY_RUNNER_URL selects
// which runner serves embed+rerank. What this pins: env -> config.Load() ->
// processor.New construction, with both roles (local default, external
// override) yielding a client whose runner answers §7a calls. The main.go
// wiring itself (which URL reaches search.New) is NOT covered here — it is
// observable via the startup role log ("runner roles: query=…") and proven
// end-to-end by the R3 IT, which takes the runner URL from env and ran
// against both a local and the Carrier runner. Answer correctness per
// runner is the search suite's job (it runs against per-test runner URLs).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

func fakeQueryRunner(t *testing.T, identity string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		switch r.URL.Path {
		case "/v1/embed":
			respond(map[string]any{
				"contract_version": "1.0", "model": identity, "dimensions": 2,
				"embeddings": [][]float32{{0.1, 0.2}},
			})
		case "/v1/rerank":
			respond(map[string]any{
				"contract_version": "1.0", "model": identity,
				"scores": []map[string]any{{"index": 0, "score": 0.9}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestQueryRunnerSwitchByEnv(t *testing.T) {
	local := fakeQueryRunner(t, "local-bge")
	external := fakeQueryRunner(t, "dedicated-bge")

	// Role: LOCAL (the default role model — always-on runner).
	t.Setenv("AXIOM_QUERY_RUNNER_URL", local.URL)
	if got := config.Load().QueryRunnerURL; got != local.URL {
		t.Fatalf("config must follow the env, got %q", got)
	}
	svcLocal := newService(local.URL, queryClientFor(t, local.URL), fakeDocs{})

	// Role: EXTERNAL (dedicated query runner) — same wiring, new URL.
	t.Setenv("AXIOM_QUERY_RUNNER_URL", external.URL)
	svcExternal := newService(external.URL, queryClientFor(t, external.URL), fakeDocs{})

	if svcLocal == nil || svcExternal == nil {
		t.Fatal("both roles must construct a search service")
	}

	// The §7a runner behind each configured role answers correctly.
	ctx := context.Background()
	for name, url := range map[string]string{"local": local.URL, "external": external.URL} {
		pc := queryClientFor(t, url)
		if _, err := pc.EmbedQueries(ctx, []string{"q"}); err != nil {
			t.Fatalf("%s role: embed failed: %v", name, err)
		}
		if _, err := pc.Rerank(ctx, "q", []string{"a"}, 1); err != nil {
			t.Fatalf("%s role: rerank failed: %v", name, err)
		}
	}
}

func queryClientFor(t *testing.T, url string) *processor.Client {
	t.Helper()
	c, err := processor.New(processor.Options{BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
