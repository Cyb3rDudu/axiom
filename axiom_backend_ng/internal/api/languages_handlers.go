package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/go-chi/chi/v5"
)

// LanguageStore is the subset of repo.Languages the handlers need.
type LanguageStore interface {
	List(ctx context.Context, includeInactive bool) ([]repo.Language, error)
	Get(ctx context.Context, code string) (repo.Language, error)
}

// LanguageDeps wires the store into the handler set.
type LanguageDeps struct {
	Languages LanguageStore
}

// List handles GET /api/languages?include_inactive=true|false.
func (d LanguageDeps) List(w http.ResponseWriter, r *http.Request) {
	include := parseBool(r.URL.Query().Get("include_inactive"))
	langs, err := d.Languages.List(r.Context(), include)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "language list failed")
		return
	}
	if langs == nil {
		langs = []repo.Language{}
	}
	writeJSON(w, http.StatusOK, langs)
}

// Get handles GET /api/languages/{code}.
func (d LanguageDeps) Get(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	lang, err := d.Languages.Get(r.Context(), code)
	if errors.Is(err, repo.ErrNotFound) {
		writeError(w, http.StatusNotFound, "language not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "language fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, lang)
}
