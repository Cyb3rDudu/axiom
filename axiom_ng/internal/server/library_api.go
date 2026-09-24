// library_api.go — the public import routes (F06, #300): the normative
// HTTP contract from the issue body, delegating to the LibraryService.
//
//	POST /api/v1/library/imports               multipart(file + request JSON) → 202 {import_id, status_url}
//	GET  /api/v1/library/imports/{id}          state, decisions, result incl. per-field provenance
//	POST /api/v1/library/imports/{id}/confirm  {decision_id, candidate_id}
//	POST /api/v1/library/imports/{id}/retry    (retry-fähige Schritte only)
//
// Idempotency: same key + same payload hash → replay (200, the SAME
// operation); same key + different payload → 409 (the contract Conflict
// class). Content validates by magic bytes ONLY — extension and client
// MIME are never sufficient. Routes register always; an unwired library
// answers 404 (sourceSecret pattern — unwired is indistinguishable from
// absent).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/go-chi/chi/v5"
)

// libraryAPI is the LibraryService surface the routes consume (the
// *library.Service satisfies it; tests may fake it).
type libraryAPI interface {
	StartImportDetailed(ctx context.Context, req library.ImportRequest, content io.Reader) (op library.ImportOperation, replayed bool, err error)
	GetImport(ctx context.Context, ref library.ImportRef) (library.ImportOperation, error)
	ConfirmImport(ctx context.Context, importID, decisionID, candidateID string) (library.ImportOperation, error)
	RetryImport(ctx context.Context, importID string) (library.ImportOperation, error)
	// MaxImportBytes bounds the multipart body (content limit + headroom).
	MaxImportBytes() int64
}

// SetLibraryAPI wires the Library import surface (nil = routes answer
// 404; the composition root decides — fake providers until F07).
func (s *Server) SetLibraryAPI(svc libraryAPI) {
	if svc == nil {
		s.librarySvc = nil
		return
	}
	s.librarySvc = svc
	s.libraryImportMaxBytes = svc.MaxImportBytes()
}

// importAccepted is the 202 body.
type importAccepted struct {
	ImportID  string `json:"import_id"`
	StatusURL string `json:"status_url"`
}

func (s *Server) handleLibraryImport(w http.ResponseWriter, r *http.Request) {
	if s.librarySvc == nil {
		http.NotFound(w, r)
		return
	}
	// Bound the whole multipart body at the content limit + headroom for
	// the request part; the content limit itself is enforced inside the
	// service against the file part bytes.
	r.Body = http.MaxBytesReader(w, r.Body, s.libraryImportMaxBytes+(4<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeContractError(w, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "multipart form: "+err.Error()))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeContractError(w, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "multipart part 'file' is required"))
		return
	}
	defer file.Close()

	reqRaw := r.FormValue("request")
	if reqRaw == "" {
		writeContractError(w, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "multipart part 'request' (JSON) is required"))
		return
	}
	var req library.ImportRequest
	if err := json.Unmarshal([]byte(reqRaw), &req); err != nil {
		writeContractError(w, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "request part is not valid JSON: "+err.Error()))
		return
	}

	op, replayed, err := s.librarySvc.StartImportDetailed(r.Context(), req, file)
	if err != nil {
		writeContractError(w, err)
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, op)
		return
	}
	writeJSON(w, http.StatusAccepted, importAccepted{
		ImportID:  op.ImportID,
		StatusURL: "/api/v1/library/imports/" + op.ImportID,
	})
}

func (s *Server) handleLibraryImportStatus(w http.ResponseWriter, r *http.Request) {
	if s.librarySvc == nil {
		http.NotFound(w, r)
		return
	}
	op, err := s.librarySvc.GetImport(r.Context(), library.ImportRef{ImportID: chi.URLParam(r, "id")})
	if err != nil {
		writeContractError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) handleLibraryImportConfirm(w http.ResponseWriter, r *http.Request) {
	if s.librarySvc == nil {
		http.NotFound(w, r)
		return
	}
	var body struct {
		DecisionID  string `json:"decision_id"`
		CandidateID string `json:"candidate_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeContractError(w, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "confirm body: "+err.Error()))
		return
	}
	op, err := s.librarySvc.ConfirmImport(r.Context(), chi.URLParam(r, "id"), body.DecisionID, body.CandidateID)
	if err != nil {
		writeContractError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) handleLibraryImportRetry(w http.ResponseWriter, r *http.Request) {
	if s.librarySvc == nil {
		http.NotFound(w, r)
		return
	}
	op, err := s.librarySvc.RetryImport(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeContractError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

// writeContractError maps the typed error world onto HTTP (the F11
// binding's exact table; the routes need it today).
func writeContractError(w http.ResponseWriter, err error) {
	class, ok := contracterr.ClassOf(err)
	if !ok {
		class = contracterr.ClassInternal
	}
	status := http.StatusInternalServerError
	switch class {
	case contracterr.ClassNotFound:
		status = http.StatusNotFound
	case contracterr.ClassInvalidArgument:
		status = http.StatusBadRequest
	case contracterr.ClassConflict:
		status = http.StatusConflict
	case contracterr.ClassUnavailable:
		status = http.StatusServiceUnavailable
	case contracterr.ClassDeadline:
		status = http.StatusGatewayTimeout
	}
	component := contracterr.ComponentLibrary
	var ce *contracterr.Error
	if errors.As(err, &ce) && ce.Component != "" {
		component = ce.Component // the error knows its origin — trust it
	}
	errBody := map[string]any{
		"component": string(component),
		"class":     string(class),
		"message":   err.Error(),
	}
	var mm *contracterr.IdempotencyMismatch
	if errors.As(err, &mm) {
		errBody["idempotency_key"] = mm.Key
	}
	writeJSON(w, status, map[string]any{"error": errBody})
}
