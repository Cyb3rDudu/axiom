// store_api.go — the Store intake route (F09 #303): POST /api/v1/store/
// ingest is revision intake's public door — the ONLY entry into the
// Store's processing pipeline (the legacy sync lane stays as the internal
// transition dual-write). Request body is the contract's
// IngestRevisionRequest; every accepted intake answers 202 + IngestJob
// (replay included — the job echo is advisory, search visibility is the
// terminal observable), contract errors map onto HTTP classes via
// writeContractError.
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
)

// StoreAPI is what the intake route needs (implemented by *store.Service).
type StoreAPI interface {
	IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error)
}

// SetStoreAPI wires the intake route (nil keeps it 503ing).
func (s *Server) SetStoreAPI(a StoreAPI) { s.storeAPI = a }

// handleStoreIngest serves POST /api/v1/store/ingest.
func (s *Server) handleStoreIngest(w http.ResponseWriter, r *http.Request) {
	if s.storeAPI == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store intake not wired"})
		return
	}
	var req store.IngestRevisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeContractError(w, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "intake body: "+err.Error()))
		return
	}
	job, err := s.storeAPI.IngestRevision(r.Context(), req)
	if err != nil {
		writeContractError(w, err)
		return
	}
	// 202 for every accepted intake (fresh or replay): the job runs
	// asynchronously; eventual Search visibility is the terminal
	// observable (the contract deliberately has no status poll).
	writeJSON(w, http.StatusAccepted, job)
}
