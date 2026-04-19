package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// SettingsStore is the subset of repo.Users the settings handler needs.
type SettingsStore interface {
	GetByID(ctx context.Context, id int32) (repo.User, error)
	UpdateSettings(ctx context.Context, id int32, settings json.RawMessage) error
}

// SettingsDeps wires the store into the handler set.
type SettingsDeps struct {
	Users SettingsStore
}

// Get handles GET /api/me/settings. Returns the raw JSONB settings
// blob; the frontend parses and validates the nested shape.
func (d SettingsDeps) Get(w http.ResponseWriter, r *http.Request) {
	uid, ok := userIDFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	user, err := d.Users.GetByID(r.Context(), uid)
	if errors.Is(err, repo.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "User not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings fetch failed")
		return
	}
	out := user.Settings
	if out == nil {
		out = json.RawMessage("{}")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// Put handles PUT /api/me/settings. Accepts any JSON object and stores
// it verbatim; validation is the frontend's job (matching Python).
func (d SettingsDeps) Put(w http.ResponseWriter, r *http.Request) {
	uid, ok := userIDFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "body read failed")
		return
	}
	if !json.Valid(buf) {
		writeError(w, http.StatusBadRequest, "body is not valid JSON")
		return
	}
	if err := d.Users.UpdateSettings(r.Context(), uid, json.RawMessage(buf)); err != nil {
		writeError(w, http.StatusInternalServerError, "settings update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Settings updated"})
}
