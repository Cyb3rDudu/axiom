package api

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/gpuworker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/version"
)

// SystemDeps exposes the minimum set of dependencies for /api/system/*.
type SystemDeps struct {
	Health DBHealth
	GPU    GPUProbe
}

// DBHealth is a single-method interface the system handlers use to
// probe Postgres. Any implementation that returns nil on success is
// fine; production uses db.Ping.
type DBHealth interface {
	Ping(ctx context.Context) error
}

// GPUProbe is the single-method interface /api/system/gpu-status uses
// to poke the Python gpu_worker. Production wires this to
// *gpuworker.Client; tests can stub it directly.
type GPUProbe interface {
	Health(ctx context.Context) (gpuworker.HealthInfo, error)
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

// GPUStatus handles GET /api/system/gpu-status. Talks to the Python
// gpu_worker over msgpack; falls back to a "not_connected" payload
// when no socket is configured or the worker is unreachable, so the
// frontend's status page renders without error.
func (d SystemDeps) GPUStatus(w http.ResponseWriter, r *http.Request) {
	if d.GPU == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "not_connected",
			"loaded":     map[string]bool{},
			"pid":        0,
			"uptime_sec": 0,
			"vram_mb":    nil,
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	h, err := d.GPU.Health(ctx)
	if err != nil {
		status := "error"
		if errors.Is(err, gpuworker.ErrNoSocket) {
			status = "not_connected"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     status,
			"loaded":     map[string]bool{},
			"pid":        0,
			"uptime_sec": 0,
			"vram_mb":    nil,
			"error":      err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ready",
		"loaded":     h.Loaded,
		"pid":        h.PID,
		"uptime_sec": h.UptimeSec,
		"vram_mb":    h.VRAMMB,
	})
}
