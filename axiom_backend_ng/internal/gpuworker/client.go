package gpuworker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// SocketEnvVars names the Python-shared env vars axiom-ng honors when
// constructing a client. Order of precedence: AXIOM_NG_GPU_WORKER_SOCKET
// (our own prefix) → AXIOM_GPU_WORKER_SOCKET (Python legacy).
var SocketEnvVars = []string{"AXIOM_NG_GPU_WORKER_SOCKET", "AXIOM_GPU_WORKER_SOCKET"}

// DefaultClientSocket is the socket path the Python gpu_worker spawns
// under `/tmp/axiom-gpu.sock` when a sibling process connects in
// client mode. Used as the last-resort fallback.
const DefaultClientSocket = "/tmp/axiom-gpu.sock"

// DefaultCallTimeout bounds every round-trip by default. Override with
// WithTimeout or by passing a deadline via context.
const DefaultCallTimeout = 30 * time.Second

// Client dials the Python gpu_worker socket on demand. Instances are
// goroutine-safe: each Call opens a fresh socket so concurrent calls
// don't trip over the worker's non-pipelined framing.
type Client struct {
	socketPath string
	timeout    time.Duration
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithTimeout overrides the default per-call timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// NewClient constructs a Client rooted at socketPath. An empty
// socketPath is allowed — calls will fail cleanly with a wrapped
// net.OpError so the caller can decide whether to treat a missing
// worker as fatal or graceful (e.g. /api/system/gpu-status returns
// "not_connected" rather than 500ing).
func NewClient(socketPath string, opts ...Option) *Client {
	c := &Client{socketPath: socketPath, timeout: DefaultCallTimeout}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SocketPathFromEnv resolves the socket path by checking the env vars
// in precedence order. Returns "" if none are set.
func SocketPathFromEnv(lookup func(string) string) string {
	if lookup == nil {
		lookup = os.Getenv
	}
	for _, key := range SocketEnvVars {
		if v := lookup(key); v != "" {
			return v
		}
	}
	return ""
}

// ResolveSocketPath falls back to the Python-parity default when no
// env var is set, mirroring axiom_backend/ai_researcher/gpu_worker/
// client.py:90 (client mode → /tmp/axiom-gpu.sock). Returns "" only
// when the caller explicitly wants opt-out behaviour.
func ResolveSocketPath(lookup func(string) string) string {
	if p := SocketPathFromEnv(lookup); p != "" {
		return p
	}
	return DefaultClientSocket
}

// SocketPath returns the socket path this client is configured to use.
// An empty string means the client is in graceful-degradation mode —
// Call always returns ErrNoSocket.
func (c *Client) SocketPath() string { return c.socketPath }

// ErrNoSocket is returned by Call when the client has no socket path
// configured. Callers typically treat this as "worker not connected"
// rather than a hard failure.
var ErrNoSocket = errors.New("gpu-worker: no socket configured")

// Call sends a single RPC and decodes the response's `result` field
// into out (if non-nil). One retry is attempted on EOF, broken-pipe,
// or reset errors — matching the Python client's retry semantics.
func (c *Client) Call(ctx context.Context, method string, args map[string]any, out any) error {
	if c.socketPath == "" {
		return ErrNoSocket
	}
	if args == nil {
		args = map[string]any{}
	}

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(c.timeout)
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		err := c.once(ctx, deadline, method, args, out)
		if err == nil {
			return nil
		}
		if !isRetryable(err) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// once dials the socket, writes the request, reads the response, and
// decodes it. Closes the socket either way.
func (c *Client) once(ctx context.Context, deadline time.Time, method string, args map[string]any, out any) error {
	id, err := newCorrelationID()
	if err != nil {
		return fmt.Errorf("gpu-worker: correlation id: %w", err)
	}
	req := request{Method: method, Args: args, ID: id}

	dialer := net.Dialer{}
	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	conn, err := dialer.DialContext(dialCtx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("gpu-worker: dial %q: %w", c.socketPath, err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("gpu-worker: set deadline: %w", err)
	}
	if err := writeFrame(conn, req); err != nil {
		return fmt.Errorf("gpu-worker: write %s: %w", method, err)
	}
	resp, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("gpu-worker: read %s: %w", method, err)
	}
	if resp.ID != id {
		return fmt.Errorf("gpu-worker: %s: id mismatch (got %q want %q)", method, resp.ID, id)
	}
	if !resp.OK {
		return &ErrRemote{Method: method, Message: resp.Error, Traceback: resp.Traceback}
	}
	if out == nil {
		return nil
	}
	return decodeResult(resp.Result, out)
}

// isRetryable reports whether err indicates a transient socket fault
// worth one retry. Protocol-level errors (e.g. remote exceptions) are
// NOT retried — the worker rejected the request deterministically.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var remote *ErrRemote
	if errors.As(err, &remote) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		// Socket file disappeared — worker restarted. One retry.
		return true
	}
	var nErr net.Error
	if errors.As(err, &nErr) {
		// net.OpError, read/write reset: all retryable.
		return true
	}
	// syscall errors (EPIPE, ECONNRESET) surface as net.OpError which
	// the check above catches. Fall-through is safe.
	return false
}

// decodeResult shoves the generic msgpack value into a typed struct
// by round-tripping through msgpack again. Cheap and keeps per-method
// decoding inside the typed wrappers (Health, EmbedQuery, ...).
func decodeResult(raw, out any) error {
	// Re-marshal and unmarshal into the caller's type — this lets us
	// accept map[string]any for dict-valued responses and still
	// populate typed Go structs without per-method codec logic.
	b, err := reencode(raw)
	if err != nil {
		return fmt.Errorf("gpu-worker: encode result: %w", err)
	}
	return unmarshal(b, out)
}

// newCorrelationID returns a 16-byte hex string. Unique-enough for
// per-call request matching.
func newCorrelationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
