// Package models declares GORM models for the axiom schema.
//
// Source of truth for DDL is init-db/*.sql + axiom_backend/init-db/*.sql +
// axiom_backend/database/migrations/* — these structs map 1:1 onto the
// existing columns so GORM never owns migrations. All FK, index, and
// CHECK constraints live in SQL.
//
// Convention: every field that is NULL-able in SQL is modelled as a
// pointer (or a sql.Null*). Non-null fields with a SQL default are
// modelled as plain types; GORM writes whatever we put on insert, and
// the database trigger fixes updated_at.
package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// User matches the users table.
type User struct {
	ID             int32          `gorm:"primaryKey;column:id"`
	Username       string         `gorm:"column:username;uniqueIndex;not null"`
	Email          string         `gorm:"column:email;uniqueIndex;not null"`
	HashedPassword string         `gorm:"column:hashed_password;not null"`
	FullName       *string        `gorm:"column:full_name"`
	Location       *string        `gorm:"column:location"`
	JobTitle       *string        `gorm:"column:job_title"`
	Theme          *string        `gorm:"column:theme"`
	ColorScheme    *string        `gorm:"column:color_scheme"`
	LanguageCode   *string        `gorm:"column:language_code"`
	Settings       datatypes.JSON `gorm:"column:settings;type:jsonb"`
	IsAdmin        bool           `gorm:"column:is_admin;not null;default:false"`
	IsActive       bool           `gorm:"column:is_active;not null;default:true"`
	Role           string         `gorm:"column:role;not null;default:'user'"`
	UserType       string         `gorm:"column:user_type;not null;default:'individual'"`
	APIKey         *string        `gorm:"column:api_key;uniqueIndex"`
	CreatedAt      time.Time      `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt      time.Time      `gorm:"column:updated_at;autoUpdateTime:false"`
}

// TableName forces the canonical table name regardless of GORM's pluralizer.
func (User) TableName() string { return "users" }

// Chat matches the chats table.
type Chat struct {
	ID              uuid.UUID      `gorm:"type:uuid;primaryKey;column:id"`
	UserID          int32          `gorm:"column:user_id;not null;index"`
	DocumentGroupID *uuid.UUID     `gorm:"type:uuid;column:document_group_id"`
	Title           string         `gorm:"column:title"`
	ChatType        string         `gorm:"column:chat_type;not null;default:'research';index"`
	Settings        datatypes.JSON `gorm:"column:settings;type:jsonb"`
	CreatedAt       time.Time      `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt       time.Time      `gorm:"column:updated_at;autoUpdateTime:false"`
}

// TableName forces the canonical table name.
func (Chat) TableName() string { return "chats" }

// Message matches the messages table.
type Message struct {
	ID        uuid.UUID      `gorm:"type:uuid;primaryKey;column:id"`
	ChatID    uuid.UUID      `gorm:"type:uuid;column:chat_id;not null;index"`
	Content   string         `gorm:"column:content;not null"`
	Role      string         `gorm:"column:role;not null"`
	Sources   datatypes.JSON `gorm:"column:sources;type:jsonb"`
	CreatedAt time.Time      `gorm:"column:created_at;autoCreateTime:false"`
}

// TableName forces the canonical table name.
func (Message) TableName() string { return "messages" }

// Mission matches the missions table (only the fields axiom-ng reads today).
type Mission struct {
	ID                   uuid.UUID `gorm:"type:uuid;primaryKey;column:id"`
	ChatID               uuid.UUID `gorm:"type:uuid;column:chat_id;not null;index"`
	UserRequest          string    `gorm:"column:user_request"`
	Status               string    `gorm:"column:status;not null;default:'pending'"`
	CurrentReportVersion int32     `gorm:"column:current_report_version;not null;default:1"`
	CreatedAt            time.Time `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt            time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

// TableName forces the canonical table name.
func (Mission) TableName() string { return "missions" }

// SupportedLanguage matches the supported_languages table.
type SupportedLanguage struct {
	Code                 string    `gorm:"primaryKey;column:code"`
	Name                 string    `gorm:"column:name;not null"`
	NativeName           string    `gorm:"column:native_name;not null"`
	IsActive             bool      `gorm:"column:is_active;not null;default:true"`
	CompletionPercentage int32     `gorm:"column:completion_percentage;not null;default:0"`
	CreatedAt            time.Time `gorm:"column:created_at;autoCreateTime:false"`
}

// TableName forces the canonical table name.
func (SupportedLanguage) TableName() string { return "supported_languages" }

// SystemSetting matches the system_settings table.
type SystemSetting struct {
	ID        int32          `gorm:"primaryKey;column:id"`
	Key       string         `gorm:"column:key;uniqueIndex;not null"`
	Value     datatypes.JSON `gorm:"column:value;type:jsonb"`
	CreatedAt time.Time      `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt time.Time      `gorm:"column:updated_at;autoUpdateTime:false"`
}

// TableName forces the canonical table name.
func (SystemSetting) TableName() string { return "system_settings" }

// Document matches the documents table (subset — only what dashboard
// counts today).
type Document struct {
	ID     uuid.UUID `gorm:"type:uuid;primaryKey;column:id"`
	UserID int32     `gorm:"column:user_id;not null;index"`
}

// TableName forces the canonical table name.
func (Document) TableName() string { return "documents" }

// WritingSession matches the writing_sessions table (subset — only what
// dashboard counts today; full schema lands with the writing-session PR).
type WritingSession struct {
	ID     uuid.UUID `gorm:"type:uuid;primaryKey;column:id"`
	ChatID uuid.UUID `gorm:"type:uuid;column:chat_id;not null;index"`
}

// TableName forces the canonical table name.
func (WritingSession) TableName() string { return "writing_sessions" }
