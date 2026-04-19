package api

import (
	"context"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// DashboardStore is the subset of repo.Dashboard the handler uses.
type DashboardStore interface {
	ForUser(ctx context.Context, userID int32) (repo.DashboardStats, error)
}

// DashboardDeps wires the store into the handler set.
type DashboardDeps struct {
	Stats DashboardStore
}

// Stats handles GET /api/dashboard/stats.
func (d DashboardDeps) StatsHandler(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	stats, err := d.Stats.ForUser(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard query failed")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
