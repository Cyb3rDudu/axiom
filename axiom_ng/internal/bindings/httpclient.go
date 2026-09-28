// httpclient.go — the HTTP bindings (F11 #305): HTTPLibraryClient and
// HTTPStoreClient speak the internal versioned edge over the F03 DTO
// wire form. Transport failures map onto the typed error world (the
// package-doc table); GET-shaped calls retry once on retryable
// transport failures; the keyed writes fly exactly once.
package bindings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
)

// DefaultComponentTimeout is the per-request budget for non-streaming
// internal calls (AXIOM_COMPONENT_TIMEOUT overrides).
const DefaultComponentTimeout = 30 * time.Second

// retryPause spaces the single read retry.
const retryPause = 200 * time.Millisecond

// transportCeiling caps one import's buffered content on the client (the
// var form exists so the oversize sonde can lower it instead of
// allocating a gigabyte).
var transportCeiling = int64(1 << 30)

// Options configures an HTTP binding client.
type Options struct {
	// BaseURL is the component edge root (e.g. http://127.0.0.1:8211);
	// the client appends /internal/v1/<component>/….
	BaseURL string
	// Timeout bounds each non-streaming request (0 = DefaultComponentTimeout).
	Timeout time.Duration
	// Client overrides the http.Client (tests inject transports).
	Client *http.Client
	// Logger receives raw transport failures (never the wire — the leak
	// sonde's guarantee). nil = log.Default.
	Logger *log.Logger
}

// clientShared is the transport plumbing both clients use.
type clientShared struct {
	base    string
	timeout time.Duration
	hc      *http.Client
	log     *log.Logger
}

func newShared(component string, o Options) clientShared {
	if o.Timeout <= 0 {
		o.Timeout = DefaultComponentTimeout
	}
	if o.Client == nil {
		o.Client = &http.Client{}
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return clientShared{
		base:    strings.TrimSuffix(o.BaseURL, "/") + "/internal/v1/" + component,
		timeout: o.Timeout,
		hc:      o.Client,
		log:     o.Logger,
	}
}

// do runs one request; retryable once for GET-shaped calls. body must
// be re-derivable when retryGET is set (nil or a bytes reader).
func (c *clientShared) do(ctx context.Context, component contracterr.Component, op, method, path string, body []byte, headers map[string]string, retryGET bool) (*http.Response, error) {
	// attempt returns the budget's cancel WITH the response: the ctx
	// governs the body's ENTIRE lifetime (net/http), so cancel must not
	// fire before the caller finished reading — the returned body cancels
	// it at Close (cancelOnClose). Firing it here (a defer inside the
	// closure) would abort mid-body reads on every response large or
	// flushed enough to outrun the transport's buffering (review C1).
	attempt := func() (*http.Response, context.CancelFunc, error) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		// The per-request budget replaces the caller ctx for this one
		// flight (never shorter than the caller's own deadline).
		bctx, cancel := context.WithTimeout(ctx, c.timeout)
		req, err := http.NewRequestWithContext(bctx, method, c.base+path, rd)
		if err != nil {
			cancel()
			return nil, nil, contracterr.New(component, contracterr.ClassInternal, op+": build request failed")
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			cancel()
			c.log.Printf("bindings: %s %s: transport: %v", op, method, err)
			return nil, nil, transportErr(component, op, err)
		}
		return resp, cancel, nil
	}
	resp, cancel, err := attempt()
	if err == nil {
		resp.Body = &cancelOnClose{rc: resp.Body, cancel: cancel} // cancel fires at Body.Close, after the read
		return resp, nil
	}
	if !retryGET || !contracterr.Retryable(err) || ctx.Err() != nil {
		return nil, err
	}
	// ponytail: single fixed-pause retry on GETs only — enough to ride
	// out a restart gap; backoff storms belong to a future QoS pass.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(retryPause):
	}
	resp, cancel, err = attempt()
	if err == nil {
		resp.Body = &cancelOnClose{rc: resp.Body, cancel: cancel}
		return resp, nil
	}
	return nil, err
}

// decodeBody parses a 2xx JSON body into out; any other status maps to
// the typed error world. A 2xx body that fails to decode (truncated,
// foreign, cut by the size limit — or the read aborted because the
// budget expired mid-body) surfaces typed: context errors classify per
// the mapping table (deadline → Deadline, caller cancel propagated
// UNWRAPPED); every other decode failure is Internal. A mid-body
// connection RESET lands on the Internal branch by design — the
// budget never expired and the caller never canceled, so there is no
// honest class but Internal (review round 2, Finding 1).
func decodeBody(component contracterr.Component, op string, resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeErr(component, op, resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return err // the caller gave up — unwrapped, per the table
		case errors.Is(err, context.DeadlineExceeded):
			return contracterr.New(component, contracterr.ClassDeadline, op+": deadline exceeded (component budget)")
		default:
			return contracterr.Wrap(component, contracterr.ClassInternal, err, op+": decoding response body")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTPLibraryClient

// HTTPLibraryClient binds library.Library over the internal edge.
type HTTPLibraryClient struct {
	sh clientShared
}

// NewHTTPLibraryClient builds the client for o.BaseURL.
func NewHTTPLibraryClient(o Options) *HTTPLibraryClient {
	return &HTTPLibraryClient{sh: newShared("library", o)}
}

func (c *HTTPLibraryClient) GetSource(ctx context.Context, ref library.SourceRef) (library.Source, error) {
	if ref.SourceID == "" {
		return library.Source{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source ref is blank")
	}
	var out library.Source
	resp, err := c.sh.do(ctx, contracterr.ComponentLibrary, "GetSource", http.MethodGet, "/sources/"+pathEscape(ref.SourceID), nil, nil, true)
	if err != nil {
		return library.Source{}, err
	}
	return out, decodeBody(contracterr.ComponentLibrary, "GetSource", resp, &out)
}

func (c *HTTPLibraryClient) StartImport(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, error) {
	op, _, err := c.StartImportDetailed(ctx, req, content)
	return op, err
}

// StartImportDetailed is the F06 replay signal across the edge (200
// replay vs 201 fresh); the backing service decides, the status carries
// it. Content is fully buffered — the multipart body must be complete
// before the first byte flies (and the service bound already caps it).
func (c *HTTPLibraryClient) StartImportDetailed(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, bool, error) {
	// Read one byte PAST the transport ceiling: oversize content
	// fails loudly as InvalidArgument instead of silently truncating into
	// a wrong-class hash conflict (review m4).
	body, err := io.ReadAll(io.LimitReader(content, transportCeiling+1))
	if err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, err, "reading import content")
	}
	if int64(len(body)) > transportCeiling {
		return library.ImportOperation{}, false, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "import content exceeds the internal transport ceiling (1 GiB)")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("request", string(mustMarshal(req))); err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "building multipart request")
	}
	fw, err := mw.CreateFormFile("file", "rendition")
	if err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "building multipart request")
	}
	if _, err := fw.Write(body); err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "building multipart request")
	}
	if err := mw.Close(); err != nil {
		return library.ImportOperation{}, false, contracterr.Wrap(contracterr.ComponentLibrary, contracterr.ClassInternal, err, "building multipart request")
	}
	// The keyed write flies exactly once (no retryGET).
	resp, err := c.sh.do(ctx, contracterr.ComponentLibrary, "StartImport", http.MethodPost, "/imports", buf.Bytes(),
		map[string]string{"Content-Type": mw.FormDataContentType()}, false)
	if err != nil {
		return library.ImportOperation{}, false, err
	}
	var op library.ImportOperation
	if derr := decodeBody(contracterr.ComponentLibrary, "StartImport", resp, &op); derr != nil {
		return library.ImportOperation{}, false, derr
	}
	return op, resp.StatusCode == http.StatusOK, nil
}

// ConfirmImport answers the F06 decision surface over the edge (404
// from services without it maps to the contract's NotFound).
func (c *HTTPLibraryClient) ConfirmImport(ctx context.Context, importID, decisionID, candidateID string) (library.ImportOperation, error) {
	body := mustMarshal(struct {
		DecisionID  string `json:"decision_id"`
		CandidateID string `json:"candidate_id"`
	}{DecisionID: decisionID, CandidateID: candidateID})
	resp, err := c.sh.do(ctx, contracterr.ComponentLibrary, "ConfirmImport", http.MethodPost, "/imports/"+pathEscape(importID)+"/confirm", body, nil, false)
	if err != nil {
		return library.ImportOperation{}, err
	}
	var op library.ImportOperation
	return op, decodeBody(contracterr.ComponentLibrary, "ConfirmImport", resp, &op)
}

// RetryImport re-drives a retryable-failed import over the edge.
func (c *HTTPLibraryClient) RetryImport(ctx context.Context, importID string) (library.ImportOperation, error) {
	resp, err := c.sh.do(ctx, contracterr.ComponentLibrary, "RetryImport", http.MethodPost, "/imports/"+pathEscape(importID)+"/retry", nil, nil, false)
	if err != nil {
		return library.ImportOperation{}, err
	}
	var op library.ImportOperation
	return op, decodeBody(contracterr.ComponentLibrary, "RetryImport", resp, &op)
}

func (c *HTTPLibraryClient) GetImport(ctx context.Context, ref library.ImportRef) (library.ImportOperation, error) {
	if ref.ImportID == "" {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "import ref is blank")
	}
	var op library.ImportOperation
	resp, err := c.sh.do(ctx, contracterr.ComponentLibrary, "GetImport", http.MethodGet, "/imports/"+pathEscape(ref.ImportID), nil, nil, true)
	if err != nil {
		return library.ImportOperation{}, err
	}
	return op, decodeBody(contracterr.ComponentLibrary, "GetImport", resp, &op)
}

func (c *HTTPLibraryClient) OpenRendition(ctx context.Context, ticket library.ContentTicket) (io.ReadCloser, error) {
	if ticket == "" {
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "content ticket is blank")
	}
	// Streaming: the CALLER's ctx governs the ENTIRE exchange — no
	// total budget on the stream (a rendition may be large and slow; the
	// contract has the CALLER verify the hash and close the stream). The
	// per-request budget deliberately does NOT apply here.
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sh.base+"/renditions/"+pathEscape(string(ticket)), nil)
	if err != nil {
		cancel()
		return nil, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInternal, "OpenRendition: build request failed")
	}
	resp, err := c.sh.hc.Do(req)
	if err != nil {
		cancel()
		c.sh.log.Printf("bindings: OpenRendition GET: transport: %v", err)
		return nil, transportErr(contracterr.ComponentLibrary, "OpenRendition", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		cancel()
		return nil, decodeErr(contracterr.ComponentLibrary, "OpenRendition", resp)
	}
	return cancelOnClose{rc: resp.Body, cancel: cancel}, nil
}

func (c *HTTPLibraryClient) ProjectCitation(ctx context.Context, req library.CitationRequest) (library.CitationProjection, error) {
	var proj library.CitationProjection
	resp, err := c.sh.do(ctx, contracterr.ComponentLibrary, "ProjectCitation", http.MethodPost, "/citations", mustMarshal(req), nil, true)
	if err != nil {
		return library.CitationProjection{}, err
	}
	return proj, decodeBody(contracterr.ComponentLibrary, "ProjectCitation", resp, &proj)
}

// ---------------------------------------------------------------------------
// HTTPStoreClient

// HTTPStoreClient binds store.Store over the internal edge.
type HTTPStoreClient struct {
	sh clientShared
}

// NewHTTPStoreClient builds the client for o.BaseURL.
func NewHTTPStoreClient(o Options) *HTTPStoreClient {
	return &HTTPStoreClient{sh: newShared("store", o)}
}

func (c *HTTPStoreClient) IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error) {
	var job store.IngestJob
	// The keyed write flies exactly once.
	resp, err := c.sh.do(ctx, contracterr.ComponentStore, "IngestRevision", http.MethodPost, "/ingest", mustMarshal(req), nil, false)
	if err != nil {
		return store.IngestJob{}, err
	}
	return job, decodeBody(contracterr.ComponentStore, "IngestRevision", resp, &job)
}

func (c *HTTPStoreClient) Search(ctx context.Context, req store.SearchRequest) (store.SearchResult, error) {
	var res store.SearchResult
	resp, err := c.sh.do(ctx, contracterr.ComponentStore, "Search", http.MethodPost, "/search", mustMarshal(req), nil, true)
	if err != nil {
		return store.SearchResult{}, err
	}
	return res, decodeBody(contracterr.ComponentStore, "Search", resp, &res)
}

func (c *HTTPStoreClient) GetPassage(ctx context.Context, ref store.PassageRef) (store.Passage, error) {
	if ref.ChunkID == "" {
		return store.Passage{}, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "passage ref is blank")
	}
	var p store.Passage
	resp, err := c.sh.do(ctx, contracterr.ComponentStore, "GetPassage", http.MethodGet, "/passages/"+pathEscape(ref.ChunkID), nil, nil, true)
	if err != nil {
		return store.Passage{}, err
	}
	return p, decodeBody(contracterr.ComponentStore, "GetPassage", resp, &p)
}

// ---------------------------------------------------------------------------
// small helpers

func mustMarshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("null") // unreachable for the DTOs (all marshalable)
	}
	return raw
}

// cancelOnClose ties the response-body close to the request cancel so a
// dropped rendition stream cannot leak its context.
type cancelOnClose struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Read(p []byte) (int, error) { return c.rc.Read(p) }

func (c cancelOnClose) Close() error {
	err := c.rc.Close()
	c.cancel()
	return err
}

// pathEscape keeps ids with '/' or '?' inside one path segment (ids are
// opaque strings per the contracts; none legitimately contain a slash,
// but the client never assumes the server's validation).
func pathEscape(s string) string { return url.PathEscape(s) }
