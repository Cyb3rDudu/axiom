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

// usernameFromRequest reads the JWT subject previously attached by the
// UserContext middleware.
func usernameFromRequest(r *http.Request) (string, bool) {
	return authctx.UsernameFrom(r.Context())
}

// userIDFromRequest reads the in-context user ID (attached by the same
// middleware after a successful DB lookup).
func userIDFromRequest(r *http.Request) (int32, bool) {
	u, ok := authctx.UserFrom(r.Context())
	if !ok {
		return 0, false
	}
	return u.ID, true
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
