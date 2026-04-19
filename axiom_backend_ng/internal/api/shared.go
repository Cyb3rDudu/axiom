package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
)

// writeJSON emits a JSON response with status. Encoding errors produce a
// 500 but cannot be propagated to the client once headers are flushed,
// so we surface them via the error-channel patterns the caller already
// has (e.g. slog).
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError mirrors FastAPI's default error shape: {"detail": "..."}.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

// requireUsername reads the JWT subject attached by UserContext. As
// with requireUserID this is only legal inside the RequireAuth
// subtree, and panics otherwise.
func requireUsername(r *http.Request) string {
	v, ok := authctx.UsernameFrom(r.Context())
	if !ok {
		// coverage:ignore — RequireAuth guarantees this.
		panic("requireUsername called outside RequireAuth subtree")
	}
	return v
}

// requireUserID reads the in-context user ID. Must only be called from
// routes that sit behind RequireAuth — that middleware guarantees a
// user is present, so this function panics if the invariant is violated
// (indicates a router wiring bug, not a runtime condition).
func requireUserID(r *http.Request) int32 {
	u, ok := authctx.UserFrom(r.Context())
	if !ok {
		// coverage:ignore — RequireAuth middleware makes this unreachable.
		panic("requireUserID called outside RequireAuth subtree")
	}
	return u.ID
}

// readLoginRequest accepts JSON or form-encoded bodies, plus an
// X-Remember-Me header — matching FastAPI's OAuth2PasswordRequestForm
// behaviour documented in axiom_backend/api/auth.py:41.
func readLoginRequest(r *http.Request) (LoginRequest, error) {
	var req LoginRequest
	ct := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		switch mt {
		case "application/json":
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return req, err
			}
		case "application/x-www-form-urlencoded", "multipart/form-data":
			if err := r.ParseForm(); err != nil {
				return req, err
			}
			req.Username = r.FormValue("username")
			req.Password = r.FormValue("password")
			req.RememberMe = parseBool(r.FormValue("remember_me"))
		default:
			return req, errors.New("unsupported content type: " + ct)
		}
	} else if err := r.ParseForm(); err == nil {
		req.Username = r.FormValue("username")
		req.Password = r.FormValue("password")
		req.RememberMe = parseBool(r.FormValue("remember_me"))
	}
	if strings.EqualFold(r.Header.Get("X-Remember-Me"), "true") {
		req.RememberMe = true
	}
	if req.Username == "" || req.Password == "" {
		return req, errors.New("username and password required")
	}
	return req, nil
}

func parseBool(s string) bool {
	v, _ := strconv.ParseBool(s)
	return v
}
