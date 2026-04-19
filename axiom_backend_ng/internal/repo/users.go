// Package repo holds the typed database access layer for axiom-ng.
// Each file in this package owns a small domain slice (users, chats,
// settings, ...) using GORM on top of pgx. Handler-facing types are
// derived from the raw GORM models so the HTTP layer never imports
// GORM directly.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
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
type Users struct{ gdb *gorm.DB }

// NewUsers wires the repository to a gorm.DB.
func NewUsers(gdb *gorm.DB) *Users { return &Users{gdb: gdb} }

// GetByUsername matches on username. Returns ErrNotFound if none.
func (u *Users) GetByUsername(ctx context.Context, username string) (User, error) {
	var m models.User
	if err := u.gdb.WithContext(ctx).Where("username = ?", username).First(&m).Error; err != nil {
		return User{}, mapErr(err)
	}
	return userFromModel(m), nil
}

// GetByID fetches a user by primary key.
func (u *Users) GetByID(ctx context.Context, id int32) (User, error) {
	var m models.User
	if err := u.gdb.WithContext(ctx).Where("id = ?", id).First(&m).Error; err != nil {
		return User{}, mapErr(err)
	}
	return userFromModel(m), nil
}

// GetByAPIKey fetches a user via the api_key column.
func (u *Users) GetByAPIKey(ctx context.Context, key string) (User, error) {
	var m models.User
	if err := u.gdb.WithContext(ctx).Where("api_key = ?", key).First(&m).Error; err != nil {
		return User{}, mapErr(err)
	}
	return userFromModel(m), nil
}

// CreateInput carries the fields needed to register a new user.
// hashed_password must already be bcrypt-hashed by the caller.
type CreateInput struct {
	Username       string
	Email          string
	HashedPassword string
	IsAdmin        bool
}

// Create inserts a new user and returns the populated row.
func (u *Users) Create(ctx context.Context, in CreateInput) (User, error) {
	now := time.Now().UTC()
	m := models.User{
		Username:       in.Username,
		Email:          in.Email,
		HashedPassword: in.HashedPassword,
		IsAdmin:        in.IsAdmin,
		IsActive:       true,
		Role:           roleFor(in.IsAdmin),
		UserType:       userTypeFor(in.IsAdmin),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := u.gdb.WithContext(ctx).Create(&m).Error; err != nil {
		return User{}, err
	}
	return userFromModel(m), nil
}

// UpdatePassword sets a new bcrypt hash for the given user.
func (u *Users) UpdatePassword(ctx context.Context, id int32, hashed string) error {
	res := u.gdb.WithContext(ctx).Model(&models.User{}).
		Where("id = ?", id).
		Updates(map[string]any{"hashed_password": hashed, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSettings replaces the JSONB settings blob.
func (u *Users) UpdateSettings(ctx context.Context, id int32, settings json.RawMessage) error {
	res := u.gdb.WithContext(ctx).Model(&models.User{}).
		Where("id = ?", id).
		Updates(map[string]any{"settings": []byte(settings), "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Count returns the total number of users.
func (u *Users) Count(ctx context.Context) (int64, error) {
	var n int64
	err := u.gdb.WithContext(ctx).Model(&models.User{}).Count(&n).Error
	return n, err
}

func userFromModel(m models.User) User {
	user := User{
		ID:             m.ID,
		Username:       m.Username,
		Email:          m.Email,
		HashedPassword: m.HashedPassword,
		IsAdmin:        m.IsAdmin,
		IsActive:       m.IsActive,
		Role:           m.Role,
		UserType:       m.UserType,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}
	if m.FullName != nil {
		user.FullName = *m.FullName
	}
	if m.Location != nil {
		user.Location = *m.Location
	}
	if m.JobTitle != nil {
		user.JobTitle = *m.JobTitle
	}
	if m.Theme != nil {
		user.Theme = *m.Theme
	}
	if m.ColorScheme != nil {
		user.ColorScheme = *m.ColorScheme
	}
	if m.LanguageCode != nil {
		user.LanguageCode = *m.LanguageCode
	}
	if m.APIKey != nil {
		user.APIKey = *m.APIKey
	}
	if len(m.Settings) > 0 {
		user.Settings = json.RawMessage(m.Settings)
	}
	return user
}

// mapErr converts GORM's sentinel errors to repo-level ones so handlers
// can keep using errors.Is(err, repo.ErrNotFound).
func mapErr(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
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
