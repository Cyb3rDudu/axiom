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

// Document matches the documents table (full schema).
type Document struct {
	ID                   uuid.UUID      `gorm:"type:uuid;primaryKey;column:id"`
	UserID               int32          `gorm:"column:user_id;not null;index"`
	Filename             string         `gorm:"column:filename;not null"`
	OriginalFilename     *string        `gorm:"column:original_filename"`
	Metadata             datatypes.JSON `gorm:"column:metadata_;type:jsonb"`
	ProcessingStatus     string         `gorm:"column:processing_status;not null;default:'pending';index"`
	UploadProgress       int32          `gorm:"column:upload_progress;not null;default:0"`
	ProcessingError      *string        `gorm:"column:processing_error"`
	FileSize             *int64         `gorm:"column:file_size"`
	FilePath             *string        `gorm:"column:file_path"`
	RawFilePath          *string        `gorm:"column:raw_file_path"`
	MarkdownPath         *string        `gorm:"column:markdown_path"`
	ChunkCount           int32          `gorm:"column:chunk_count;not null;default:0"`
	DenseCollectionName  string         `gorm:"column:dense_collection_name;not null;default:'documents_dense'"`
	SparseCollectionName string         `gorm:"column:sparse_collection_name;not null;default:'documents_sparse'"`
	CreatedAt            time.Time      `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt            time.Time      `gorm:"column:updated_at;autoUpdateTime:false"`
}

// TableName forces the canonical table name.
func (Document) TableName() string { return "documents" }

// DocumentGroup matches the document_groups table.
type DocumentGroup struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	UserID          int32      `gorm:"column:user_id;not null;index"`
	Name            string     `gorm:"column:name;not null"`
	Description     *string    `gorm:"column:description"`
	SourceMissionID *uuid.UUID `gorm:"type:uuid;column:source_mission_id"`
	AutoGenerated   bool       `gorm:"column:auto_generated;not null;default:false"`
	CreatedAt       time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;autoUpdateTime:false"`
}

// TableName forces the canonical table name.
func (DocumentGroup) TableName() string { return "document_groups" }

// DocumentGroupAssociation is the M2M join table between documents and groups.
type DocumentGroupAssociation struct {
	DocumentID      uuid.UUID `gorm:"type:uuid;primaryKey;column:document_id"`
	DocumentGroupID uuid.UUID `gorm:"type:uuid;primaryKey;column:document_group_id"`
}

// TableName forces the canonical table name.
func (DocumentGroupAssociation) TableName() string { return "document_group_association" }

// DocumentProcessingJob matches the document_processing_jobs table.
type DocumentProcessingJob struct {
	ID           uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	DocumentID   uuid.UUID  `gorm:"type:uuid;column:document_id;not null;index"`
	UserID       int32      `gorm:"column:user_id;not null;index"`
	JobType      string     `gorm:"column:job_type;not null;default:'process_document'"`
	Status       string     `gorm:"column:status;not null;default:'pending'"`
	Progress     int32      `gorm:"column:progress;not null;default:0"`
	ErrorMessage *string    `gorm:"column:error_message"`
	StartedAt    *time.Time `gorm:"column:started_at"`
	CompletedAt  *time.Time `gorm:"column:completed_at"`
	CreatedAt    time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;autoUpdateTime:false"`
}

// TableName forces the canonical table name.
func (DocumentProcessingJob) TableName() string { return "document_processing_jobs" }

// DocumentChunk matches the document_chunks table.
type DocumentChunk struct {
	ID              uuid.UUID      `gorm:"type:uuid;primaryKey;column:id"`
	DocID           uuid.UUID      `gorm:"type:uuid;column:doc_id;not null;index"`
	ChunkID         string         `gorm:"column:chunk_id;uniqueIndex;not null"`
	ChunkIndex      int32          `gorm:"column:chunk_index;not null"`
	ChunkText       string         `gorm:"column:chunk_text;not null"`
	DenseEmbedding  *string        `gorm:"column:dense_embedding;type:vector(1024)"`
	SparseEmbedding datatypes.JSON `gorm:"column:sparse_embedding;type:jsonb;not null;default:'{}'"`
	ChunkMetadata   datatypes.JSON `gorm:"column:chunk_metadata;type:jsonb;not null;default:'{}'"`
	CreatedAt       time.Time      `gorm:"column:created_at;autoCreateTime:false"`
}

// TableName forces the canonical table name.
func (DocumentChunk) TableName() string { return "document_chunks" }

// DocumentImage matches the document_images table.
type DocumentImage struct {
	ID             uuid.UUID      `gorm:"type:uuid;primaryKey;column:id"`
	DocID          uuid.UUID      `gorm:"type:uuid;column:doc_id;not null;index"`
	ChunkID        *string        `gorm:"column:chunk_id"`
	ImageID        string         `gorm:"column:image_id;uniqueIndex;not null"`
	ImagePath      string         `gorm:"column:image_path;not null"`
	AltText        *string        `gorm:"column:alt_text"`
	ImageEmbedding *string        `gorm:"column:image_embedding;type:vector(512)"`
	ImageMetadata  datatypes.JSON `gorm:"column:image_metadata;type:jsonb;not null;default:'{}'"`
	CreatedAt      time.Time      `gorm:"column:created_at;autoCreateTime:false"`
}

// TableName forces the canonical table name.
func (DocumentImage) TableName() string { return "document_images" }

// DocumentEntity matches the document_entities table (knowledge-graph
// layer). Created by database/migrations/add_knowledge_graph_tables.sql.
type DocumentEntity struct {
	ID             uuid.UUID      `gorm:"type:uuid;primaryKey;column:id"`
	EntityText     string         `gorm:"column:entity_text;not null"`
	EntityType     string         `gorm:"column:entity_type;not null"`
	CanonicalForm  string         `gorm:"column:canonical_form;not null"`
	Description    *string        `gorm:"column:description"`
	EntityMetadata datatypes.JSON `gorm:"column:entity_metadata;type:jsonb;not null;default:'{}'"`
	Embedding      *string        `gorm:"column:embedding;type:vector(1024)"`
	CreatedAt      time.Time      `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt      time.Time      `gorm:"column:updated_at;autoUpdateTime:false"`
}

// TableName forces the canonical table name.
func (DocumentEntity) TableName() string { return "document_entities" }

// EntityChunkOccurrence matches the entity_chunk_occurrences table.
// One row per (entity, chunk) pair; occurrence_count incremented on
// conflict so repeated ingests don't lose history.
type EntityChunkOccurrence struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey;column:id"`
	EntityID        uuid.UUID `gorm:"type:uuid;column:entity_id;not null;index"`
	ChunkID         string    `gorm:"column:chunk_id;not null;index"`
	DocID           uuid.UUID `gorm:"type:uuid;column:doc_id;not null;index"`
	OccurrenceCount int32     `gorm:"column:occurrence_count;not null;default:1"`
	ContextSnippet  *string   `gorm:"column:context_snippet"`
	PositionInChunk *int32    `gorm:"column:position_in_chunk"`
	RelevanceScore  *float64  `gorm:"column:relevance_score"`
	CreatedAt       time.Time `gorm:"column:created_at;autoCreateTime:false"`
}

// TableName forces the canonical table name.
func (EntityChunkOccurrence) TableName() string { return "entity_chunk_occurrences" }

// WritingSession matches the writing_sessions table (subset — only what
// dashboard counts today; full schema lands with the writing-session PR).
type WritingSession struct {
	ID     uuid.UUID `gorm:"type:uuid;primaryKey;column:id"`
	ChatID uuid.UUID `gorm:"type:uuid;column:chat_id;not null;index"`
}

// TableName forces the canonical table name.
func (WritingSession) TableName() string { return "writing_sessions" }
