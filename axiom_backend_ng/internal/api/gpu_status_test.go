package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/gpuworker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/server"
)

type stubGPU struct {
	info gpuworker.HealthInfo
	err  error
}

func (s stubGPU) Health(context.Context) (gpuworker.HealthInfo, error) {
	return s.info, s.err
}

// systemOnlyServer builds a tiny handler stack with just the /api/system
// routes so we can poke the GPU-status handler without bringing up a
// full fixture.
func systemOnlyServer(probe api.GPUProbe, healthErr error) *httptest.Server {
	var health api.DBHealth = testHealth{err: healthErr}
	deps := server.Deps{
		System: api.SystemDeps{Health: health, GPU: probe},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	return httptest.NewServer(s.Handler())
}

type testHealth struct{ err error }

func (h testHealth) Ping(context.Context) error { return h.err }

func TestGPUStatusReturnsReadyWhenWorkerHealthy(t *testing.T) {
	t.Parallel()
	vram := 2048.5
	srv := systemOnlyServer(stubGPU{info: gpuworker.HealthInfo{
		PID: 7, UptimeSec: 9.0,
		Loaded: map[string]bool{"embedder": true},
		VRAMMB: &vram,
	}}, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/system/gpu-status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	for _, frag := range [][]byte{
		[]byte(`"status":"ready"`),
		[]byte(`"pid":7`),
		[]byte(`"embedder":true`),
		[]byte(`"vram_mb":2048.5`),
	} {
		if !bytes.Contains(body, frag) {
			t.Errorf("body missing %s: %s", frag, body)
		}
	}
}

func TestGPUStatusReturnsNotConnectedWhenNoSocket(t *testing.T) {
	t.Parallel()
	srv := systemOnlyServer(stubGPU{err: gpuworker.ErrNoSocket}, nil)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/system/gpu-status") //nolint:bodyclose // closed below
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"status":"not_connected"`)) {
		t.Errorf("expected not_connected, got %s", body)
	}
}

func TestGPUStatusSurfacesTransientErrors(t *testing.T) {
	t.Parallel()
	srv := systemOnlyServer(stubGPU{err: errors.New("boom")}, nil)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/system/gpu-status") //nolint:bodyclose // closed below
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"status":"error"`)) || !bytes.Contains(body, []byte(`boom`)) {
		t.Errorf("expected error envelope, got %s", body)
	}
}

func TestGPUStatusWithNilProbeReturnsStub(t *testing.T) {
	t.Parallel()
	srv := systemOnlyServer(nil, nil)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/system/gpu-status") //nolint:bodyclose // closed below
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"status":"not_connected"`)) {
		t.Errorf("nil probe: %s", body)
	}
}
