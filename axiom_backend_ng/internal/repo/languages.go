package repo

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
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
type Languages struct{ gdb *gorm.DB }

// NewLanguages wires the repo to the DB.
func NewLanguages(gdb *gorm.DB) *Languages { return &Languages{gdb: gdb} }

// List returns supported languages. When includeInactive is false only
// is_active = true rows are returned. Ordered
// (completion_percentage DESC, code ASC) to match the Python API.
func (l *Languages) List(ctx context.Context, includeInactive bool) ([]Language, error) {
	q := l.gdb.WithContext(ctx).Model(&models.SupportedLanguage{}).
		Order("completion_percentage DESC, code ASC")
	if !includeInactive {
		q = q.Where("is_active = true")
	}
	var rows []models.SupportedLanguage
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Language, 0, len(rows))
	for _, r := range rows {
		out = append(out, languageFromModel(r))
	}
	return out, nil
}

// Get returns a single language by code.
func (l *Languages) Get(ctx context.Context, code string) (Language, error) {
	var m models.SupportedLanguage
	if err := l.gdb.WithContext(ctx).Where("code = ?", code).First(&m).Error; err != nil {
		return Language{}, mapErr(err)
	}
	return languageFromModel(m), nil
}

func languageFromModel(m models.SupportedLanguage) Language {
	return Language{
		Code:                 m.Code,
		Name:                 m.Name,
		NativeName:           m.NativeName,
		IsActive:             m.IsActive,
		CompletionPercentage: m.CompletionPercentage,
		CreatedAt:            m.CreatedAt,
	}
}
