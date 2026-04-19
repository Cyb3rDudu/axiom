// Package repo holds the typed database access layer for axiom-ng.
// Each file in this package owns a small domain slice (users, chats,
// settings, ...) using the pgx/v5 pool directly. A future sqlc
// migration can swap these out without changing handler signatures.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no row matches the query. Handlers use
// errors.Is to decide between 404 and 500.
var ErrNotFound = errors.New("repo: not found")

// User is the application-level view of a users-table row. Matches the
// JSON shape returned by the Python backend's schemas.User.
type User struct {
	ID             int32           `json:"id"`
	Username       string          `json:"username"`
	Email          string          `json:"email"`
	FullName       string          `json:"full_name,omitempty"`
	Location       string          `json:"location,omitempty"`
	JobTitle       string          `json:"job_title,omitempty"`
	Theme          string          `json:"theme,omitempty"`
	ColorScheme    string          `json:"color_scheme,omitempty"`
	LanguageCode   string          `json:"language_code,omitempty"`
	Settings       json.RawMessage `json:"settings,omitempty"`
	IsAdmin        bool            `json:"is_admin"`
	IsActive       bool            `json:"is_active"`
	Role           string          `json:"role"`
	UserType       string          `json:"user_type"`
	APIKey         string          `json:"-"`
	HashedPassword string          `json:"-"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Users provides CRUD over the users table.
type Users struct {
	pool *pgxpool.Pool
}

// NewUsers wires the repository to a pool.
func NewUsers(pool *pgxpool.Pool) *Users { return &Users{pool: pool} }

const userColumns = `id, username, email, hashed_password,
	COALESCE(full_name, ''), COALESCE(location, ''), COALESCE(job_title, ''),
	COALESCE(theme, ''), COALESCE(color_scheme, ''), COALESCE(language_code, 'en'),
	settings, COALESCE(is_admin, false), COALESCE(is_active, true),
	COALESCE(role, 'user'), COALESCE(user_type, 'individual'),
	COALESCE(api_key, ''), created_at, updated_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	var settings []byte
	err := row.Scan(
		&u.ID, &u.Username, &u.Email, &u.HashedPassword,
		&u.FullName, &u.Location, &u.JobTitle,
		&u.Theme, &u.ColorScheme, &u.LanguageCode,
		&settings, &u.IsAdmin, &u.IsActive,
		&u.Role, &u.UserType,
		&u.APIKey, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	if len(settings) > 0 {
		u.Settings = settings
	}
	return u, nil
}

// GetByUsername matches on username. Returns ErrNotFound if none.
func (u *Users) GetByUsername(ctx context.Context, username string) (User, error) {
	row := u.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE username = $1`, username)
	return scanUser(row)
}

// GetByID fetches a user by primary key.
func (u *Users) GetByID(ctx context.Context, id int32) (User, error) {
	row := u.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	return scanUser(row)
}

// GetByAPIKey fetches a user via the api_key column used by the
// OpenAI-compatible Bearer auth path.
func (u *Users) GetByAPIKey(ctx context.Context, key string) (User, error) {
	row := u.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE api_key = $1`, key)
	return scanUser(row)
}

// CreateInput carries the fields needed to register a new user.
// hashed_password must already be bcrypt-hashed by the caller.
type CreateInput struct {
	Username       string
	Email          string
	HashedPassword string
	IsAdmin        bool
}

// Create inserts a new user and returns the populated row. Returns a
// raw PG duplicate-key error to the caller so handlers can map it to
// 400 without colour-coding the error string.
func (u *Users) Create(ctx context.Context, in CreateInput) (User, error) {
	row := u.pool.QueryRow(ctx, `
		INSERT INTO users (username, email, hashed_password, is_admin, is_active, role, user_type, created_at, updated_at)
		VALUES ($1, $2, $3, $4, true, $5, $6, NOW(), NOW())
		RETURNING `+userColumns,
		in.Username, in.Email, in.HashedPassword, in.IsAdmin,
		roleFor(in.IsAdmin), userTypeFor(in.IsAdmin),
	)
	return scanUser(row)
}

// UpdatePassword sets a new bcrypt hash for the given user.
func (u *Users) UpdatePassword(ctx context.Context, id int32, hashed string) error {
	cmd, err := u.pool.Exec(ctx, `UPDATE users SET hashed_password = $1, updated_at = NOW() WHERE id = $2`, hashed, id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSettings replaces the JSONB settings blob.
func (u *Users) UpdateSettings(ctx context.Context, id int32, settings json.RawMessage) error {
	cmd, err := u.pool.Exec(ctx, `UPDATE users SET settings = $1::jsonb, updated_at = NOW() WHERE id = $2`, settings, id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Count returns the total number of users. Used by bootstrap code to
// decide whether to seed a default admin.
func (u *Users) Count(ctx context.Context) (int64, error) {
	var n int64
	err := u.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func roleFor(isAdmin bool) string {
	if isAdmin {
		return "admin"
	}
	return "user"
}

func userTypeFor(isAdmin bool) string {
	if isAdmin {
		return "admin"
	}
	return "individual"
}
