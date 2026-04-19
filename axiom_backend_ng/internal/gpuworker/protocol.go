// Package gpuworker speaks the msgpack-over-AF_UNIX protocol that the
// Python gpu_worker subprocess exposes. This is the sidecar interface
// for every ML call axiom-ng needs to make (embedding, reranking,
// entity extraction) until Go-native replacements exist.
//
// Wire format — parity with axiom_backend/ai_researcher/gpu_worker/protocol.py:
//
//	[4-byte big-endian uint32 length] [msgpack payload]
//
// Request envelope: {method: str, args: dict, id: str}
// Response envelope (success): {id, ok: true, result: any}
// Response envelope (error):   {id, ok: false, error: str, traceback: str}
//
// msgpack settings: use_bin_type=true, raw=false on the Python side.
// vmihailenco/msgpack/v5 is already configured for that interop.
package gpuworker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/vmihailenco/msgpack/v5"
)

// MaxFrameSize caps the size of any single request or response to
// keep a runaway worker from exhausting client memory. 64 MiB is
// well above real embedding payloads (~16 KiB per chunk) and image
// batches.
const MaxFrameSize = 64 << 20

// ErrRemote is returned when the worker reports an error in the
// response envelope. It carries the Python traceback for debugging.
type ErrRemote struct {
	Method    string
	Message   string
	Traceback string
}

// Error formats the remote error for Go's error-surface.
func (e *ErrRemote) Error() string {
	if e.Traceback != "" {
		return fmt.Sprintf("gpu-worker %s: %s\n%s", e.Method, e.Message, e.Traceback)
	}
	return fmt.Sprintf("gpu-worker %s: %s", e.Method, e.Message)
}

// ErrFrameTooLarge is returned when an incoming frame exceeds
// MaxFrameSize. It indicates either a misbehaving worker or a
// mismatched protocol version.
var ErrFrameTooLarge = errors.New("gpu-worker: frame exceeds MaxFrameSize")

type request struct {
	Method string         `msgpack:"method"`
	Args   map[string]any `msgpack:"args"`
	ID     string         `msgpack:"id"`
}

type response struct {
	ID        string `msgpack:"id"`
	OK        bool   `msgpack:"ok"`
	Result    any    `msgpack:"result,omitempty"`
	Error     string `msgpack:"error,omitempty"`
	Traceback string `msgpack:"traceback,omitempty"`
}

// writeFrame encodes req as msgpack and writes the length-prefixed
// frame to w. Returns the first I/O error.
func writeFrame(w io.Writer, req request) error {
	payload, err := msgpack.Marshal(req)
	if err != nil {
		return fmt.Errorf("gpu-worker: marshal request: %w", err)
	}
	if len(payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

// readFrame reads the length-prefixed frame and decodes the response.
func readFrame(r io.Reader) (response, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return response{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return response{}, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return response{}, err
	}
	var resp response
	if err := msgpack.Unmarshal(buf, &resp); err != nil {
		return response{}, fmt.Errorf("gpu-worker: unmarshal response: %w", err)
	}
	return resp, nil
}
