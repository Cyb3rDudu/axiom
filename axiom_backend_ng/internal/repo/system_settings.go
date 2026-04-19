package repo

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
)

// SystemSettings owns the system_settings key/value JSONB store.
type SystemSettings struct{ gdb *gorm.DB }

// NewSystemSettings wires the repo to the DB.
func NewSystemSettings(gdb *gorm.DB) *SystemSettings { return &SystemSettings{gdb: gdb} }

// Get returns the raw JSON value stored under key, or ErrNotFound.
func (s *SystemSettings) Get(ctx context.Context, key string) (json.RawMessage, error) {
	var m models.SystemSetting
	if err := s.gdb.WithContext(ctx).Where("key = ?", key).First(&m).Error; err != nil {
		return nil, mapErr(err)
	}
	return json.RawMessage(m.Value), nil
}

// RegistrationEnabled mirrors the Python check: missing setting → true,
// explicitly false → false. Anything else → true (only literal false
// disables, matching axiom_backend/api/auth.py:17).
func (s *SystemSettings) RegistrationEnabled(ctx context.Context) (bool, error) {
	raw, err := s.Get(ctx, "registration_enabled")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return true, nil
	}
	return v, nil
}
