package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Chat matches the subset of chats fields the API returns. Nested
// messages and missions arrive via separate queries so this struct
// stays narrow.
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

// Chats owns chat + message CRUD.
type Chats struct{ pool *pgxpool.Pool }

// NewChats wires the repo to the pool.
func NewChats(pool *pgxpool.Pool) *Chats { return &Chats{pool: pool} }

const chatColumns = `id, user_id, document_group_id, COALESCE(title, ''),
	COALESCE(chat_type, 'research'), settings, created_at, updated_at`

func scanChat(row pgx.Row) (Chat, error) {
	var c Chat
	var docGroup *uuid.UUID
	var settings []byte
	err := row.Scan(&c.ID, &c.UserID, &docGroup, &c.Title, &c.ChatType, &settings, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Chat{}, ErrNotFound
		}
		return Chat{}, err
	}
	c.DocumentGroupID = docGroup
	if len(settings) > 0 {
		c.Settings = settings
	}
	return c, nil
}

// Create inserts a chat with the given title and optional chat_type
// (defaults to "research") and returns the populated row.
func (c *Chats) Create(ctx context.Context, userID int32, title, chatType string) (Chat, error) {
	if chatType == "" {
		chatType = "research"
	}
	row := c.pool.QueryRow(ctx, `
		INSERT INTO chats (id, user_id, title, chat_type, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, NOW(), NOW())
		RETURNING `+chatColumns,
		userID, title, chatType,
	)
	return scanChat(row)
}

// Get returns a chat by id scoped to userID (404 on mismatch).
func (c *Chats) Get(ctx context.Context, userID int32, id uuid.UUID) (Chat, error) {
	row := c.pool.QueryRow(ctx, `SELECT `+chatColumns+` FROM chats WHERE id = $1 AND user_id = $2`, id, userID)
	return scanChat(row)
}

// Delete removes a chat owned by userID. Silently succeeds on
// already-deleted rows to keep the client idempotent.
func (c *Chats) Delete(ctx context.Context, userID int32, id uuid.UUID) error {
	cmd, err := c.pool.Exec(ctx, `DELETE FROM chats WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateTitle renames a chat.
func (c *Chats) UpdateTitle(ctx context.Context, userID int32, id uuid.UUID, title string) error {
	cmd, err := c.pool.Exec(ctx, `UPDATE chats SET title = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, title, id, userID)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
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
	Items    []Chat `json:"items"`
	Total    int64  `json:"total"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
}

// List returns a page of chats for userID. page is 1-indexed; the
// response shape matches schemas.PaginatedChatsResponse.
func (c *Chats) List(ctx context.Context, userID int32, opt ListOptions) (Paginated, error) {
	if opt.Page < 1 {
		opt.Page = 1
	}
	if opt.PageSize < 1 || opt.PageSize > 200 {
		opt.PageSize = 50
	}

	where := "user_id = $1"
	args := []any{userID}
	if opt.ChatType != "" {
		args = append(args, opt.ChatType)
		where += " AND chat_type = $" + itoa(len(args))
	}
	if opt.Search != "" {
		args = append(args, "%"+opt.Search+"%")
		where += " AND title ILIKE $" + itoa(len(args))
	}

	var total int64
	if err := c.pool.QueryRow(ctx, `SELECT COUNT(*) FROM chats WHERE `+where, args...).Scan(&total); err != nil {
		return Paginated{}, err
	}

	args = append(args, opt.PageSize, (opt.Page-1)*opt.PageSize)
	q := `SELECT ` + chatColumns + ` FROM chats WHERE ` + where +
		` ORDER BY updated_at DESC LIMIT $` + itoa(len(args)-1) + ` OFFSET $` + itoa(len(args))

	rows, err := c.pool.Query(ctx, q, args...)
	if err != nil {
		return Paginated{}, err
	}
	defer rows.Close()

	items := make([]Chat, 0, opt.PageSize)
	for rows.Next() {
		ch, scanErr := scanChat(rows)
		if scanErr != nil {
			return Paginated{}, scanErr
		}
		items = append(items, ch)
	}
	if err := rows.Err(); err != nil {
		return Paginated{}, err
	}
	return Paginated{Items: items, Total: total, Page: opt.Page, PageSize: opt.PageSize}, nil
}

// ListMessages returns all messages for a chat, oldest first.
func (c *Chats) ListMessages(ctx context.Context, userID int32, chatID uuid.UUID) ([]Message, error) {
	// ownership check
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, chat_id, content, role, sources, created_at
		FROM messages WHERE chat_id = $1 ORDER BY created_at ASC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var sources []byte
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Content, &m.Role, &sources, &m.CreatedAt); err != nil {
			return nil, err
		}
		if len(sources) > 0 {
			m.Sources = sources
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AppendMessage inserts a new message. Ownership of chatID is checked.
func (c *Chats) AppendMessage(ctx context.Context, userID int32, chatID uuid.UUID, role, content string, sources json.RawMessage) (Message, error) {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return Message{}, err
	}
	row := c.pool.QueryRow(ctx, `
		INSERT INTO messages (id, chat_id, content, role, sources, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, NOW())
		RETURNING id, chat_id, content, role, sources, created_at`,
		chatID, content, role, sources,
	)
	var m Message
	var s []byte
	if err := row.Scan(&m.ID, &m.ChatID, &m.Content, &m.Role, &s, &m.CreatedAt); err != nil {
		return Message{}, err
	}
	if len(s) > 0 {
		m.Sources = s
	}
	return m, nil
}

// DeleteMessage removes a single message owned by userID.
func (c *Chats) DeleteMessage(ctx context.Context, userID int32, chatID, msgID uuid.UUID) error {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return err
	}
	cmd, err := c.pool.Exec(ctx, `DELETE FROM messages WHERE id = $1 AND chat_id = $2`, msgID, chatID)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearMessages wipes all messages in a chat.
func (c *Chats) ClearMessages(ctx context.Context, userID int32, chatID uuid.UUID) error {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return err
	}
	_, err := c.pool.Exec(ctx, `DELETE FROM messages WHERE chat_id = $1`, chatID)
	return err
}

// ListMissions returns mission summaries for the chat. Only the
// mission fields the frontend actually renders are selected to keep
// this query cheap.
func (c *Chats) ListMissions(ctx context.Context, userID int32, chatID uuid.UUID) ([]MissionSummary, error) {
	if _, err := c.Get(ctx, userID, chatID); err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, chat_id, COALESCE(user_request, ''), COALESCE(status, 'pending'),
		       current_report_version, created_at, updated_at
		FROM missions WHERE chat_id = $1 ORDER BY created_at DESC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MissionSummary
	for rows.Next() {
		var m MissionSummary
		if err := rows.Scan(&m.ID, &m.ChatID, &m.UserRequest, &m.Status, &m.CurrentReportVersion, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
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

// itoa is a zero-alloc int→string for small integers used in query
// placeholder numbering. strconv.Itoa works too; this keeps the hot
// List path free of an import.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
