// Package search implements the retrieval pipeline behind POST /api/search
// (epic #130 R3, #133): hybrid recall from OpenSearch (dense kNN via the
// runner's query embedding + BM25), Reciprocal-Rank-Fusion merge, optional
// cross-encoder rerank through the runner with a graceful fallback, and
// source/locator hydration from the durable store.
//
// Reference architecture: the proven retriever.py of the old Python system,
// transferred to Go + contract world — not reinvented. Graph (R6) and sparse
// (R5) arms are deliberately absent but the arm structure (searchArm) leaves
// them a slot.
package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// IndexName must match the outbox worker's index (dispatcher.outboxIndexName);
// duplicated here because the search side only consumes, never manages, it.
const IndexName = "axiom-ng-chunks-v1"

// RRF k constant (standard 60; the old system tuned dense/sparse weights
// before fusion — R7 territory, start plain).
const rrfK = 60.0

// maxCandidates caps the overfetch (3x top_n) at the runner's rerank_max_texts
// (64): reranking more than the runner accepts would silently truncate.
const maxCandidates = 64

// maxRerankTextChars bounds each candidate text sent to the reranker: the
// cross-encoder truncates at 512 tokens anyway, so longer payloads only burn
// loopback bandwidth.
const maxRerankTextChars = 4000

// Service performs hybrid retrieval.
type Service struct {
	os        *osClient
	processor Processor // runner embed/rerank
	docs      DocSource // source metadata hydration
	log       *log.Logger
	// SparseArm enables the third recall arm (learned lexical weights via
	// the OS rank_features field, R5 #135). Default false per the R7
	// benchmark (no quality gain, heavy query cost); enable per deployment
	// or benchmark (AXIOM_SEARCH_SPARSE_ARM). Requires DenseArm: the sparse
	// query weights ride the dense embed call (SparseArm with DenseArm=false
	// is silently inert).
	SparseArm bool
	// GraphArm enables the fourth candidate source (R6 #136): entities in
	// the hybrid top hits expand to neighbor chunks through the L6 graph
	// (mention-stability filtered). Default OFF — R7 measures whether it
	// helps (AXIOM_SEARCH_GRAPH_ARM). Requires SetGraphSource.
	GraphArm bool
	// Rerank runs the cross-encoder over the fused candidates (default
	// true). R7's matrix measures what it buys; ops can disable it for a
	// latency-only profile (AXIOM_SEARCH_RERANK).
	Rerank bool
	// DenseArm / BM25Arm gate the base recall arms (default true).
	// Benchmark levers (retrieval-bench matrix); no env wiring by design.
	// A disabled arm is treated like a failed one (flags reflect it).
	DenseArm bool
	BM25Arm  bool
	graph    GraphSource
}

// GraphSource expands seed chunks through stable entities into neighbor
// chunks (implemented by repo.Repo.GraphCandidates).
type GraphSource interface {
	GraphCandidates(ctx context.Context, seedChunkIDs []string, minMentions, limit int) ([]repo.KGChunkCandidate, error)
}

// SetGraphSource wires the graph expansion source (nil keeps the arm off
// even when GraphArm is set).
func (s *Service) SetGraphSource(g GraphSource) { s.graph = g }

// graphMinMentions is the stability floor for graph expansion (same L8-§6
// rationale as the KG API default).
const graphMinMentions = 2

// Processor is the runner query-side surface the pipeline needs
// (implemented by processor.Client).
type Processor interface {
	EmbedQueries(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQueriesSparse is the R5 (#135) combined call: dense vectors plus
	// learned lexical weights for the sparse rank_features arm.
	EmbedQueriesSparse(ctx context.Context, texts []string) ([][]float32, []map[string]float64, error)
	Rerank(ctx context.Context, query string, texts []string, topN int) ([]processor.RerankScore, error)
}

// DocSource hydrates bibliographic metadata for hit documents
// (implemented by repo.Repo).
type DocSource interface {
	DocumentMetaByIDs(ctx context.Context, ids []string) (map[string]repo.DocumentMeta, error)
}

// New builds a Service.
func New(osURL, osUser, osPass string, p Processor, docs DocSource, lg *log.Logger) *Service {
	if lg == nil {
		lg = log.Default()
	}
	return &Service{
		os:        newOSClient(osURL, osUser, osPass),
		processor: p,
		docs:      docs,
		log:       lg,
		SparseArm: false,
		Rerank:    true,
		DenseArm:  true,
		BM25Arm:   true,
	}
}

// Request is POST /api/search.
type Request struct {
	Query string `json:"query"`
	TopN  int    `json:"top_n"`
	// Filters is optional; v1 supports {"document_ids": ["uuid", ...]}.
	Filters *Filters `json:"filters,omitempty"`
}

// Filters narrows both recall arms.
type Filters struct {
	DocumentIDs []string `json:"document_ids,omitempty"`
}

// Hit is one ranked answer with its provenance.
type Hit struct {
	ChunkID string      `json:"chunk_id"`
	Text    string      `json:"text"`
	Score   float64     `json:"score"` // RRF score, or rerank score when Response.Reranked
	Source  Source      `json:"source"`
	Locator LocatorView `json:"locator"`
	Section []string    `json:"section"`
}

// Source is the bibliographic provenance of a hit.
type Source struct {
	Book      string   `json:"book"`
	Authors   []string `json:"authors"`
	Year      *int     `json:"year,omitempty"`
	Publisher string   `json:"publisher,omitempty"`
	DocID     string   `json:"document_id"`
}

// LocatorView is the human-usable locator (issue Ziel 5: page_span → "S. 47",
// epub_cfi → chapter/CFI short form).
type LocatorView struct {
	Kind    string `json:"kind"`              // "page" | "epub_cfi"
	Label   string `json:"label"`             // e.g. "S. 47" or "Kap. 3 · S. 12"
	Chapter string `json:"chapter,omitempty"` // deepest section title
	CFI     string `json:"cfi,omitempty"`     // epub CFI short form
}

// Response is POST /api/search.
type Response struct {
	Query    string `json:"query"`
	TopN     int    `json:"top_n"`
	Reranked bool   `json:"reranked"` // false = RRF order after rerank failure/skip
	Arms     Arms   `json:"arms"`     // which recall arms contributed
	Hits     []Hit  `json:"hits"`
	TookMS   int64  `json:"took_ms"`
}

// Arms records recall-arm health for this request.
type Arms struct {
	Dense  bool `json:"dense"`
	BM25   bool `json:"bm25"`
	Sparse bool `json:"sparse,omitempty"`
}

// Search runs the full pipeline. Errors from single arms degrade the result
// (arm flag false); only total failure (no arm) is an error.
func (s *Service) Search(ctx context.Context, req Request) (*Response, error) {
	t0 := time.Now()
	if strings.TrimSpace(req.Query) == "" {
		return nil, ErrBadRequest("query must not be blank")
	}
	if req.TopN <= 0 {
		req.TopN = 10
	}
	if req.TopN > maxCandidates {
		return nil, ErrBadRequest(fmt.Sprintf("top_n must be <= %d", maxCandidates))
	}

	// Overfetch 3x for the reranker (old system: top-30 → top-10).
	fetchN := 3 * req.TopN
	if fetchN > maxCandidates {
		fetchN = maxCandidates
	}

	resp := &Response{Query: req.Query, TopN: req.TopN}

	// Dense arm: query embedding via the runner. Failure (runner down,
	// model not loadable) degrades to BM25-only instead of failing search.
	// With the sparse arm enabled the same call carries the learned lexical
	// weights (one encode pass, R5 #135).
	var vec []float32
	var querySparse map[string]float64
	switch {
	case !s.DenseArm:
		// Dense arm disabled (retrieval-bench matrix lever): no embed call,
		// Arms.Dense stays false. SparseArm is inert here — its query
		// weights ride the dense embed call (see the SparseArm field doc).
	case s.SparseArm:
		if emb, sp, err := s.processor.EmbedQueriesSparse(ctx, []string{req.Query}); err != nil {
			s.log.Printf("search: dense+sparse embed failed, trying dense-only: %v", err)
			s.embedDenseOnly(ctx, req.Query, &vec, &resp.Arms.Dense)
		} else {
			vec, querySparse = emb[0], sp[0]
			resp.Arms.Dense = vec != nil
		}
	default:
		s.embedDenseOnly(ctx, req.Query, &vec, &resp.Arms.Dense)
	}

	// Recall arms in parallel; each returns rank-ordered chunk IDs.
	type armResult struct {
		hits []osHit
		err  error
	}
	denseCh := make(chan armResult, 1)
	bm25Ch := make(chan armResult, 1)
	sparseCh := make(chan armResult, 1)
	go func() {
		if vec == nil {
			denseCh <- armResult{}
			return
		}
		hits, err := s.os.knn(ctx, vec, fetchN, req.Filters)
		denseCh <- armResult{hits, err}
	}()
	go func() {
		if !s.BM25Arm {
			bm25Ch <- armResult{}
			return
		}
		hits, err := s.os.bm25(ctx, req.Query, fetchN, req.Filters)
		bm25Ch <- armResult{hits, err}
	}()
	go func() {
		// Empty-but-non-nil weights must not fire a query (nor claim the
		// arm): a fully pruned query vector means no lexical signal.
		if len(querySparse) == 0 {
			sparseCh <- armResult{}
			return
		}
		hits, err := s.os.sparse(ctx, querySparse, fetchN, req.Filters)
		sparseCh <- armResult{hits, err}
	}()
	dense := <-denseCh
	bm25 := <-bm25Ch
	sparse := <-sparseCh
	if dense.err != nil {
		s.log.Printf("search: dense arm failed: %v", dense.err)
		resp.Arms.Dense = false
	}
	if !s.BM25Arm {
		resp.Arms.BM25 = false
	} else if bm25.err != nil {
		s.log.Printf("search: bm25 arm failed: %v", bm25.err)
		resp.Arms.BM25 = false
	} else {
		resp.Arms.BM25 = true
	}
	if sparse.err != nil {
		s.log.Printf("search: sparse arm failed (degrading to 2-arm hybrid): %v", sparse.err)
	} else if len(querySparse) > 0 {
		resp.Arms.Sparse = true
	}
	if !resp.Arms.Dense && !resp.Arms.BM25 {
		return nil, fmt.Errorf("search: no recall arm succeeded")
	}

	// RRF merge over the arms (rank 1-based per arm). Arm order fixes tie
	// order: dense first, then BM25, then sparse (graph appends as 4th).
	recallArms := []([]osHit){dense.hits, bm25.hits, sparse.hits}
	merged := rrfMerge(recallArms, fetchN)

	// Graph arm (R6 #136, default off): expand the hybrid top through the
	// L6 graph and re-fuse — neighbor chunks enter the candidate pool with
	// their own RRF ranks, never displacing hybrid hits outright.
	if s.GraphArm && s.graph != nil && len(merged) > 0 {
		seed := make([]string, 0, min(req.TopN, len(merged)))
		for _, c := range merged {
			if len(seed) == req.TopN {
				break
			}
			seed = append(seed, c.ID)
		}
		cands, gerr := s.graph.GraphCandidates(ctx, seed, graphMinMentions, fetchN)
		if gerr != nil {
			s.log.Printf("search: graph arm failed (continuing without): %v", gerr)
		} else if len(cands) > 0 {
			graphHits := make([]osHit, 0, len(cands))
			for _, c := range cands {
				loc, _ := json.Marshal(c.Locator)
				graphHits = append(graphHits, osHit{
					ID: c.ChunkID, Text: c.Text, DocumentID: c.DocumentID,
					Locator: loc, SectionTitles: c.SectionTitles,
				})
			}
			merged = rrfMerge(append(recallArms, graphHits), fetchN)
		}
	}
	if len(merged) == 0 {
		resp.Hits = []Hit{}
		resp.TookMS = msSince(t0)
		return resp, nil
	}

	// Hydrate source metadata (document titles/authors) for the candidates.
	byDoc := map[string]struct{}{}
	for _, c := range merged {
		byDoc[c.DocumentID] = struct{}{}
	}
	docIDs := make([]string, 0, len(byDoc))
	for id := range byDoc {
		docIDs = append(docIDs, id)
	}
	meta := map[string]repo.DocumentMeta{}
	if s.docs != nil {
		if m, err := s.docs.DocumentMetaByIDs(ctx, docIDs); err != nil {
			s.log.Printf("search: source hydration failed: %v", err)
		} else {
			meta = m
		}
	}

	// Rerank over the merged candidates; failure degrades to RRF order
	// (issue Ziel 3: retrieval must not hard-depend on the reranker).
	candidates := merged // already capped at fetchN <= maxCandidates by rrfMerge
	if !s.Rerank {
		// Rerank disabled by configuration (R7 matrix / latency profile):
		// RRF order is the answer, flagged as unreranked.
		resp.Reranked = false
	} else {
		texts := make([]string, len(candidates))
		for i, c := range candidates {
			texts[i] = truncateChars(c.Text, maxRerankTextChars)
		}
		scores, err := s.processor.Rerank(ctx, req.Query, texts, len(texts))
		if err != nil {
			s.log.Printf("search: rerank failed, serving RRF order: %v", err)
			resp.Reranked = false
		} else {
			resp.Reranked = applyRerank(candidates, scores)
		}
	}
	if len(candidates) > req.TopN {
		candidates = candidates[:req.TopN]
	}

	resp.Hits = make([]Hit, len(candidates))
	for i, c := range candidates {
		h := Hit{
			ChunkID: c.ID,
			Text:    c.Text,
			Score:   c.RRFScore,
			Source:  sourceFor(meta[c.DocumentID], c.DocumentID),
			Locator: locatorView(c.Locator, c.SectionTitles),
			Section: c.SectionTitles,
		}
		if resp.Reranked {
			h.Score = c.RerankScore
		}
		resp.Hits[i] = h
	}
	resp.TookMS = msSince(t0)
	return resp, nil
}

// embedDenseOnly is the degraded embed path when the combined call fails.
func (s *Service) embedDenseOnly(ctx context.Context, query string, vec *[]float32, denseOK *bool) {
	if emb, err := s.processor.EmbedQueries(ctx, []string{query}); err != nil {
		s.log.Printf("search: dense arm skipped (embed failed): %v", err)
	} else {
		*vec = emb[0]
		*denseOK = *vec != nil
	}
}

// applyRerank reorders candidates by the reranker scores (index refers to the
// texts slice order). Returns false if the scores are not a usable ranking
// (wrong count / out of range) — caller then keeps RRF order.
func applyRerank(candidates []osCandidate, scores []processor.RerankScore) bool {
	if len(scores) != len(candidates) {
		return false
	}
	seen := map[int]bool{}
	for _, sc := range scores {
		if sc.Index < 0 || sc.Index >= len(candidates) || seen[sc.Index] {
			return false
		}
		seen[sc.Index] = true
		candidates[sc.Index].RerankScore = sc.Score
	}
	// Stable sort by rerank score descending; ties keep RRF order.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].RerankScore > candidates[j].RerankScore
	})
	return true
}

// rrfMerge fuses rank lists: score(d) = sum over arms 1/(k + rank_arm(d)).
// Deterministic: sort.SliceStable over first-seen insertion order (dense arm
// is iterated first), so equal RRF scores keep dense-first, then BM25 order.
func rrfMerge(arms [][]osHit, limit int) []osCandidate {
	byID := map[string]*osCandidate{}
	var order []*osCandidate
	for _, hits := range arms {
		for rk, h := range hits {
			c, ok := byID[h.ID]
			if !ok {
				c = &osCandidate{ID: h.ID, Text: h.Text, DocumentID: h.DocumentID, Locator: h.Locator, SectionTitles: h.SectionTitles}
				byID[h.ID] = c
				order = append(order, c)
			}
			c.RRFScore += 1.0 / (rrfK + float64(rk+1))
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return order[i].RRFScore > order[j].RRFScore
	})
	out := make([]osCandidate, 0, len(order))
	for _, c := range order {
		out = append(out, *c)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// --- OpenSearch client -----------------------------------------------------

type osClient struct {
	base string
	user string
	pass string
	hc   *http.Client
}

func newOSClient(base, user, pass string) *osClient {
	return &osClient{
		base: strings.TrimRight(base, "/"),
		user: user,
		pass: pass,
		hc:   &http.Client{Timeout: 10 * time.Second},
	}
}

// osHit is one hit from an OS query.
type osHit struct {
	ID            string          `json:"-"`
	Text          string          `json:"text"`
	DocumentID    string          `json:"document_id"`
	Locator       json.RawMessage `json:"locator"`
	SectionTitles []string        `json:"section_titles"`
}

// osCandidate is a fused candidate.
type osCandidate struct {
	ID            string
	Text          string
	DocumentID    string
	Locator       json.RawMessage
	SectionTitles []string
	RRFScore      float64
	RerankScore   float64
}

func (c *osClient) knn(ctx context.Context, vec []float32, k int, f *Filters) ([]osHit, error) {
	query := map[string]any{
		"knn": map[string]any{
			"embedding": map[string]any{"vector": vec, "k": k},
		},
	}
	if f != nil && len(f.DocumentIDs) > 0 {
		query = map[string]any{
			"bool": map[string]any{
				"must":   []any{query},
				"filter": []any{map[string]any{"terms": map[string]any{"document_id": f.DocumentIDs}}},
			},
		}
	}
	return c.search(ctx, k, query)
}

// sparseTopK bounds how many query tokens feed the rank_feature bool-should:
// rank_feature queries carry no per-term weight, so query-side importance is
// expressed by picking the strongest tokens (BGE-M3 lexical weights are heavy
// -tailed). R7 tunes this.
const sparseTopK = 64

// sparse queries the learned-lexical arm (R5 #135): bool-should of
// rank_feature queries, one per top-K query token; each term's score is the
// saturation of the DOC's stored weight (the learned importance).
func (c *osClient) sparse(ctx context.Context, weights map[string]float64, size int, f *Filters) ([]osHit, error) {
	type tw struct {
		t string
		w float64
	}
	toks := make([]tw, 0, len(weights))
	for t, w := range weights {
		if w > 0 {
			toks = append(toks, tw{t, w})
		}
	}
	sort.Slice(toks, func(i, j int) bool {
		if toks[i].w != toks[j].w {
			return toks[i].w > toks[j].w
		}
		return toks[i].t < toks[j].t // deterministic order for ties
	})
	if len(toks) > sparseTopK {
		toks = toks[:sparseTopK]
	}
	should := make([]any, 0, len(toks))
	for _, t := range toks {
		should = append(should, map[string]any{
			"rank_feature": map[string]any{"field": "sparse." + t.t},
		})
	}
	if len(should) == 0 {
		return nil, nil
	}
	query := map[string]any{"bool": map[string]any{"should": should}}
	if f != nil && len(f.DocumentIDs) > 0 {
		query = map[string]any{
			"bool": map[string]any{
				"should": should,
				"filter": []any{map[string]any{"terms": map[string]any{"document_id": f.DocumentIDs}}},
			},
		}
	}
	return c.search(ctx, size, query)
}

func (c *osClient) bm25(ctx context.Context, q string, size int, f *Filters) ([]osHit, error) {
	query := map[string]any{
		"match": map[string]any{"text": q},
	}
	if f != nil && len(f.DocumentIDs) > 0 {
		query = map[string]any{
			"bool": map[string]any{
				"must":   []any{query},
				"filter": []any{map[string]any{"terms": map[string]any{"document_id": f.DocumentIDs}}},
			},
		}
	}
	return c.search(ctx, size, query)
}

func (c *osClient) search(ctx context.Context, size int, query map[string]any) ([]osHit, error) {
	body := map[string]any{
		"size":    size,
		"query":   query,
		"_source": []string{"text", "document_id", "locator", "section_titles"},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := c.base + "/" + IndexName + "/_search"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		hreq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass)))
	}
	hres, err := c.hc.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer hres.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(hres.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if hres.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opensearch %s: status %d: %s", url, hres.StatusCode, truncateChars(string(rb), 300))
	}
	var parsed struct {
		Hits struct {
			Hits []struct {
				ID     string `json:"_id"`
				Source osHit  `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return nil, fmt.Errorf("opensearch decode: %w", err)
	}
	hits := make([]osHit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		h.Source.ID = h.ID
		hits = append(hits, h.Source)
	}
	return hits, nil
}

// --- helpers ---------------------------------------------------------------

// ErrBadRequest is a 4xx the API layer maps directly.
type ErrBadRequest string

func (e ErrBadRequest) Error() string { return string(e) }

func msSince(t0 time.Time) int64 { return time.Since(t0).Milliseconds() }

func truncateChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Rune-safe: back off to the last rune boundary <= n bytes so a
	// multibyte char is never split mid-sequence.
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// sourceFor builds the Source block; missing metadata degrades to doc ID only.
func sourceFor(m repo.DocumentMeta, docID string) Source {
	return Source{
		Book:      m.Title,
		Authors:   m.Authors,
		Year:      m.Year,
		Publisher: m.Publisher,
		DocID:     docID,
	}
}

// locatorView renders the stored locator into the human form (issue Ziel 5).
func locatorView(raw json.RawMessage, section []string) LocatorView {
	var loc struct {
		Type              string `json:"type"`
		PageLabelStart    string `json:"page_label_start"`
		PageLabelEnd      string `json:"page_label_end"`
		PhysicalPageStart *int   `json:"physical_page_start"`
		PhysicalPageEnd   *int   `json:"physical_page_end"`
		CFIStart          string `json:"cfi_start"`
	}
	_ = json.Unmarshal(raw, &loc)

	chapter := ""
	if len(section) > 0 {
		chapter = section[len(section)-1]
	}

	switch loc.Type {
	case "epub_cfi":
		return LocatorView{
			Kind:    "epub_cfi",
			Label:   chapter,
			Chapter: chapter,
			CFI:     cfiShort(loc.CFIStart),
		}
	case "page_span":
		label := loc.PageLabelStart
		if label == "" && loc.PhysicalPageStart != nil {
			label = fmt.Sprintf("%d", *loc.PhysicalPageStart+1) // physical is 0-based
		}
		if loc.PageLabelEnd != "" && loc.PageLabelEnd != loc.PageLabelStart {
			label += "-" + loc.PageLabelEnd
		}
		switch {
		case label != "" && chapter != "":
			label = chapter + " · S. " + label
		case label != "":
			label = "S. " + label
		default:
			label = chapter // no page info: bare chapter, never a dangling "S. "
		}
		return LocatorView{Kind: "page", Label: label, Chapter: chapter}
	default:
		if chapter != "" {
			return LocatorView{Kind: "none", Label: chapter, Chapter: chapter}
		}
		return LocatorView{Kind: "none"}
	}
}

// cfiShort reduces an epubcfi(/6/4!/4/10,/1:0) to its /6/4!/4/10 core.
func cfiShort(cfi string) string {
	s := strings.TrimPrefix(cfi, "epubcfi(")
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, ")")
}
