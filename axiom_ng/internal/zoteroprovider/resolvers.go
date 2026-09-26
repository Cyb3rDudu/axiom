// resolvers.go — real BibliographicResolver rungs (F07, #301): Crossref
// and Open Library behind the same port the F06 fakes proved. No API keys
// required (both public endpoints; the polite-pool mailto is optional and
// unset — personal-library volume).
//
// Discipline (the ladder's invariants):
//   - exact identifier lookup BEFORE fuzzy search (DOI/ISBN direct hit
//     beats any title query — the resolver does this itself, as the port
//     doc requires),
//   - candidates carry ONLY fields the resolver actually vouches for,
//   - candidates return in DESCENDING confidence (ladder compares
//     neighbors — an unsorted list silently degrades ambiguity()),
//   - errors are classed Unavailable (transient network → retryable) so
//     the saga fails retryable, never terminal, on a resolver hiccup.
package zoteroprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
)

// resolverHTTP is the shared HTTP shape (tests inject httptest servers).
type resolverHTTP struct {
	name    string
	version string
	base    string
	client  *http.Client
}

func (r *resolverHTTP) Name() string    { return r.name }
func (r *resolverHTTP) Version() string { return r.version }

func (r *resolverHTTP) get(ctx context.Context, path string, q url.Values, out any) error {
	u := r.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, r.name+" request")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable, err, r.name+" unreachable")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
			&StatusError{Status: resp.StatusCode, Body: string(body[:min(len(body), 200)])}, r.name+" http")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassUnavailable, err, r.name+" decode")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Crossref

// CrossrefAPI is the real Crossref resolver (api.crossref.org).
type CrossrefAPI struct{ resolverHTTP }

// NewCrossref builds the Crossref resolver against its public API.
func NewCrossref(baseURL string, client *http.Client) *CrossrefAPI {
	if baseURL == "" {
		baseURL = "https://api.crossref.org"
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &CrossrefAPI{resolverHTTP{name: "crossref", version: "v1", base: strings.TrimRight(baseURL, "/"), client: client}}
}

type crWork struct {
	Title     []string `json:"title"`
	DOI       string   `json:"DOI"`
	Type      string   `json:"type"`
	Publisher string   `json:"publisher"`
	Language  string   `json:"language"`
	ISSN      []string `json:"ISSN"`
	Author    []struct {
		Given  string `json:"given"`
		Family string `json:"family"`
		Name   string `json:"name"`
	} `json:"author"`
	Issued struct {
		DateParts [][]int `json:"date-parts"`
	} `json:"issued"`
	Score float64 `json:"score"`
}

func crCandidate(w crWork, confidence float64) library.Candidate {
	fields := library.ResolvedFields{
		Title:      firstNonEmptyStr(w.Title...),
		Publisher:  w.Publisher,
		Language:   w.Language,
		DOI:        library.NormalizeDOI(w.DOI),
		RecordType: crossrefTypeToRecord(w.Type),
	}
	for _, a := range w.Author {
		switch {
		case a.Name != "":
			fields.Authors = append(fields.Authors, a.Name)
		case a.Family != "":
			fields.Authors = append(fields.Authors, strings.TrimSpace(a.Given+" "+a.Family))
		}
	}
	if y := crYear(w); y > 0 {
		fields.Year = &y
	}
	id := "crossref:" + w.DOI
	if id == "crossref:" {
		id = "crossref:" + firstNonEmptyStr(w.Title...)
	}
	return library.Candidate{CandidateID: id, Fields: fields, Confidence: confidence}
}

func crYear(w crWork) int {
	for _, dp := range w.Issued.DateParts {
		if len(dp) > 0 && dp[0] > 1000 && dp[0] < 3000 {
			return dp[0]
		}
	}
	return 0
}

// crossrefTypeToRecord maps Crossref work types onto the Library record
// vocabulary (the ladder's type conflict compares in OUR vocabulary).
var crossrefTypes = map[string]string{
	"book": "book", "monograph": "book", "edited-book": "book", "book-chapter": "bookSection",
	"journal-article": "journalArticle", "proceedings-article": "conferencePaper",
	"report": "report", "report-component": "report", "dissertation": "thesis",
	"posted-content": "report",
}

func crossrefTypeToRecord(t string) string {
	if r, ok := crossrefTypes[t]; ok {
		return r
	}
	return t
}

// Resolve implements library.BibliographicResolver: DOI exact first, then
// bibliographic fuzzy search.
func (c *CrossrefAPI) Resolve(ctx context.Context, q library.ResolveQuery) ([]library.Candidate, error) {
	if q.DOI != "" {
		var out struct {
			Message crWork `json:"message"`
		}
		if err := c.get(ctx, "/works/"+url.PathEscape(library.NormalizeDOI(q.DOI)), nil, &out); err == nil {
			return []library.Candidate{crCandidate(out.Message, 1.0)}, nil
		} else if !isStatus(err, http.StatusNotFound) {
			return nil, err
		}
		// DOI unknown to Crossref → fall through to fuzzy with the title.
	}
	if q.Title == "" {
		return nil, nil
	}
	query := url.Values{"rows": {"3"}}
	query.Set("query.bibliographic", strings.TrimSpace(q.Title+" "+strings.Join(q.Authors, " ")))
	if q.Year != nil {
		query.Set("filter", "from-pub-date:"+strconv.Itoa(*q.Year)+",until-pub-date:"+strconv.Itoa(*q.Year))
	}
	var out struct {
		Message struct {
			Items []crWork `json:"items"`
		} `json:"message"`
	}
	if err := c.get(ctx, "/works", query, &out); err != nil {
		return nil, err
	}
	var cands []library.Candidate
	for _, w := range out.Message.Items {
		// Title-token overlap ranks the hit — Crossref's own score is not
		// comparable across queries, our overlap is stable and honest.
		conf := titleSimilarity(q.Title, firstNonEmptyStr(w.Title...))
		if y := q.Year; y != nil && crYear(w) > 0 && crYear(w) != *y {
			conf -= 0.1
		}
		if conf <= 0 {
			continue
		}
		cands = append(cands, crCandidate(w, clamp01(conf)))
	}
	sortCandidatesDesc(cands)
	return cands, nil
}

// ---------------------------------------------------------------------------
// Open Library

// OpenLibraryAPI is the real Open Library resolver (openlibrary.org).
type OpenLibraryAPI struct{ resolverHTTP }

// NewOpenLibrary builds the Open Library resolver.
func NewOpenLibrary(baseURL string, client *http.Client) *OpenLibraryAPI {
	if baseURL == "" {
		baseURL = "https://openlibrary.org"
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &OpenLibraryAPI{resolverHTTP{name: "open_library", version: "v1", base: strings.TrimRight(baseURL, "/"), client: client}}
}

type olAuthor struct {
	Name string `json:"name"`
}

type olEdition struct {
	Title       string     `json:"title"`
	Subtitle    *string    `json:"subtitle"`
	Publishers  []string   `json:"publishers"`
	PublishDate string     `json:"publish_date"`
	Language    []string   `json:"languages"` // keys: /languages/ger
	Authors     []olAuthor `json:"authors"`
	Key         string     `json:"key"` // /books/ISBN...
	Type        struct {
		Key string `json:"key"`
	} `json:"type"`
}

func olCandidate(e olEdition, confidence float64) library.Candidate {
	fields := library.ResolvedFields{Title: e.Title}
	if e.Subtitle != nil && *e.Subtitle != "" {
		fields.Title = e.Title + ": " + *e.Subtitle
	}
	if len(e.Publishers) > 0 {
		fields.Publisher = e.Publishers[0]
	}
	if len(e.Language) > 0 {
		fields.Language = olLanguage(e.Language[0])
	}
	for _, a := range e.Authors {
		if a.Name != "" {
			fields.Authors = append(fields.Authors, a.Name)
		}
	}
	if y := olYear(e.PublishDate); y > 0 {
		fields.Year = &y
	}
	return library.Candidate{CandidateID: "open_library:" + strings.TrimPrefix(e.Key, "/books/"), Fields: fields, Confidence: confidence}
}

var olLanguages = map[string]string{
	"/languages/ger": "de", "/languages/eng": "en", "/languages/fre": "fr",
	"/languages/spa": "es", "/languages/ita": "it", "/languages/nld": "nl",
	"/languages/rus": "ru", "/languages/por": "pt", "/languages/pol": "pl",
}

func olLanguage(key string) string { return olLanguages[key] }

func olYear(date string) int {
	// scan all digit runs, take the first plausible year
	for _, run := range digitRuns(date) {
		if y, err := strconv.Atoi(run); err == nil && y >= 1000 && y <= 3000 {
			return y
		}
	}
	return 0
}

func digitRuns(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			cur.WriteRune(r)
		} else if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// Resolve implements library.BibliographicResolver: ISBN exact first,
// then title search.
func (o *OpenLibraryAPI) Resolve(ctx context.Context, q library.ResolveQuery) ([]library.Candidate, error) {
	if q.ISBN != "" {
		var out map[string]olEdition
		if err := o.get(ctx, "/api/books", url.Values{
			"bibkeys": {"ISBN:" + library.NormalizeISBN(q.ISBN)},
			"format":  {"json"}, "jscmd": {"data"},
		}, &out); err != nil {
			return nil, err
		}
		if len(out) > 0 {
			// Deterministic order: map iteration is random, the ladder's
			// neighbor-comparison demands stable candidate order.
			keys := make([]string, 0, len(out))
			for k := range out {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var cands []library.Candidate
			for _, k := range keys {
				cands = append(cands, olCandidate(out[k], 1.0))
			}
			return cands, nil
		}
		// ISBN unknown to Open Library: fall through to title search —
		// an unknown identifier must not end the rung early.
	}
	if q.Title == "" {
		return nil, nil
	}
	query := url.Values{"q": {strings.TrimSpace(q.Title + " " + strings.Join(q.Authors, " "))}, "limit": {"3"}, "fields": {"key,title,subtitle,publishers,publish_date,language,author_name,type"}}
	var out struct {
		Docs []struct {
			Key         string   `json:"key"`
			Title       string   `json:"title"`
			Subtitle    *string  `json:"subtitle"`
			Publishers  []string `json:"publishers"`
			PublishDate string   `json:"publish_date"`
			Language    []string `json:"language"`
			AuthorName  []string `json:"author_name"`
			Type        struct {
				Key string `json:"key"`
			} `json:"type"`
		} `json:"docs"`
	}
	if err := o.get(ctx, "/search.json", query, &out); err != nil {
		return nil, err
	}
	var cands []library.Candidate
	for _, d := range out.Docs {
		conf := titleSimilarity(q.Title, d.Title)
		if y := q.Year; y != nil && olYear(d.PublishDate) > 0 && olYear(d.PublishDate) != *y {
			conf -= 0.1
		}
		if conf <= 0 {
			continue
		}
		e := olEdition{
			Title: d.Title, Subtitle: d.Subtitle, Publishers: d.Publishers,
			PublishDate: d.PublishDate, Key: d.Key,
		}
		for _, l := range d.Language {
			e.Language = []string{"/languages/" + l}
			break
		}
		for _, a := range d.AuthorName {
			e.Authors = append(e.Authors, olAuthor{Name: a})
		}
		cands = append(cands, olCandidate(e, clamp01(conf)))
	}
	sortCandidatesDesc(cands)
	return cands, nil
}

// ---------------------------------------------------------------------------
// shared scoring

// titleSimilarity is the token-overlap score (0..1): |shared stop-free
// tokens| / |query tokens|. Stable, honest, and deliberately naive — it
// ranks candidates within one resolver; the ladder's ambiguity check
// compares neighbors.
func titleSimilarity(query, candidate string) float64 {
	qt := titleTokens(query)
	if len(qt) == 0 {
		return 0
	}
	ct := map[string]bool{}
	for _, t := range titleTokens(candidate) {
		ct[t] = true
	}
	shared := 0
	for _, t := range qt {
		if ct[t] {
			shared++
		}
	}
	return float64(shared) / float64(len(qt))
}

func titleTokens(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 0xc0)
	}) {
		switch f {
		case "the", "a", "an", "of", "and", "in", "on", "for", "der", "die", "das", "und", "ein", "eine":
			continue
		}
		out = append(out, f)
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// sortCandidatesDesc enforces the port contract (descending confidence).
func sortCandidatesDesc(c []library.Candidate) {
	sort.Slice(c, func(i, j int) bool { return c[i].Confidence > c[j].Confidence })
}

// compile-time port assertions.
var (
	_ library.BibliographicResolver = (*CrossrefAPI)(nil)
	_ library.BibliographicResolver = (*OpenLibraryAPI)(nil)
)
