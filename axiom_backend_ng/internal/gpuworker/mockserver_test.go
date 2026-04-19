package gpuworker_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

// mockServer speaks the same framed msgpack protocol as the Python
// gpu_worker. Tests register per-method handlers; unregistered methods
// return a clean "unknown method" error over the wire. A mockServer
// listens on a temp-dir AF_UNIX socket so it's fully isolated.
type mockServer struct {
	t        *testing.T
	Path     string
	ln       net.Listener
	handlers map[string]handlerFunc

	mu       sync.Mutex
	calls    []mockCall
	closed   bool
	wg       sync.WaitGroup
	failNext int // number of connections to drop mid-response for retry tests
}

type handlerFunc func(args map[string]any) (any, error)

type mockCall struct {
	Method string
	Args   map[string]any
}

// newMockServer boots a mock worker on a short unix socket. macOS
// caps socket paths at 104 bytes, so we sidestep t.TempDir's long
// per-test directory names by living under os.TempDir directly.
func newMockServer(t *testing.T) *mockServer {
	t.Helper()
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	path := filepath.Join(os.TempDir(), "axgw-"+hex.EncodeToString(buf[:])+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("mockServer listen: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	s := &mockServer{
		t:        t,
		Path:     path,
		ln:       ln,
		handlers: map[string]handlerFunc{},
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.Close)
	return s
}

// Handle registers a method handler. Replaces any prior handler for
// the same method.
func (s *mockServer) Handle(method string, fn handlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

// FailNextConnections drops the next N connections mid-response so
// tests can exercise the retry path.
func (s *mockServer) FailNextConnections(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

// Calls returns the recorded call log in order.
func (s *mockServer) Calls() []mockCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mockCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// Close stops the listener and waits for in-flight handlers.
func (s *mockServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.ln.Close()
	s.wg.Wait()
}

func (s *mockServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

func (s *mockServer) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()

	req, err := readMockFrame(conn)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			s.t.Logf("mockServer read: %v", err)
		}
		return
	}

	s.mu.Lock()
	s.calls = append(s.calls, mockCall{Method: req.Method, Args: req.Args})
	handler, ok := s.handlers[req.Method]
	drop := s.failNext > 0
	if drop {
		s.failNext--
	}
	s.mu.Unlock()

	if drop {
		// Simulate a broken pipe: close the connection without
		// sending a reply so the client's readFrame sees EOF.
		return
	}

	resp := mockResp{ID: req.ID}
	if !ok {
		resp.OK = false
		resp.Error = "unknown method: " + req.Method
		resp.Traceback = "mock traceback"
	} else {
		result, herr := handler(req.Args)
		if herr != nil {
			resp.OK = false
			resp.Error = herr.Error()
			resp.Traceback = "handler error"
		} else {
			resp.OK = true
			resp.Result = result
		}
	}
	if err := writeMockFrame(conn, resp); err != nil {
		s.t.Logf("mockServer write: %v", err)
	}
}

// mockReq / mockResp mirror the client-side request/response envelopes
// so tests don't depend on internal unexported types.
type mockReq struct {
	Method string         `msgpack:"method"`
	Args   map[string]any `msgpack:"args"`
	ID     string         `msgpack:"id"`
}

type mockResp struct {
	ID        string `msgpack:"id"`
	OK        bool   `msgpack:"ok"`
	Result    any    `msgpack:"result,omitempty"`
	Error     string `msgpack:"error,omitempty"`
	Traceback string `msgpack:"traceback,omitempty"`
}

func readMockFrame(r io.Reader) (mockReq, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return mockReq{}, err
	}
	n := uint32(hdr[0])<<24 | uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return mockReq{}, err
	}
	var req mockReq
	if err := msgpack.Unmarshal(buf, &req); err != nil {
		return mockReq{}, err
	}
	return req, nil
}

func writeMockFrame(w io.Writer, resp mockResp) error {
	payload, err := msgpack.Marshal(resp)
	if err != nil {
		return err
	}
	n := uint32(len(payload))
	if _, err := w.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}
