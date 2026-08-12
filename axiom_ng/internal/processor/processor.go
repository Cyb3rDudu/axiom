// Package processor is a typed client for the axiom-ng document processor
// contract v1 (see docs/PROCESSOR_CONTRACT.md). axiom-ng owns all durable state;
// the processor performs compute only. The wire types below are the processor
// request/response vocabulary and deliberately mirror the contract schema.
//
// IMPORTANT: the process wire request has NO profile_hash field. profile_hash is
// snapshot IDENTITY owned by axiom-ng (stored in ingest_jobs.profile_hash and in
// the frozen input snapshot); it is never emitted to /v1/process. The dispatcher
// builds a ProcessRequest from the frozen snapshot, never reusing a struct that
// carries profile_hash at the top level.
package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ContractVersion is the contract major version this client speaks and requires.
const ContractVersion = "1.0"

// ErrCancelled is returned when a request fails because its context was
// cancelled or its deadline exceeded (as opposed to a processor-side error).
var ErrCancelled = fmt.Errorf("processor: request cancelled")

// Source is the canonical Zotero source block in a process request.
type Source struct {
	Type     string `json:"type"`
	SourceID string `json:"source_id"`
	ServerID string `json:"server_id,omitempty"`
}

// Document carries the document projection identity and the immutable
// publishable metadata snapshot (lossless, never processor-enriched).
type Document struct {
	DocumentID       string          `json:"document_id"`
	ZoteroKey        string          `json:"zotero_key"`
	ZoteroVersion    int64           `json:"zotero_version"`
	MetadataSnapshot json.RawMessage `json:"metadata_snapshot"`
}

// Attachment carries the processable file identity and facts.
type Attachment struct {
	AttachmentID  string `json:"attachment_id"`
	ZoteroKey     string `json:"zotero_key"`
	ZoteroVersion int64  `json:"zotero_version"`
	ContentType   string `json:"content_type"`
	Filename      string `json:"filename,omitempty"`
	LocalPath     string `json:"local_path"`
	ContentHash   string `json:"content_hash"`
	SizeBytes     int64  `json:"size_bytes"`
	MtimeMS       int64  `json:"mtime_ms"`
}

// Processing is the flat processing block: a profile NAME plus feature flags.
// There is intentionally NO profile_hash here.
type Processing struct {
	Profile                 string `json:"profile"`
	ForceRebuild            bool   `json:"force_rebuild"`
	LanguageHint            string `json:"language_hint,omitempty"`
	ExtractImages           bool   `json:"extract_images"`
	ComputeDenseEmbeddings  bool   `json:"compute_dense_embeddings"`
	ComputeSparseEmbeddings bool   `json:"compute_sparse_embeddings"`
	ExtractEntities         bool   `json:"extract_entities"`
	ExtractRelationships    bool   `json:"extract_relationships"`
}

// ProcessRequest is the POST /v1/process payload. It is the contract schema and
// deliberately contains no top-level profile_hash.
type ProcessRequest struct {
	ContractVersion string     `json:"contract_version"`
	JobID           string     `json:"job_id"`
	IdempotencyKey  string     `json:"idempotency_key"`
	Source          Source     `json:"source"`
	Document        Document   `json:"document"`
	Attachment      Attachment `json:"attachment"`
	Processing      Processing `json:"processing"`
}

// ProcessAccepted is the 202 response from POST /v1/process.
type ProcessAccepted struct {
	ContractVersion string `json:"contract_version"`
	JobID           string `json:"job_id"`
	Status          string `json:"status"`
	Deduplicated    bool   `json:"deduplicated"`
}

// JobStatus is the advisory status object from GET /v1/jobs/{id}.
type JobStatus struct {
	ContractVersion string    `json:"contract_version"`
	JobID           string    `json:"job_id"`
	Status          string    `json:"status"`
	Stage           string    `json:"stage"`
	Error           *JobError `json:"error,omitempty"`
}

// JobError is a stable machine-readable process failure.
type JobError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Stage     string `json:"stage"`
}

// Ack is the POST /v1/jobs/{id}/ack body.
type Ack struct {
	Persisted  bool   `json:"persisted"`
	SnapshotID string `json:"snapshot_id"`
}

// Client talks to one processor endpoint over loopback HTTP.
type Client struct {
	baseURL string
	hc      *http.Client
	maxBody int64
}

// Options configure a Client.
type Options struct {
	// BaseURL is the processor base, loopback by default.
	BaseURL string
	// ConnectTimeout bounds connection establishment.
	ConnectTimeout time.Duration
	// RequestTimeout bounds each single HTTP request/response.
	RequestTimeout time.Duration
	// MaxBody bounds the largest accepted response body in bytes.
	MaxBody int64
}

const (
	defaultMaxBody      = 512 << 20 // 512 MiB, enough for a full result payload
	defaultConnect      = 5 * time.Second
	defaultRequest      = 30 * time.Second
	defaultLoopbackBase = "http://127.0.0.1:8012"
)

// New returns a processor Client. BaseURL defaults to the loopback processor.
func New(opts Options) (*Client, error) {
	base := opts.BaseURL
	if base == "" {
		base = defaultLoopbackBase
	}
	connect := opts.ConnectTimeout
	if connect <= 0 {
		connect = defaultConnect
	}
	rq := opts.RequestTimeout
	if rq <= 0 {
		rq = defaultRequest
	}
	maxBody := opts.MaxBody
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	tr := &http.Transport{
		DialContext: (&net.Dialer{Timeout: connect}).DialContext,
		// Loopback processor: small idle pool, no proxy on the sidecar.
		Proxy:               nil,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     60 * time.Second,
	}
	return &Client{
		baseURL: strings.TrimRight(base, "/"),
		hc:      &http.Client{Transport: tr, Timeout: rq},
		maxBody: maxBody,
	}, nil
}

// StatusError is a non-2xx HTTP response from the processor.
type StatusError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("processor %s %s: status %d: %s", e.Method, e.Path, e.Code, e.Body)
}

// do performs a request, decoding into out on 2xx. On non-2xx it returns a
// *StatusError with a bounded body excerpt. Bodies are size-bounded and always
// closed. This client never auto-retries; callers decide retry safety per route.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("processor marshal %s: %w", path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("processor %s: %w: %v", path, ErrCancelled, ctx.Err())
		}
		return fmt.Errorf("processor %s: %w", path, err)
	}
	defer resp.Body.Close()

	// Read up to maxBody+1 to detect oversized bodies.
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if rerr != nil {
		return fmt.Errorf("processor %s read body: %w", path, rerr)
	}
	if int64(len(raw)) > c.maxBody {
		return fmt.Errorf("processor %s: response body exceeds %d bytes", path, c.maxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode, Body: excerpts(strings.TrimSpace(string(raw)))}
	}
	if out == nil {
		return nil
	}
	if len(raw) == 0 {
		return fmt.Errorf("processor %s: empty response body", path)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("processor %s decode: %w", path, err)
	}
	return nil
}

func excerpts(s string) string {
	if len(s) > 256 {
		return s[:256] + "..."
	}
	return s
}

// Capabilities is the processor's declared implementation and features.
type Capabilities struct {
	ContractVersions []string `json:"contract_versions"`
	Processor        struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"processor"`
	Formats  []string        `json:"formats"`
	Features map[string]bool `json:"features"`
	Models   map[string]any  `json:"models"`
	Limits   struct {
		MaxConcurrentJobs int   `json:"max_concurrent_jobs"`
		MaxSourceBytes    int64 `json:"max_source_bytes"`
	} `json:"limits"`
}

// Health returns the processor health endpoint result (empty on 2xx).
func (c *Client) Health(ctx context.Context) error {
	var out map[string]any
	return c.do(ctx, http.MethodGet, "/v1/health", nil, &out)
}

// Capabilities retrieves the processor's declared capabilities.
func (c *Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	var caps Capabilities
	if err := c.do(ctx, http.MethodGet, "/v1/capabilities", nil, &caps); err != nil {
		return nil, err
	}
	return &caps, nil
}

// SubmitProcess sends a process request and returns the acceptance result.
// The response is contract-validated: it must carry a supported contract
// version, echo the submitted job_id, a known status, and the deduplicated flag.
func (c *Client) SubmitProcess(ctx context.Context, req *ProcessRequest) (*ProcessAccepted, error) {
	var acc ProcessAccepted
	if err := c.do(ctx, http.MethodPost, "/v1/process", req, &acc); err != nil {
		return nil, err
	}
	if !contractVersionOk(acc.ContractVersion) {
		return nil, fmt.Errorf("processor /v1/process: unsupported contract_version %q", acc.ContractVersion)
	}
	if acc.JobID != req.JobID {
		return nil, fmt.Errorf("processor /v1/process: acceptance echoes job_id %q, want %q", acc.JobID, req.JobID)
	}
	if !validJobStatus(acc.Status) {
		return nil, fmt.Errorf("processor /v1/process: unknown acceptance status %q", acc.Status)
	}
	return &acc, nil
}

// validJobStatus reports whether s is one of the contract's known job states.
func validJobStatus(s string) bool {
	switch s {
	case "accepted", "running", "completed", "failed", "cancelled":
		return true
	}
	return false
}

// contractVersionOk reports whether a reported contract version is within the
// minor-compatible v1 range this client supports.
func contractVersionOk(v string) bool {
	return v == "1.0" || v == "1.1" || v == "1.2" || v == "1.3" || v == "1.4" || v == "1.5"
}

// JobStatus fetches the current advisory status of a processor job.
func (c *Client) JobStatus(ctx context.Context, jobID string) (*JobStatus, error) {
	var st JobStatus
	if err := c.do(ctx, http.MethodGet, "/v1/jobs/"+jobID, nil, &st); err != nil {
		return nil, err
	}
	if !contractVersionOk(st.ContractVersion) {
		return nil, fmt.Errorf("processor /v1/jobs/%s: unsupported contract_version %q", jobID, st.ContractVersion)
	}
	if st.JobID != jobID {
		return nil, fmt.Errorf("processor /v1/jobs/%s: status echoes job_id %q, want %q", jobID, st.JobID, jobID)
	}
	if !validJobStatus(st.Status) {
		return nil, fmt.Errorf("processor /v1/jobs/%s: unknown status %q", jobID, st.Status)
	}
	return &st, nil
}

// JobResult fetches a completed job's result payload as raw bytes (caller is the
// object for a full result, or a validator). Returns the raw body bounded.
func (c *Client) JobResult(ctx context.Context, jobID string) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.doRaw(ctx, http.MethodGet, "/v1/jobs/"+jobID+"/result", &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Artifact fetches one artifact's bytes.
func (c *Client) Artifact(ctx context.Context, jobID, ref string) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.doRaw(ctx, http.MethodGet, "/v1/jobs/"+jobID+"/artifacts/"+ref, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Cancel requests cooperative cancellation of a running job (idempotent).
func (c *Client) Cancel(ctx context.Context, jobID string) error {
	var out map[string]any
	return c.do(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/cancel", nil, &out)
}

// Ack notifies the processor that its result was durably persisted (idempotent).
func (c *Client) Ack(ctx context.Context, jobID string, ack Ack) error {
	var out map[string]any
	return c.do(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/ack", ack, &out)
}

// doRaw performs a request and writes the response body (bounded) into buf for
// arbitrary byte payloads (result, artifacts).
func (c *Client) doRaw(ctx context.Context, method, path string, buf *bytes.Buffer) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("processor %s: %w: %v", path, ErrCancelled, ctx.Err())
		}
		return fmt.Errorf("processor %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode, Body: excerpts(strings.TrimSpace(string(raw)))}
	}
	n, err := io.Copy(buf, io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return fmt.Errorf("processor %s read body: %w", path, err)
	}
	if n > c.maxBody {
		return fmt.Errorf("processor %s: response body exceeds %d bytes", path, c.maxBody)
	}
	return nil
}
