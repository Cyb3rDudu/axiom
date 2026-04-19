package opensearch_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
)

// multiCaptureMock records every request path + method. We need this
// for IndexChunks which fires a HEAD, a PUT (index create), one index
// op per chunk, and a POST to _refresh.
type multiCaptureMock struct {
	mu       sync.Mutex
	calls    []call
	response func(r *http.Request) (int, []byte)
}

type call struct {
	method string
	path   string
	body   string
}

func newMultiMock(t *testing.T, resp func(r *http.Request) (int, []byte)) (*multiCaptureMock, *opensearch.Client) {
	t.Helper()
	m := &multiCaptureMock{response: resp}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		var bodyBytes []byte
		if r.Body != nil {
			bodyBytes, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		m.calls = append(m.calls, call{r.Method, r.URL.Path, string(bodyBytes)})
		m.mu.Unlock()
		code, body := http.StatusOK, []byte(`{}`)
		if m.response != nil {
			code, body = m.response(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)
	cfg := opensearch.DefaultConfig()
	cfg.Enabled = true
	cfg.Host = host
	cfg.Port = port
	cli, err := opensearch.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return m, cli
}

func TestIndexChunksCreatesIndexThenIndexesThenRefreshes(t *testing.T) {
	t.Parallel()
	// Respond: HEAD /axiom_chunks → 404, PUT /axiom_chunks → 200, rest 200.
	m, cli := newMultiMock(t, func(r *http.Request) (int, []byte) {
		if r.Method == http.MethodHead && r.URL.Path == "/axiom_chunks" {
			return http.StatusNotFound, []byte{}
		}
		return http.StatusOK, []byte(`{}`)
	})

	docs := []opensearch.ChunkDoc{
		{
			ChunkID:       "doc-1_chunk_0000",
			DocID:         uuid.New().String(),
			ChunkText:     "hello world",
			SectionTitles: "Intro",
			ChunkIndex:    0,
			TokenCount:    2,
			Metadata:      map[string]any{"title": "Paper"},
		},
		{
			ChunkID:    "doc-1_chunk_0001",
			DocID:      uuid.New().String(),
			ChunkText:  "second chunk",
			ChunkIndex: 1,
			TokenCount: 2,
			Metadata:   map[string]any{},
		},
	}

	if err := cli.IndexChunks(context.Background(), docs); err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}

	// Expected call pattern: HEAD (exists), PUT (create), POST/PUT per
	// chunk, POST _refresh.
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) < 5 {
		t.Fatalf("expected >=5 calls, got %d: %+v", len(m.calls), m.calls)
	}
	if m.calls[0].method != http.MethodHead {
		t.Errorf("first call: %s %s", m.calls[0].method, m.calls[0].path)
	}
	// Index creation body should include the BM25 mapping.
	if m.calls[1].method != http.MethodPut {
		t.Errorf("second call: %s %s", m.calls[1].method, m.calls[1].path)
	}
	if !strings.Contains(m.calls[1].body, "chunk_text") ||
		!strings.Contains(m.calls[1].body, "section_titles") {
		t.Errorf("create-index body missing fields: %s", m.calls[1].body)
	}
	// Last call is the refresh.
	last := m.calls[len(m.calls)-1]
	if !strings.Contains(last.path, "_refresh") {
		t.Errorf("last call not refresh: %+v", last)
	}
}

func TestIndexChunksSkipsCreateWhenIndexExists(t *testing.T) {
	t.Parallel()
	m, cli := newMultiMock(t, func(r *http.Request) (int, []byte) {
		if r.Method == http.MethodHead && r.URL.Path == "/axiom_chunks" {
			return http.StatusOK, []byte{}
		}
		return http.StatusOK, []byte(`{}`)
	})
	if err := cli.IndexChunks(context.Background(), []opensearch.ChunkDoc{
		{ChunkID: "x", DocID: uuid.New().String(), ChunkText: "hi"},
	}); err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.calls {
		if c.method == http.MethodPut && c.path == "/axiom_chunks" {
			t.Errorf("PUT /axiom_chunks should be skipped when index exists: %+v", c)
		}
	}
}

func TestIndexChunksSkipsEmptyChunkID(t *testing.T) {
	t.Parallel()
	m, cli := newMultiMock(t, func(r *http.Request) (int, []byte) {
		if r.Method == http.MethodHead {
			return http.StatusOK, []byte{}
		}
		return http.StatusOK, []byte(`{}`)
	})
	err := cli.IndexChunks(context.Background(), []opensearch.ChunkDoc{
		{ChunkID: "", DocID: uuid.New().String(), ChunkText: "skipped"},
		{ChunkID: "keep", DocID: uuid.New().String(), ChunkText: "indexed"},
	})
	if err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var keepIndexed, emptyIndexed bool
	for _, c := range m.calls {
		if strings.Contains(c.path, "/axiom_chunks/_doc/keep") ||
			strings.Contains(c.path, "/axiom_chunks/_create/keep") ||
			(c.path == "/axiom_chunks/_doc/keep") {
			keepIndexed = true
		}
		if strings.Contains(c.path, "/axiom_chunks/_doc/") && strings.HasSuffix(c.path, "/_doc/") {
			emptyIndexed = true
		}
	}
	if !keepIndexed {
		t.Errorf("non-empty chunk should have been indexed; calls=%+v", m.calls)
	}
	if emptyIndexed {
		t.Error("empty chunk_id should have been skipped")
	}
}

func TestDeleteDocumentSendsDeleteByQuery(t *testing.T) {
	t.Parallel()
	var got atomic.Value
	m, cli := newMultiMock(t, func(r *http.Request) (int, []byte) {
		if strings.Contains(r.URL.Path, "_delete_by_query") {
			got.Store(r.URL.Path)
		}
		return http.StatusOK, []byte(`{"deleted":1}`)
	})
	docID := uuid.New()
	if err := cli.DeleteDocument(context.Background(), docID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if p := got.Load(); p == nil || !strings.Contains(p.(string), "axiom_chunks") {
		t.Errorf("no delete_by_query observed: %+v", m.calls)
	}
}

func TestEnsureIndexReturnsErrDisabledOnNilClient(t *testing.T) {
	t.Parallel()
	var c *opensearch.Client
	if err := c.EnsureIndex(context.Background()); err != opensearch.ErrDisabled {
		t.Errorf("want ErrDisabled, got %v", err)
	}
	if err := c.IndexChunks(context.Background(), nil); err != opensearch.ErrDisabled {
		t.Errorf("IndexChunks want ErrDisabled, got %v", err)
	}
	if err := c.DeleteDocument(context.Background(), uuid.New()); err != opensearch.ErrDisabled {
		t.Errorf("DeleteDocument want ErrDisabled, got %v", err)
	}
}

func TestIndexChunksSurfacesServerError(t *testing.T) {
	t.Parallel()
	_, cli := newMultiMock(t, func(r *http.Request) (int, []byte) {
		if r.Method == http.MethodHead {
			return http.StatusOK, []byte{}
		}
		if strings.Contains(r.URL.Path, "/_doc/") {
			return http.StatusInternalServerError, []byte(`{"error":"boom"}`)
		}
		return http.StatusOK, []byte(`{}`)
	})
	err := cli.IndexChunks(context.Background(), []opensearch.ChunkDoc{
		{ChunkID: "x", DocID: uuid.New().String(), ChunkText: "body"},
	})
	if err == nil || !strings.Contains(err.Error(), "index chunk") {
		t.Fatalf("want wrapped index-chunk error, got %v", err)
	}
}
