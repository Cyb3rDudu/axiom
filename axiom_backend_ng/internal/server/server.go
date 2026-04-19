// Package server wires the chi router, middleware stack, and http.Server
// lifecycle for axiom-ng.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
)

// Server owns the http.Server instance and its router.
type Server struct {
	cfg    config.Config
	logger *slog.Logger
	http   *http.Server
}

// New constructs a Server with the handlers registered. It does not start
// listening — call Run.
func New(cfg config.Config, logger *slog.Logger) *Server {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(slogAccessLog(logger))
	r.Use(middleware.Recoverer)

	r.Get("/", api.RootHandler())
	r.Get("/health", api.HealthHandler())

	return &Server{
		cfg:    cfg,
		logger: logger,
		http: &http.Server{
			Addr:              net.JoinHostPort("", strconv.Itoa(cfg.Port)),
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

// Run starts the server and blocks until ctx is cancelled or the server
// exits with an error. Shutdown has a 15s grace period.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("axiom-ng listening", slog.String("addr", s.http.Addr))
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	}
}

// Handler returns the underlying chi router, useful for tests.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

func slogAccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("dur", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}
