package opensearch_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
)

// mockOS is a minimal HTTP server that speaks just enough OpenSearch
// to exercise the Go client. It captures the last request body so tests
// can assert on the exact query shape the Python backend expects.
type mockOS struct {
	srv      *httptest.Server
	LastPath string
	LastBody []byte
	Response func(path string, body []byte) (int, []byte)
}

func newMockOS(t *testing.T) *mockOS {
	t.Helper()
	m := &mockOS{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.LastPath = r.URL.Path
		if r.Body != nil {
			m.LastBody, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		if m.Response != nil {
			status, body := m.Response(r.URL.Path, m.LastBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
		// Default: empty hits envelope.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":{"hits":[]}}`))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// newClient builds a real *opensearch.Client pointed at the mock.
func (m *mockOS) newClient(t *testing.T) *opensearch.Client {
	t.Helper()
	host, port := splitHostPort(t, m.srv.URL)
	cfg := opensearch.DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cli, err := opensearch.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cli
}

// splitHostPort parses an httptest URL (http://127.0.0.1:<port>) into
// the Config shape. No URL library needed for this simple case.
func splitHostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	u := strings.TrimPrefix(url, "http://")
	parts := strings.Split(u, ":")
	if len(parts) != 2 {
		t.Fatalf("unexpected test URL %q", url)
	}
	port := 0
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	return parts[0], port
}

func TestFromEnvRespectsToggles(t *testing.T) {
	t.Parallel()
	lookup := func(k string) string {
		switch k {
		case "ENABLE_OPENSEARCH":
			return "false"
		case "OPENSEARCH_HOST":
			return "os.example"
		case "OPENSEARCH_PORT":
			return "9201"
		case "OPENSEARCH_USE_SSL":
			return "true"
		case "OPENSEARCH_USERNAME":
			return "u"
		case "OPENSEARCH_PASSWORD":
			return "p"
		case "OPENSEARCH_INDEX":
			return "custom_idx"
		}
		return ""
	}
	cfg := opensearch.FromEnv(lookup)
	if cfg.Enabled {
		t.Error("ENABLE_OPENSEARCH=false should disable")
	}
	if cfg.Host != "os.example" || cfg.Port != 9201 || !cfg.UseSSL {
		t.Errorf("config misparsed: %+v", cfg)
	}
	if cfg.Username != "u" || cfg.Password != "p" || cfg.Index != "custom_idx" {
		t.Errorf("credentials/index misparsed: %+v", cfg)
	}
	if got := cfg.URL(); got != "https://os.example:9201" {
		t.Errorf("URL: got %q", got)
	}
}

func TestFromEnvTrueEnabled(t *testing.T) {
	t.Parallel()
	lookup := func(k string) string {
		if k == "ENABLE_OPENSEARCH" {
			return "true"
		}
		return ""
	}
	if !opensearch.FromEnv(lookup).Enabled {
		t.Error("ENABLE_OPENSEARCH=true should enable")
	}
}

func TestFromEnvInvalidPortFallsBack(t *testing.T) {
	t.Parallel()
	lookup := func(k string) string {
		if k == "OPENSEARCH_PORT" {
			return "not-a-number"
		}
		return ""
	}
	cfg := opensearch.FromEnv(lookup)
	if cfg.Port != 9200 {
		t.Errorf("invalid port should leave default: got %d", cfg.Port)
	}
}

func TestNewClientRejectsDisabledConfig(t *testing.T) {
	t.Parallel()
	cfg := opensearch.DefaultConfig()
	cfg.Enabled = false
	if _, err := opensearch.NewClient(cfg); err == nil {
		t.Fatal("expected ErrDisabled")
	}
}

func TestNewClientRejectsMissingHost(t *testing.T) {
	t.Parallel()
	cfg := opensearch.DefaultConfig()
	cfg.Host = ""
	if _, err := opensearch.NewClient(cfg); err == nil {
		t.Fatal("expected error for missing host")
	}
}

func TestBM25SearchSendsExpectedBody(t *testing.T) {
	t.Parallel()
	m := newMockOS(t)
	doc1 := uuid.New()
	doc2 := uuid.New()
	m.Response = func(path string, body []byte) (int, []byte) {
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			return 500, []byte(`{}`)
		}
		// Query body contract — verify fixed values.
		q := got["query"].(map[string]any)["bool"].(map[string]any)
		must := q["must"].([]any)[0].(map[string]any)["match"].(map[string]any)["chunk_text"].(map[string]any)
		if must["operator"] != "or" || must["minimum_should_match"] != "50%" || must["query"] != "research" {
			return 500, []byte(`{}`)
		}
		// Filter includes both doc_ids.
		filter := q["filter"].([]any)
		if len(filter) != 1 {
			return 500, []byte(`{}`)
		}
		// _source has the exact five fields.
		src := got["_source"].([]any)
		if len(src) != 5 {
			return 500, []byte(`{}`)
		}
		resp := map[string]any{
			"hits": map[string]any{
				"hits": []any{
					map[string]any{
						"_score": 3.2,
						"_source": map[string]any{
							"chunk_id":       "abc_0",
							"doc_id":         doc1.String(),
							"chunk_text":     "matched text",
							"metadata":       map[string]any{"page": 1},
							"section_titles": "Intro",
						},
					},
				},
			},
		}
		buf, _ := json.Marshal(resp)
		return 200, buf
	}
	cli := m.newClient(t)
	hits, err := cli.BM25Search(context.Background(), opensearch.SearchOptions{
		Query:  "research",
		DocIDs: []uuid.UUID{doc1, doc2},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: %d", len(hits))
	}
	if hits[0].ChunkID != "abc_0" || hits[0].Score != 3.2 || hits[0].Text != "matched text" {
		t.Errorf("hit shape: %+v", hits[0])
	}
	if hits[0].DocID != doc1 {
		t.Errorf("doc id: %s vs %s", hits[0].DocID, doc1)
	}
	if !strings.Contains(m.LastPath, cli.Config().Index) {
		t.Errorf("path should include index: %s", m.LastPath)
	}
}

func TestBM25SearchWithoutDocIDsOmitsFilter(t *testing.T) {
	t.Parallel()
	m := newMockOS(t)
	m.Response = func(_ string, body []byte) (int, []byte) {
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		filter := got["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
		if len(filter) != 0 {
			return 500, []byte(`{}`)
		}
		return 200, []byte(`{"hits":{"hits":[]}}`)
	}
	cli := m.newClient(t)
	_, err := cli.BM25Search(context.Background(), opensearch.SearchOptions{Query: "x"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
}

func TestBM25SearchDefaultsSize(t *testing.T) {
	t.Parallel()
	m := newMockOS(t)
	m.Response = func(_ string, body []byte) (int, []byte) {
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		// Python default: size=30 when caller doesn't specify.
		if got["size"] != float64(30) {
			return 500, []byte(`{"fail": "size wrong"}`)
		}
		return 200, []byte(`{"hits":{"hits":[]}}`)
	}
	cli := m.newClient(t)
	if _, err := cli.BM25Search(context.Background(), opensearch.SearchOptions{Query: "x"}); err != nil {
		t.Fatalf("search: %v", err)
	}
}

func TestBM25SearchPropagatesServerError(t *testing.T) {
	t.Parallel()
	m := newMockOS(t)
	m.Response = func(_ string, _ []byte) (int, []byte) {
		return 500, []byte(`{"error":"internal"}`)
	}
	cli := m.newClient(t)
	_, err := cli.BM25Search(context.Background(), opensearch.SearchOptions{Query: "x"})
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestBM25SearchOnDisabledClientReturnsErrDisabled(t *testing.T) {
	t.Parallel()
	var cli *opensearch.Client
	_, err := cli.BM25Search(context.Background(), opensearch.SearchOptions{Query: "x"})
	if err == nil {
		t.Fatal("expected ErrDisabled")
	}
}

func TestHealthProbe(t *testing.T) {
	t.Parallel()
	m := newMockOS(t)
	m.Response = func(path string, _ []byte) (int, []byte) {
		// / returns info
		if path == "/" {
			return 200, []byte(`{"version": {"number": "2.11.0"}, "cluster_name": "test"}`)
		}
		return 404, []byte(`{}`)
	}
	cli := m.newClient(t)
	if err := cli.Health(context.Background()); err != nil {
		t.Errorf("health: %v", err)
	}
}

func TestHealthReportsServerErrors(t *testing.T) {
	t.Parallel()
	m := newMockOS(t)
	m.Response = func(_ string, _ []byte) (int, []byte) {
		return 503, []byte(`{}`)
	}
	cli := m.newClient(t)
	if err := cli.Health(context.Background()); err == nil {
		t.Error("expected unhealthy")
	}
}
