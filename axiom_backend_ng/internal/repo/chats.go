package repo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
)

// Chat matches the subset of chats fields the API returns.
type Chat struct {
	ID              uuid.UUID       `json:"id"`
	UserID          int32           `json:"user_id"`
	DocumentGroupID *uuid.UUID      `json:"document_group_id,omitempty"`
	Title           string          `json:"title"`
	ChatType        string          `json:"chat_type"`
	Settings        json.RawMessage `json:"settings,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// Message matches the messages table.
type Message struct {
	ID        uuid.UUID       `json:"id"`
	ChatID    uuid.UUID       `json:"chat_id"`
	Content   string          `json:"content"`
	Role      string          `json:"role"`
	Sources   json.RawMessage `json:"sources,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// MissionSummary is a trimmed Mission shape for list responses.
type MissionSummary struct {
	ID                   uuid.UUID `json:"id"`
	ChatID               uuid.UUID `json:"chat_id"`
	UserRequest          string    `json:"user_request"`
	Status               string    `json:"status"`
	CurrentReportVersion int32     `json:"current_report_version"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// Chats owns chat + message CRUD.
type Chats struct{ gdb *gorm.DB }

// NewChats wires the repo to the DB.
func NewChats(gdb *gorm.DB) *Chats { return &Chats{gdb: gdb} }

// Create inserts a chat with the given title and optional chat_type.
func (c *Chats) Create(ctx context.Context, userID int32, title, chatType string) (Chat, error) {
	if chatType == "" {
		chatType = "research"
	}
	now := time.Now().UTC()
	m := models.Chat{
		ID:        uuid.New(),
		UserID:    userID,
		Title:     title,
		ChatType:  chatType,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := c.gdb.WithContext(ctx).Create(&m).Error; err != nil {
		return Chat{}, err
	}
	return chatFromModel(m), nil
}

// Get returns a chat by id scoped to userID (404 on mismatch).
func (c *Chats) Get(ctx context.Context, userID int32, id uuid.UUID) (Chat, error) {
	var m models.Chat
	err := c.gdb.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&m).Error
	if err != nil {
		return Chat{}, mapErr(err)
	}
	return chatFromModel(m), nil
}

// Delete removes a chat owned by userID.
func (c *Chats) Delete(ctx context.Context, userID int32, id uuid.UUID) error {
	res := c.gdb.WithContext(ctx).
		Where("id = ? AND user_id = ?", id, userID).
		Delete(&models.Chat{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateTitle renames a chat.
func (c *Chats) UpdateTitle(ctx context.Context, userID int32, id uuid.UUID, title string) error {
	res := c.gdb.WithContext(ctx).Model(&models.Chat{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]any{"title": title, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSettings replaces the chats.settings JSONB blob.
func (c *Chats) UpdateSettings(ctx context.Context, userID int32, id uuid.UUID, settings json.RawMessage) error {
	res := c.gdb.WithContext(ctx).Model(&models.Chat{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]any{"settings": []byte(settings), "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListOptions controls pagination + filtering for List.
type ListOptions struct {
	Page     int
	PageSize int
	ChatType string
	Search   string
}

// Paginated bundles chat rows with the cursor metadata the frontend expects.
type Paginated struct {
	Items      []Chat `json:"items"`
	Total      int64  `json:"total"`
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
	TotalPages int    `json:"total_pages"`
}

// List returns a page of chats for userID. page is 1-indexed.
func (c *Chats) List(ctx context.Context, userID int32, opt ListOptions) (Paginated, error) {
	if opt.Page < 1 {
		opt.Page = 1
	}
	if opt.PageSize < 1 || opt.PageSize > 200 {
		opt.PageSize = 50
	}

	q := c.gdb.WithContext(ctx).Model(&models.Chat{}).Where("user_id = ?", userID)
	if opt.ChatType != "" {
		q = q.Where("chat_type = ?", opt.ChatType)
	}
	if opt.Search != "" {
		q = q.Where("title ILIKE ?", "%"+opt.Search+"%")
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return Paginated{}, err
	}

	var rows []models.Chat
	err := q.Order("updated_at DESC").
		Limit(opt.PageSize).
		Offset((opt.Page - 1) * opt.PageSize).
		Find(&rows).Error
	if err != nil {
		return Paginated{}, err
	}

	items := make([]Chat, 0, len(rows))
	for _, r := range rows {
		items = append(items, chatFromModel(r))
	}
	totalPages := 0
	if opt.PageSize > 0 {
		totalPages = int((total + int64(opt.PageSize) - 1) / int64(opt.PageSize))
	}
	return Paginated{
		Items:      items,
		Total:      total,
		Page:       opt.Page,
		PageSize:   opt.PageSize,
		TotalPages: totalPages,
	}, nil
}

// ListMessages returns all messages for a chat, oldest first.
func (c *Chats) ListMessages(ctx context.Context, userID int32, chatID uuid.UUID) ([]Message, error) {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return nil, err
	}
	var rows []models.Message
	err := c.gdb.WithContext(ctx).
		Where("chat_id = ?", chatID).
		Order("created_at ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, messageFromModel(r))
	}
	return out, nil
}

// AppendMessage inserts a new message after verifying chat ownership.
func (c *Chats) AppendMessage(ctx context.Context, userID int32, chatID uuid.UUID, role, content string, sources json.RawMessage) (Message, error) {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return Message{}, err
	}
	m := models.Message{
		ID:        uuid.New(),
		ChatID:    chatID,
		Content:   content,
		Role:      role,
		Sources:   []byte(sources),
		CreatedAt: time.Now().UTC(),
	}
	if err := c.gdb.WithContext(ctx).Create(&m).Error; err != nil {
		return Message{}, err
	}
	return messageFromModel(m), nil
}

// DeleteMessage removes a single message owned by userID.
func (c *Chats) DeleteMessage(ctx context.Context, userID int32, chatID, msgID uuid.UUID) error {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return err
	}
	res := c.gdb.WithContext(ctx).
		Where("id = ? AND chat_id = ?", msgID, chatID).
		Delete(&models.Message{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearMessages wipes all messages in a chat.
func (c *Chats) ClearMessages(ctx context.Context, userID int32, chatID uuid.UUID) error {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return err
	}
	return c.gdb.WithContext(ctx).
		Where("chat_id = ?", chatID).
		Delete(&models.Message{}).Error
}

// ListMissions returns mission summaries for the chat.
func (c *Chats) ListMissions(ctx context.Context, userID int32, chatID uuid.UUID) ([]MissionSummary, error) {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return nil, err
	}
	var rows []models.Mission
	err := c.gdb.WithContext(ctx).
		Where("chat_id = ?", chatID).
		Order("created_at DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]MissionSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, missionFromModel(r))
	}
	return out, nil
}

func chatFromModel(m models.Chat) Chat {
	c := Chat{
		ID:        m.ID,
		UserID:    m.UserID,
		Title:     m.Title,
		ChatType:  m.ChatType,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
	if m.DocumentGroupID != nil {
		c.DocumentGroupID = m.DocumentGroupID
	}
	if len(m.Settings) > 0 {
		c.Settings = json.RawMessage(m.Settings)
	}
	return c
}

func messageFromModel(m models.Message) Message {
	msg := Message{
		ID:        m.ID,
		ChatID:    m.ChatID,
		Content:   m.Content,
		Role:      m.Role,
		CreatedAt: m.CreatedAt,
	}
	if len(m.Sources) > 0 {
		msg.Sources = json.RawMessage(m.Sources)
	}
	return msg
}

func missionFromModel(m models.Mission) MissionSummary {
	return MissionSummary{
		ID:                   m.ID,
		ChatID:               m.ChatID,
		UserRequest:          m.UserRequest,
		Status:               m.Status,
		CurrentReportVersion: m.CurrentReportVersion,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
	}
}
