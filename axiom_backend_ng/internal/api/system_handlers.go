package api

import (
	"context"
	"net/http"
	"runtime"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/version"
)

// SystemDeps exposes the minimum set of dependencies for /api/system/*.
type SystemDeps struct {
	Health DBHealth
}

// DBHealth is a single-method interface the system handlers use to
// probe Postgres. Any implementation that returns nil on success is
// fine; production uses db.Ping.
type DBHealth interface {
	Ping(ctx context.Context) error
}

// Status handles GET /api/system/status — mirrors SystemStatus schema.
func (d SystemDeps) Status(w http.ResponseWriter, r *http.Request) {
	components := map[string]string{
		"database":       "unknown",
		"authentication": "healthy",
		"ai_researcher":  "pending",
	}
	dbStatus := "healthy"
	if d.Health != nil {
		if err := d.Health.Ping(r.Context()); err != nil {
			dbStatus = "unhealthy"
		}
	} else {
		dbStatus = "unknown"
	}
	components["database"] = dbStatus

	overall := "healthy"
	for _, v := range components {
		if v == "unhealthy" {
			overall = "unhealthy"
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     overall,
		"version":    version.Current().Version,
		"components": components,
		"runtime": map[string]string{
			"go_version": runtime.Version(),
			"arch":       runtime.GOARCH,
			"os":         runtime.GOOS,
		},
	})
}

// Config handles GET /api/system/config — returns a narrow,
// safe-to-expose snapshot of the running configuration.
func (d SystemDeps) Config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version.Current(),
	})
}

// GPUStatus handles GET /api/system/gpu-status. The GPU worker client
// ships in a later slice; for now we return a stub so the frontend's
// status pages do not 404.
func (d SystemDeps) GPUStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "not_connected",
		"loaded":     false,
		"pid":        0,
		"uptime_sec": 0,
		"vram_mb":    0,
	})
}
