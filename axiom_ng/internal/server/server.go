// Package server exposes the axiom-ng REST API. Clients talk only to this API
// and never directly to Zotero, Postgres or OpenSearch.
package server

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Checker reports reachability of a backing dependency (e.g. Zotero).
type Checker interface {
	// Ready returns nil if the dependency is healthy, else an error describing
	// why it is not.
	Ready() error
}

// Server is the axiom-ng HTTP API.
type Server struct {
	cfg    Config
	zotero Checker
	// postgres and opensearch checkers are attached once the store layers land.
	log *log.Logger
}

// Config carries server options.
type Config struct {
	// Addr is the listen address, e.g. ":8011".
	Addr string
}

// New builds a Server. zotero may be nil; the health report will mark Zotero
// as unknown in that case.
func New(addr string, zotero Checker, log *log.Logger) *Server {
	return &Server{cfg: Config{Addr: addr}, zotero: zotero, log: log}
}

// Handler returns the chi router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/api/health", s.handleHealth)

	return r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// healthResponse is returned by /api/health.
type healthResponse struct {
	OK     bool           `json:"ok"`
	Checks map[string]any `json:"checks"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	checks := map[string]any{}
	ok := true

	if s.zotero == nil {
		checks["zotero"] = "unknown"
		ok = false
	} else if err := s.zotero.Ready(); err != nil {
		checks["zotero"] = err.Error()
		ok = false
	} else {
		checks["zotero"] = "ok"
	}

	writeJSON(w, http.StatusOK, healthResponse{OK: ok, Checks: checks})
}
