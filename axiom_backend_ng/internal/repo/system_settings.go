package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SystemSettings owns the system_settings key/value JSONB store.
type SystemSettings struct{ pool *pgxpool.Pool }

// NewSystemSettings wires the repo to the pool.
func NewSystemSettings(pool *pgxpool.Pool) *SystemSettings { return &SystemSettings{pool: pool} }

// Get returns the raw JSON value stored under key, or ErrNotFound.
func (s *SystemSettings) Get(ctx context.Context, key string) (json.RawMessage, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM system_settings WHERE key = $1`, key).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return raw, nil
}

// RegistrationEnabled mirrors the Python check: if the setting is
// missing OR explicitly false the API returns 403. Treat "missing" as
// enabled to keep out-of-the-box installs open.
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
		// non-boolean value → treat as enabled, same as Python's
		// `setting.value is False` check (only literal False disables).
		return true, nil
	}
	return v, nil
}

func wrapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
