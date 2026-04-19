package repo

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
)

// Document is the handler-facing view of a documents row. Mirrors the
// Pydantic schemas.Document shape: metadata_ is flattened into some
// top-level fields for the frontend, but the raw JSONB blob is still
// returned in `metadata` for round-tripping.
type Document struct {
	ID               uuid.UUID       `json:"id"`
	UserID           int32           `json:"user_id"`
	Filename         string          `json:"filename"`
	OriginalFilename string          `json:"original_filename"`
	Title            string          `json:"title,omitempty"`
	Authors          any             `json:"authors,omitempty"`
	PublicationYear  *int32          `json:"publication_year,omitempty"`
	Journal          string          `json:"journal,omitempty"`
	Abstract         string          `json:"abstract,omitempty"`
	Keywords         any             `json:"keywords,omitempty"`
	DOI              string          `json:"doi,omitempty"`
	DocumentType     string          `json:"document_type,omitempty"`
	Metadata         json.RawMessage `json:"metadata_,omitempty"`
	ProcessingStatus string          `json:"processing_status"`
	UploadProgress   int32           `json:"upload_progress"`
	ProcessingError  string          `json:"processing_error,omitempty"`
	FileSize         *int64          `json:"file_size,omitempty"`
	ChunkCount       int32           `json:"chunk_count"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// DocumentListOptions drives /api/documents/all pagination + filtering.
type DocumentListOptions struct {
	Page         int
	Limit        int
	Search       string
	Author       string
	Year         *int32
	Journal      string
	DocumentType string
	Status       string
	SortBy       string // created_at|updated_at|filename|processing_status
	SortOrder    string // asc|desc
	GroupID      *uuid.UUID
}

// Pagination mirrors the frontend's expected cursor envelope.
type Pagination struct {
	TotalCount  int64 `json:"total_count"`
	Page        int   `json:"page"`
	Limit       int   `json:"limit"`
	TotalPages  int   `json:"total_pages"`
	HasNext     bool  `json:"has_next"`
	HasPrevious bool  `json:"has_previous"`
}

// PaginatedDocuments is the /api/documents/all response envelope.
type PaginatedDocuments struct {
	Documents      []Document     `json:"documents"`
	Pagination     Pagination     `json:"pagination"`
	FiltersApplied map[string]any `json:"filters_applied"`
}

// Documents provides CRUD over the documents table.
type Documents struct{ gdb *gorm.DB }

// NewDocuments wires the repo to the DB.
func NewDocuments(gdb *gorm.DB) *Documents { return &Documents{gdb: gdb} }

var allowedDocSortColumns = map[string]string{
	"created_at":        "created_at",
	"updated_at":        "updated_at",
	"filename":          "filename",
	"processing_status": "processing_status",
}

// List returns a page of documents scoped to userID with optional
// JSONB-metadata filters. Matches the Python /api/documents/all query.
func (d *Documents) List(ctx context.Context, userID int32, opt DocumentListOptions) (PaginatedDocuments, error) {
	if opt.Page < 1 {
		opt.Page = 1
	}
	if opt.Limit < 1 || opt.Limit > 100 {
		opt.Limit = 20
	}
	sortColumn := allowedDocSortColumns[opt.SortBy]
	if sortColumn == "" {
		sortColumn = "created_at"
	}
	sortOrder := strings.ToLower(opt.SortOrder)
	if sortOrder != "asc" {
		sortOrder = "desc"
	}

	q := d.gdb.WithContext(ctx).Model(&models.Document{}).Where("user_id = ?", userID)

	if opt.GroupID != nil {
		q = q.Joins(`JOIN document_group_association a ON a.document_id = documents.id`).
			Where("a.document_group_id = ?", *opt.GroupID)
	}
	if opt.Search != "" {
		like := "%" + opt.Search + "%"
		q = q.Where(`
			filename ILIKE ? OR
			metadata_->>'title' ILIKE ? OR
			metadata_->>'abstract' ILIKE ?`,
			like, like, like)
	}
	if opt.Author != "" {
		q = q.Where(`metadata_->>'authors' ILIKE ?`, "%"+opt.Author+"%")
	}
	if opt.Year != nil {
		q = q.Where(`(metadata_->>'publication_year')::int = ?`, *opt.Year)
	}
	if opt.Journal != "" {
		q = q.Where(`metadata_->>'journal_or_source' ILIKE ?`, "%"+opt.Journal+"%")
	}
	if opt.DocumentType != "" {
		q = q.Where(`metadata_->>'document_type' = ?`, opt.DocumentType)
	}
	if opt.Status != "" {
		q = q.Where(`processing_status = ?`, opt.Status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return PaginatedDocuments{}, err
	}

	var rows []models.Document
	err := q.Order(sortColumn + " " + sortOrder).
		Limit(opt.Limit).
		Offset((opt.Page - 1) * opt.Limit).
		Find(&rows).Error
	if err != nil {
		return PaginatedDocuments{}, err
	}

	items := make([]Document, 0, len(rows))
	for _, r := range rows {
		items = append(items, documentFromModel(r))
	}

	totalPages := 0
	if opt.Limit > 0 {
		totalPages = int((total + int64(opt.Limit) - 1) / int64(opt.Limit))
	}
	return PaginatedDocuments{
		Documents: items,
		Pagination: Pagination{
			TotalCount:  total,
			Page:        opt.Page,
			Limit:       opt.Limit,
			TotalPages:  totalPages,
			HasNext:     opt.Page < totalPages,
			HasPrevious: opt.Page > 1,
		},
		FiltersApplied: map[string]any{
			"search":        nilIfEmpty(opt.Search),
			"author":        nilIfEmpty(opt.Author),
			"year":          opt.Year,
			"journal":       nilIfEmpty(opt.Journal),
			"document_type": nilIfEmpty(opt.DocumentType),
			"status":        nilIfEmpty(opt.Status),
			"group_id":      opt.GroupID,
		},
	}, nil
}

// ListSimple is the /api/documents/ read path: skip/limit with no filters.
func (d *Documents) ListSimple(ctx context.Context, userID int32, skip, limit int) ([]Document, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	if skip < 0 {
		skip = 0
	}
	var rows []models.Document
	err := d.gdb.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Limit(limit).
		Offset(skip).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]Document, 0, len(rows))
	for _, r := range rows {
		out = append(out, documentFromModel(r))
	}
	return out, nil
}

// Get returns a single document scoped to userID.
func (d *Documents) Get(ctx context.Context, userID int32, id uuid.UUID) (Document, error) {
	var m models.Document
	err := d.gdb.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&m).Error
	if err != nil {
		return Document{}, mapErr(err)
	}
	return documentFromModel(m), nil
}

// GetRawModel returns the internal GORM model (file_path / markdown_path).
// Used by the view handler which needs disk paths.
func (d *Documents) GetRawModel(ctx context.Context, userID int32, id uuid.UUID) (models.Document, error) {
	var m models.Document
	err := d.gdb.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&m).Error
	if err != nil {
		return models.Document{}, mapErr(err)
	}
	return m, nil
}

// Delete removes a document and cascades to chunks, images, processing
// jobs via SQL FK constraints. Association rows are cleaned up explicitly
// to be safe.
func (d *Documents) Delete(ctx context.Context, userID int32, id uuid.UUID) error {
	return d.gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND user_id = ?", id, userID).Delete(&models.Document{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Where("document_id = ?", id).
			Delete(&models.DocumentGroupAssociation{}).Error; err != nil {
			return err
		}
		return nil
	})
}

// BulkDelete removes multiple documents in one transaction. Returns
// the ids that were successfully deleted and a map of failed ids → error.
func (d *Documents) BulkDelete(ctx context.Context, userID int32, ids []uuid.UUID) ([]uuid.UUID, map[uuid.UUID]error) {
	ok := make([]uuid.UUID, 0, len(ids))
	fail := map[uuid.UUID]error{}
	for _, id := range ids {
		if err := d.Delete(ctx, userID, id); err != nil {
			fail[id] = err
		} else {
			ok = append(ok, id)
		}
	}
	return ok, fail
}

// UpdateMetadata merges the provided fields into the JSONB metadata_
// blob. Only fields present in `patch` are overwritten (matching
// Pydantic's exclude_unset behaviour).
func (d *Documents) UpdateMetadata(ctx context.Context, userID int32, id uuid.UUID, patch map[string]any) (Document, error) {
	if len(patch) == 0 {
		return d.Get(ctx, userID, id)
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return Document{}, err
	}
	res := d.gdb.WithContext(ctx).Model(&models.Document{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]any{
			"metadata_":  gorm.Expr(`COALESCE(metadata_, '{}'::jsonb) || ?::jsonb`, string(patchJSON)),
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return Document{}, res.Error
	}
	if res.RowsAffected == 0 {
		return Document{}, ErrNotFound
	}
	return d.Get(ctx, userID, id)
}

// Cancel transitions a document from 'processing' to 'cancelled'.
func (d *Documents) Cancel(ctx context.Context, userID int32, id uuid.UUID) error {
	res := d.gdb.WithContext(ctx).Model(&models.Document{}).
		Where("id = ? AND user_id = ? AND processing_status = 'processing'", id, userID).
		Updates(map[string]any{"processing_status": "cancelled", "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// QueueReprocess flips the processing status back to 'pending' so the
// background processor picks it up. The bulk handler fans this out.
// The Python doc-processor reads metadata_['reprocess_metadata']=true
// to know this is a metadata-only re-extraction rather than a full
// re-embed; we must merge that flag in or the processor will re-embed
// unnecessarily (axiom_backend/api/documents.py:1263).
func (d *Documents) QueueReprocess(ctx context.Context, userID int32, ids []uuid.UUID) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := d.gdb.WithContext(ctx).Model(&models.Document{}).
		Where("id IN ? AND user_id = ?", ids, userID).
		Updates(map[string]any{
			"processing_status": "pending",
			"upload_progress":   0,
			"metadata_":         gorm.Expr(`COALESCE(metadata_, '{}'::jsonb) || '{"reprocess_metadata": true}'::jsonb`),
			"updated_at":        time.Now().UTC(),
		})
	if res.Error != nil {
		return 0, res.Error
	}
	return int(res.RowsAffected), nil
}

// FilterOptions returns the distinct authors/years/journals/types a user
// has across their library, optionally scoped to a group.
type FilterOptions struct {
	Authors       []string `json:"authors"`
	Years         []int32  `json:"years"`
	Journals      []string `json:"journals"`
	DocumentTypes []string `json:"document_types"`
}

// FilterOptionsFor returns distinct filter values for the user. Extracts
// straight from the JSONB metadata blob.
func (d *Documents) FilterOptionsFor(ctx context.Context, userID int32, groupID *uuid.UUID) (FilterOptions, error) {
	var opts FilterOptions

	base := d.gdb.WithContext(ctx).Model(&models.Document{}).Where("user_id = ?", userID)
	if groupID != nil {
		base = base.Joins(`JOIN document_group_association a ON a.document_id = documents.id`).
			Where("a.document_group_id = ?", *groupID)
	}

	queries := []struct {
		key  string
		dest any
		sort string
	}{
		{"authors", &opts.Authors, "ASC"},
		{"journal_or_source", &opts.Journals, "ASC"},
		{"document_type", &opts.DocumentTypes, "ASC"},
	}
	for _, q := range queries {
		rows := base.Session(&gorm.Session{}).
			Select(`DISTINCT metadata_->>'` + q.key + `' AS v`).
			Where(`metadata_->>'` + q.key + `' IS NOT NULL AND metadata_->>'` + q.key + `' <> ''`).
			Order("v " + q.sort)
		if err := rows.Find(q.dest).Error; err != nil {
			return FilterOptions{}, err
		}
	}
	if err := base.Session(&gorm.Session{}).
		Select(`DISTINCT (metadata_->>'publication_year')::int AS v`).
		Where(`metadata_->>'publication_year' IS NOT NULL AND metadata_->>'publication_year' ~ '^[0-9]+$'`).
		Order("v DESC").
		Find(&opts.Years).Error; err != nil {
		return FilterOptions{}, err
	}
	if opts.Authors == nil {
		opts.Authors = []string{}
	}
	if opts.Journals == nil {
		opts.Journals = []string{}
	}
	if opts.DocumentTypes == nil {
		opts.DocumentTypes = []string{}
	}
	if opts.Years == nil {
		opts.Years = []int32{}
	}
	return opts, nil
}

// documentFromModel flattens the JSONB metadata onto the handler-facing struct.
func documentFromModel(m models.Document) Document {
	d := Document{
		ID:               m.ID,
		UserID:           m.UserID,
		Filename:         m.Filename,
		OriginalFilename: m.Filename,
		ProcessingStatus: m.ProcessingStatus,
		UploadProgress:   m.UploadProgress,
		ChunkCount:       m.ChunkCount,
		FileSize:         m.FileSize,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
	}
	if m.OriginalFilename != nil && *m.OriginalFilename != "" {
		d.OriginalFilename = *m.OriginalFilename
	}
	if m.ProcessingError != nil {
		d.ProcessingError = *m.ProcessingError
	}
	if len(m.Metadata) > 0 {
		d.Metadata = json.RawMessage(m.Metadata)
		var meta map[string]any
		if err := json.Unmarshal(m.Metadata, &meta); err == nil {
			if v, ok := meta["title"].(string); ok {
				d.Title = v
			}
			if v, ok := meta["abstract"].(string); ok {
				d.Abstract = v
			}
			if v, ok := meta["journal_or_source"].(string); ok {
				d.Journal = v
			}
			if v, ok := meta["doi"].(string); ok {
				d.DOI = v
			}
			if v, ok := meta["document_type"].(string); ok {
				d.DocumentType = v
			}
			if v, ok := meta["publication_year"]; ok && v != nil {
				switch y := v.(type) {
				case float64:
					i := int32(y)
					d.PublicationYear = &i
				case int:
					i := int32(y)
					d.PublicationYear = &i
				case string:
					// leave unset; frontend tolerates
				}
			}
			if v, ok := meta["authors"]; ok {
				d.Authors = v
			}
			if v, ok := meta["keywords"]; ok {
				d.Keywords = v
			}
		}
	}
	return d
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
