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

// Deps bundles the concrete dependencies axiom-ng needs to serve the
// full API surface. All fields may be nil when only the bootstrap
// endpoints (/, /health) are required (e.g. in smoke tests).
type Deps struct {
	Auth      api.AuthDeps
	Languages api.LanguageDeps
	System    api.SystemDeps
	Dashboard api.DashboardDeps
	Settings  api.SettingsDeps
	Chats     api.ChatDeps
	// UserCtx, if set, wires the JWT-cookie middleware so authenticated
	// routes can resolve the current user.
	UserCtx UserContextConfig
}

// New constructs a Server with the bootstrap-only routes registered. See
// NewWithDeps to enable the full API surface.
func New(cfg config.Config, logger *slog.Logger) *Server {
	return NewWithDeps(cfg, logger, Deps{})
}

// NewWithDeps wires the chi router with every route axiom-ng exposes.
// Nil dep groups mean "skip this route set" — useful during bring-up.
func NewWithDeps(cfg config.Config, logger *slog.Logger, deps Deps) *Server {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(slogAccessLog(logger))
	r.Use(middleware.Recoverer)
	r.Use(CORS(nil))

	if deps.UserCtx.Signer != nil {
		r.Use(UserContext(deps.UserCtx))
	}

	r.Get("/", api.RootHandler())
	r.Get("/health", api.HealthHandler())

	r.Route("/api", func(r chi.Router) {
		// System / public read-only
		r.Get("/system/status", deps.System.Status)
		r.Get("/system/config", deps.System.Config)
		r.Get("/system/gpu-status", deps.System.GPUStatus)
		r.Get("/languages", deps.Languages.List)
		r.Get("/languages/{code}", deps.Languages.Get)

		// Auth: register + login are public. Logout/me/test-csrf/
		// change-password are authenticated; change-password +
		// test-csrf additionally require a matching CSRF token.
		r.Post("/auth/register", deps.Auth.Register)
		r.Post("/auth/login", deps.Auth.Login)

		r.Group(func(r chi.Router) {
			r.Use(RequireAuth)
			r.Post("/auth/logout", deps.Auth.Logout)
			r.Get("/auth/me", deps.Auth.Me)
			r.Get("/dashboard/stats", deps.Dashboard.StatsHandler)
			r.Get("/me/settings", deps.Settings.Get)

			// CSRF-protected subtree: mutations that rely on the
			// cookie-scoped session.
			r.Group(func(r chi.Router) {
				r.Use(CSRF)
				r.Post("/auth/test-csrf", deps.Auth.TestCSRF)
				r.Post("/auth/change-password", deps.Auth.ChangePassword)
				r.Put("/me/settings", deps.Settings.Put)

				r.Route("/chats", func(r chi.Router) {
					r.Get("/", deps.Chats.List)
					r.Post("/", deps.Chats.Create)
					r.Get("/{id}", deps.Chats.Get)
					r.Delete("/{id}", deps.Chats.Delete)
					r.Get("/{id}/title", deps.Chats.GetTitle)
					r.Put("/{id}/title", deps.Chats.UpdateTitle)
					r.Get("/{id}/messages", deps.Chats.ListMessages)
					r.Post("/{id}/messages", deps.Chats.AppendMessage)
					r.Delete("/{id}/messages", deps.Chats.ClearMessages)
					r.Delete("/{id}/messages/{msgID}", deps.Chats.DeleteMessage)
					r.Get("/{id}/missions", deps.Chats.ListMissions)
				})
			})
		})
	})

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
