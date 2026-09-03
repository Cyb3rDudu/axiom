// GET /api/runners/live (Epic B · B3, #169): the REST snapshot of the derived
// per-runner live state. Serializes the same events.RunnerStateChanged values
// the WS `runners` topic streams — structural identity by construction (the
// type test pins it). Registered always; 404 when no deriver is wired (the
// sourceSecret/repair-API pattern).
package server

import (
	"net/http"
)

// SetRunnerLive wires the live-view deriver: it feeds the runners-topic WS
// snapshot and the /api/runners/live REST snapshot. Safe to call before or
// after SetWSAPI (it patches the wsServer when one exists); the REST route
// stays 404 until this is called.
func (s *Server) SetRunnerLive(v *RunnerLive) {
	s.runnerLive = v
	if s.ws != nil && v != nil {
		s.ws.runnerSnap = v
	}
}

// handleRunnersLive serves the REST snapshot of the runner live view.
// pi-lens-ignore: unusedfunc:default
func (s *Server) handleRunnersLive(w http.ResponseWriter, r *http.Request) {
	if s.runnerLive == nil {
		http.NotFound(w, r) // unwired: indistinguishable from no route
		return
	}
	writeJSON(w, http.StatusOK, s.runnerLive.Snapshot())
}
