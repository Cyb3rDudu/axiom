package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
)

func TestRouterRoutesHealthAndRoot(t *testing.T) {
	t.Parallel()
	s := New(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		path       string
		wantStatus int
	}{
		{"/", http.StatusOK},
		{"/health", http.StatusOK},
		{"/does-not-exist", http.StatusNotFound},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("%s: got %d, want %d", tc.path, rec.Code, tc.wantStatus)
			}
		})
	}
}

// TestRunStartsAndShutsDownOnContextCancel exercises the happy path of Run:
// the server listens, serves a real request, and shuts down cleanly when
// the parent context is cancelled.
func TestRunStartsAndShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	cfg := config.Defaults()
	cfg.Port = port
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	// Wait for the server to accept connections, then hit /health.
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	if err := waitForHTTP(url, 2*time.Second); err != nil {
		t.Fatalf("server never accepted connections: %v", err)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status: got %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned error on clean shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s after context cancel")
	}
}

// TestRunReturnsErrorOnBindFailure ensures the listen-error branch of Run
// is reached when the configured port is invalid (out of TCP range).
func TestRunReturnsErrorOnBindFailure(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.Port = 99999 // invalid TCP port — listen must fail immediately
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("expected listen error for out-of-range port")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitForHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s", timeout)
}
