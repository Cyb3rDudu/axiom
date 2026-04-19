// Package api holds the HTTP handlers exposed by axiom-ng.
package api

import (
	"net/http"
)

// RootHandler returns the service banner. JSON shape mirrors the Python
// backend's main.py:read_root so the frontend and any existing smoke tests
// do not notice the swap.
func RootHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "Axiom API v2.0",
			"status":  "running",
			"docs":    "/docs",
		})
	}
}

// HealthHandler returns liveness status. JSON shape mirrors the Python
// backend's main.py:health_check.
func HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	}
}
