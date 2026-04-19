package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/retriever"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/server"
)

// --- stubs ---

type stubFulltext struct {
	hits []opensearch.Hit
	err  error
}

func (s *stubFulltext) BM25Search(_ context.Context, _ opensearch.SearchOptions) ([]opensearch.Hit, error) {
	return s.hits, s.err
}

type stubRetriever struct {
	results []retriever.Result
	err     error
}

func (s *stubRetriever) Retrieve(_ context.Context, _ retriever.Options) ([]retriever.Result, error) {
	return s.results, s.err
}

type stubUserDocs struct {
	ids      []uuid.UUID
	err      error
	groupIDs []uuid.UUID
	groupErr error
	groupHit uuid.UUID // if set and matches, return groupIDs
}

func (s *stubUserDocs) DocIDs(_ context.Context, _ int32) ([]uuid.UUID, error) {
	return s.ids, s.err
}

func (s *stubUserDocs) DocIDsInGroup(_ context.Context, _ int32, groupID uuid.UUID) ([]uuid.UUID, error) {
	if s.groupErr != nil {
		return nil, s.groupErr
	}
	if s.groupHit != uuid.Nil && groupID == s.groupHit {
		return s.groupIDs, nil
	}
	return s.groupIDs, nil
}

// --- fixture ---

type searchFixture struct {
	srv       *httptest.Server
	fulltext  *stubFulltext
	retriever *stubRetriever
	userDocs  *stubUserDocs
	token     string
}

func newSearchFixture(t *testing.T, attachOS, attachRet bool) *searchFixture {
	t.Helper()
	f := &searchFixture{
		fulltext:  &stubFulltext{},
		retriever: &stubRetriever{},
		userDocs:  &stubUserDocs{ids: []uuid.UUID{uuid.New()}},
	}
	var osClient api.SearchFulltextClient
	if attachOS {
		osClient = f.fulltext
	}
	var ret api.HybridRetriever
	if attachRet {
		ret = f.retriever
	}
	signer, err := auth.NewSigner("search-test")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	deps := server.Deps{
		Search: api.SearchDeps{OpenSearch: osClient, Retriever: ret, UserDocs: f.userDocs},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: stubLookup{user: authctx.User{ID: 42, Username: "alice"}},
		},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)

	tok, err := signer.Issue("alice", false, auth.AccessTokenLifetime)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	f.token = tok
	return f
}

// --- tests ---

func TestFulltextRejectsShortQuery(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=a", f.token)
	if status != http.StatusBadRequest {
		t.Errorf("short query: got %d", status)
	}
}

func TestFulltextReturnsServiceUnavailableWhenNoOpenSearch(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, false, true)
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag", f.token)
	if status != http.StatusServiceUnavailable {
		t.Errorf("no OS: got %d", status)
	}
}

func TestFulltextBadGroupID(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag&group_id=not-a-uuid", f.token)
	if status != http.StatusBadRequest {
		t.Errorf("bad group_id: got %d", status)
	}
}

func TestFulltextEmptyLibraryShortCircuits(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	f.userDocs.ids = nil
	status, body := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag", f.token)
	if status != http.StatusOK {
		t.Fatalf("status: %d", status)
	}
	if !bytes.Equal(bytes.TrimSpace(body), []byte("[]")) {
		t.Errorf("empty body expected: %s", body)
	}
}

func TestFulltextUserDocsErrorSurfaces500(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	f.userDocs.err = errors.New("db")
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag", f.token)
	if status != http.StatusInternalServerError {
		t.Errorf("userdocs err: got %d", status)
	}
}

func TestFulltextDedupsByDocID(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	docA := uuid.New()
	docB := uuid.New()
	f.fulltext.hits = []opensearch.Hit{
		{ChunkID: "a_0", DocID: docA, Text: "first a chunk", Score: 4.0, Metadata: map[string]any{"title": "A"}},
		{ChunkID: "a_1", DocID: docA, Text: "second a chunk (higher score)", Score: 9.0, Metadata: map[string]any{"title": "A"}},
		{ChunkID: "b_0", DocID: docB, Text: "only b chunk", Score: 3.5, Metadata: map[string]any{"title": "B"}},
	}
	status, body := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag", f.token)
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	var hits []api.FulltextHit
	if err := json.Unmarshal(body, &hits); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("dedup failed: %d rows (%s)", len(hits), body)
	}
	if hits[0].ID != docA || hits[0].Score != 9.0 {
		t.Errorf("winning doc: got %+v", hits[0])
	}
}

func TestFulltextTruncatesSnippet(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	long := string(bytes.Repeat([]byte("x"), 500))
	f.fulltext.hits = []opensearch.Hit{
		{ChunkID: "a_0", DocID: uuid.New(), Text: long, Score: 1.0, Metadata: map[string]any{}},
	}
	_, body := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag", f.token)
	var hits []api.FulltextHit
	_ = json.Unmarshal(body, &hits)
	if len(hits[0].Snippet) != 200 {
		t.Errorf("snippet length: %d", len(hits[0].Snippet))
	}
}

func TestFulltextLimitQueryParamClamps(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	f.fulltext.hits = []opensearch.Hit{
		{ChunkID: "a_0", DocID: uuid.New(), Text: "a", Score: 5},
		{ChunkID: "b_0", DocID: uuid.New(), Text: "b", Score: 4},
	}
	status, body := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag&limit=1", f.token)
	if status != http.StatusOK {
		t.Fatalf("%d", status)
	}
	var hits []api.FulltextHit
	_ = json.Unmarshal(body, &hits)
	if len(hits) != 1 {
		t.Errorf("limit: got %d rows", len(hits))
	}
}

func TestFulltextOpenSearchDisabled(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	f.fulltext.err = opensearch.ErrDisabled
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag", f.token)
	if status != http.StatusServiceUnavailable {
		t.Errorf("disabled: got %d", status)
	}
}

func TestFulltextOpenSearchError(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	f.fulltext.err = errors.New("boom")
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag", f.token)
	if status != http.StatusBadGateway {
		t.Errorf("err: got %d", status)
	}
}

func TestSearchRequiresQuery(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/search/", f.token)
	if status != http.StatusBadRequest {
		t.Errorf("missing query: got %d", status)
	}
}

func TestSearchNilRetrieverReturnsEmpty(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, false)
	status, body := getSearchAuthed(t, f.srv.URL+"/api/search/?query=rag", f.token)
	if status != http.StatusOK {
		t.Fatalf("%d", status)
	}
	if !bytes.Contains(body, []byte(`"results":[]`)) {
		t.Errorf("empty: %s", body)
	}
}

func TestSearchBadGroupID(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/search/?query=rag&group_id=nope", f.token)
	if status != http.StatusBadRequest {
		t.Errorf("bad group_id: got %d", status)
	}
}

func TestSearchRetrieverError(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	f.retriever.err = errors.New("db")
	status, _ := getSearchAuthed(t, f.srv.URL+"/api/search/?query=rag", f.token)
	if status != http.StatusInternalServerError {
		t.Errorf("retriever err: got %d", status)
	}
}

func TestSearchEmptyLibraryReturnsEmpty(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	f.userDocs.ids = nil
	status, body := getSearchAuthed(t, f.srv.URL+"/api/search/?query=rag", f.token)
	if status != http.StatusOK {
		t.Fatalf("%d", status)
	}
	if !bytes.Contains(body, []byte(`"results":[]`)) {
		t.Errorf("empty: %s", body)
	}
}

func TestFulltextScopesByGroupID(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	groupID := uuid.New()
	groupDocID := uuid.New()
	// Library has two docs but only one belongs to the group.
	f.userDocs.ids = []uuid.UUID{uuid.New(), groupDocID}
	f.userDocs.groupIDs = []uuid.UUID{groupDocID}
	f.userDocs.groupHit = groupID
	f.fulltext.hits = []opensearch.Hit{
		{ChunkID: "g_0", DocID: groupDocID, Text: "in group", Score: 9.0},
	}
	status, body := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag&group_id="+groupID.String(), f.token)
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	var hits []api.FulltextHit
	_ = json.Unmarshal(body, &hits)
	if len(hits) != 1 || hits[0].ID != groupDocID {
		t.Errorf("expected single in-group result, got %+v", hits)
	}
}

func TestFulltextSurfacesMetadataFields(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	docID := uuid.New()
	f.fulltext.hits = []opensearch.Hit{
		{
			ChunkID: "a_0", DocID: docID, Text: "x", Score: 1.0,
			Metadata: map[string]any{
				"title":             "A Paper",
				"original_filename": "paper.pdf",
				"authors":           []any{"Alice", "Bob"},
				"publication_year":  float64(2025),
				"document_type":     "article",
			},
		},
	}
	status, body := getSearchAuthed(t, f.srv.URL+"/api/documents/search/fulltext?query=rag", f.token)
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	var hits []api.FulltextHit
	if err := json.Unmarshal(body, &hits); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits", len(hits))
	}
	h := hits[0]
	if h.Title != "A Paper" || h.OriginalFilename != "paper.pdf" || h.DocumentType != "article" {
		t.Errorf("metadata extraction: %+v", h)
	}
	if len(h.Authors) != 2 || h.Authors[0] != "Alice" {
		t.Errorf("authors: %+v", h.Authors)
	}
	if h.PublicationYear == nil || *h.PublicationYear != 2025 {
		t.Errorf("year: %+v", h.PublicationYear)
	}
}

func TestSearchReturnsHits(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t, true, true)
	docID := uuid.New()
	f.retriever.results = []retriever.Result{
		{ChunkID: "a_0", DocID: docID, Text: "match", Score: 0.91},
	}
	status, body := getSearchAuthed(t, f.srv.URL+"/api/search/?query=rag&n_results=5&rerank=true", f.token)
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	var env map[string]any
	_ = json.Unmarshal(body, &env)
	results := env["results"].([]any)
	if len(results) != 1 {
		t.Errorf("result count: %d", len(results))
	}
}

// getSearchAuthed is a tiny helper distinct from the e2e-test's
// `do` helpers since those take a fixture.
func getSearchAuthed(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: token})
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed below
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
