package gpuworker

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestErrRemoteFormatting(t *testing.T) {
	t.Parallel()
	err := &ErrRemote{Method: "health", Message: "boom"}
	if !strings.Contains(err.Error(), "health") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("without traceback: %q", err.Error())
	}
	err.Traceback = "Traceback (most recent call last):..."
	if !strings.Contains(err.Error(), "Traceback") {
		t.Errorf("with traceback: %q", err.Error())
	}
}

func TestWriteFrameRejectsOversizedPayload(t *testing.T) {
	t.Parallel()
	// Build a request whose marshalled size is way over MaxFrameSize
	// by stuffing a giant string into args. Shortcut: bypass Marshal
	// by hitting the len check directly via manual buffer.
	huge := strings.Repeat("x", MaxFrameSize+1)
	req := request{Method: "bigger", Args: map[string]any{"blob": huge}, ID: "1"}
	var sink nopWriter
	err := writeFrame(&sink, req)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
}

type nopWriter struct{ n int }

func (n *nopWriter) Write(b []byte) (int, error) { n.n += len(b); return len(b), nil }

func TestReadFrameRejectsOversizedHeader(t *testing.T) {
	t.Parallel()
	// Build a frame with header claiming >MaxFrameSize — no payload
	// needed because we fail before attempting to read it.
	hdr := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	r := &bufReader{buf: hdr}
	_, err := readFrame(r)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
}

type bufReader struct {
	buf []byte
	i   int
}

func (b *bufReader) Read(p []byte) (int, error) {
	if b.i >= len(b.buf) {
		return 0, io.EOF
	}
	n := copy(p, b.buf[b.i:])
	b.i += n
	return n, nil
}

func TestIsRetryableClassification(t *testing.T) {
	t.Parallel()
	if isRetryable(nil) {
		t.Error("nil is not retryable")
	}
	// Remote errors are never retried — the worker rejected us
	// deterministically.
	if isRetryable(&ErrRemote{Method: "x", Message: "y"}) {
		t.Error("ErrRemote must not be retryable")
	}
	// EOF and variants are retryable.
	if !isRetryable(io.EOF) {
		t.Error("EOF should be retryable")
	}
	if !isRetryable(io.ErrUnexpectedEOF) {
		t.Error("ErrUnexpectedEOF should be retryable")
	}
}

func TestAsFloatCoerces(t *testing.T) {
	t.Parallel()
	cases := []any{
		float64(1), float32(1), int(1), int8(1), int16(1), int32(1), int64(1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1),
	}
	for _, v := range cases {
		got, ok := asFloat(v)
		if !ok || got != 1 {
			t.Errorf("asFloat(%T=%v): got %v ok=%v", v, v, got, ok)
		}
	}
	if _, ok := asFloat("string"); ok {
		t.Error("asFloat(string) should fail")
	}
}

func TestAsIntCoerces(t *testing.T) {
	t.Parallel()
	cases := []any{
		int(2), int8(2), int16(2), int32(2), int64(2),
		uint(2), uint8(2), uint16(2), uint32(2), uint64(2),
	}
	for _, v := range cases {
		got, ok := asInt(v)
		if !ok || got != 2 {
			t.Errorf("asInt(%T=%v): got %v ok=%v", v, v, got, ok)
		}
	}
	if _, ok := asInt("string"); ok {
		t.Error("asInt(string) should fail")
	}
}

func TestDecodeResultPopulatesStruct(t *testing.T) {
	t.Parallel()
	var out HealthInfo
	raw := map[string]any{"pid": int64(9), "uptime_sec": 1.5, "loaded": map[string]bool{"x": true}}
	if err := decodeResult(raw, &out); err != nil {
		t.Fatalf("decodeResult: %v", err)
	}
	if out.PID != 9 || out.UptimeSec != 1.5 || !out.Loaded["x"] {
		t.Errorf("decoded: %+v", out)
	}
}
