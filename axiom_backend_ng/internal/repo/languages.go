package repo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Language matches supported_languages rows + Python's SupportedLanguage schema.
type Language struct {
	Code                 string    `json:"code"`
	Name                 string    `json:"name"`
	NativeName           string    `json:"native_name"`
	IsActive             bool      `json:"is_active"`
	CompletionPercentage int32     `json:"completion_percentage"`
	CreatedAt            time.Time `json:"created_at"`
}

// Languages owns supported_languages queries.
type Languages struct{ pool *pgxpool.Pool }

// NewLanguages wires the repo to the pool.
func NewLanguages(pool *pgxpool.Pool) *Languages { return &Languages{pool: pool} }

// List returns supported languages. When includeInactive is false only
// rows with is_active = true are returned. Rows are ordered
// (completion_percentage DESC, code ASC) to match the Python API.
func (l *Languages) List(ctx context.Context, includeInactive bool) ([]Language, error) {
	q := `SELECT code, name, native_name, is_active, completion_percentage, created_at
	      FROM supported_languages`
	if !includeInactive {
		q += ` WHERE is_active = true`
	}
	q += ` ORDER BY completion_percentage DESC, code ASC`

	rows, err := l.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Language
	for rows.Next() {
		var lang Language
		if err := rows.Scan(&lang.Code, &lang.Name, &lang.NativeName, &lang.IsActive, &lang.CompletionPercentage, &lang.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, lang)
	}
	return out, rows.Err()
}

// Get returns a single language by code or ErrNotFound.
func (l *Languages) Get(ctx context.Context, code string) (Language, error) {
	var lang Language
	err := l.pool.QueryRow(ctx, `
		SELECT code, name, native_name, is_active, completion_percentage, created_at
		FROM supported_languages WHERE code = $1
	`, code).Scan(&lang.Code, &lang.Name, &lang.NativeName, &lang.IsActive, &lang.CompletionPercentage, &lang.CreatedAt)
	if err != nil {
		return Language{}, wrapNotFound(err)
	}
	return lang, nil
}
