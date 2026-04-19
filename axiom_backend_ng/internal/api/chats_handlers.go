package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ChatStore is the subset of repo.Chats the chat handlers need.
type ChatStore interface {
	Create(ctx context.Context, userID int32, title, chatType string) (repo.Chat, error)
	Get(ctx context.Context, userID int32, id uuid.UUID) (repo.Chat, error)
	Delete(ctx context.Context, userID int32, id uuid.UUID) error
	UpdateTitle(ctx context.Context, userID int32, id uuid.UUID, title string) error
	UpdateSettings(ctx context.Context, userID int32, id uuid.UUID, settings json.RawMessage) error
	List(ctx context.Context, userID int32, opt repo.ListOptions) (repo.Paginated, error)
	ListMessages(ctx context.Context, userID int32, chatID uuid.UUID) ([]repo.Message, error)
	AppendMessage(ctx context.Context, userID int32, chatID uuid.UUID, role, content string, sources json.RawMessage) (repo.Message, error)
	DeleteMessage(ctx context.Context, userID int32, chatID, msgID uuid.UUID) error
	ClearMessages(ctx context.Context, userID int32, chatID uuid.UUID) error
	ListMissions(ctx context.Context, userID int32, chatID uuid.UUID) ([]repo.MissionSummary, error)
}

// ChatDeps wires the store.
type ChatDeps struct {
	Chats ChatStore
}

// ChatCreateRequest is the POST /api/chats body.
type ChatCreateRequest struct {
	Title    string `json:"title"`
	ChatType string `json:"chat_type"`
}

// TitleUpdateRequest is the PUT /api/chats/{id}/title body.
type TitleUpdateRequest struct {
	Title string `json:"title"`
}

// ChatUpdateRequest is the unified PUT /api/chats/{id} body matching
// the Python ChatUpdate schema: either field may be set.
type ChatUpdateRequest struct {
	Title    *string         `json:"title,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// MessageRequest is the POST /api/chats/{id}/messages body.
type MessageRequest struct {
	Role    string          `json:"role"`
	Content string          `json:"content"`
	Sources json.RawMessage `json:"sources,omitempty"`
}

// List handles GET /api/chats.
func (d ChatDeps) List(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	opt := repo.ListOptions{
		Page:     atoi(r.URL.Query().Get("page")),
		PageSize: atoi(r.URL.Query().Get("page_size")),
		ChatType: r.URL.Query().Get("chat_type"),
		Search:   r.URL.Query().Get("search"),
	}
	page, err := d.Chats.List(r.Context(), uid, opt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat list failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// Create handles POST /api/chats.
func (d ChatDeps) Create(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	var req ChatCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	chat, err := d.Chats.Create(r.Context(), uid, req.Title, req.ChatType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat create failed")
		return
	}
	writeJSON(w, http.StatusCreated, chat)
}

// Get handles GET /api/chats/{id}.
func (d ChatDeps) Get(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		chat, err := d.Chats.Get(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chat fetch failed")
			return
		}
		writeJSON(w, http.StatusOK, chat)
	})
}

// Delete handles DELETE /api/chats/{id}.
func (d ChatDeps) Delete(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		err := d.Chats.Delete(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chat delete failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Chat deleted"})
	})
}

// Update handles PUT /api/chats/{id} with an optional title and/or
// settings patch (parity with axiom_backend/api/chats.py:ChatUpdate).
func (d ChatDeps) Update(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		var req ChatUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		if req.Title == nil && req.Settings == nil {
			writeError(w, http.StatusBadRequest, "title or settings required")
			return
		}
		if req.Title != nil {
			if err := d.Chats.UpdateTitle(r.Context(), uid, id, *req.Title); err != nil {
				if errors.Is(err, repo.ErrNotFound) {
					writeError(w, http.StatusNotFound, "chat not found")
					return
				}
				writeError(w, http.StatusInternalServerError, "title update failed")
				return
			}
		}
		if req.Settings != nil {
			if err := d.Chats.UpdateSettings(r.Context(), uid, id, req.Settings); err != nil {
				if errors.Is(err, repo.ErrNotFound) {
					writeError(w, http.StatusNotFound, "chat not found")
					return
				}
				writeError(w, http.StatusInternalServerError, "settings update failed")
				return
			}
		}
		chat, err := d.Chats.Get(r.Context(), uid, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chat refetch failed")
			return
		}
		writeJSON(w, http.StatusOK, chat)
	})
}

// UpdateTitle handles PUT /api/chats/{id}/title.
func (d ChatDeps) UpdateTitle(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		var req TitleUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		if req.Title == "" {
			writeError(w, http.StatusBadRequest, "title is required")
			return
		}
		err := d.Chats.UpdateTitle(r.Context(), uid, id, req.Title)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "title update failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Title updated"})
	})
}

// GetTitle handles GET /api/chats/{id}/title.
func (d ChatDeps) GetTitle(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		chat, err := d.Chats.Get(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chat fetch failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"title": chat.Title})
	})
}

// ListMessages handles GET /api/chats/{id}/messages.
func (d ChatDeps) ListMessages(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		msgs, err := d.Chats.ListMessages(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "message list failed")
			return
		}
		if msgs == nil {
			msgs = []repo.Message{}
		}
		writeJSON(w, http.StatusOK, msgs)
	})
}

// AppendMessage handles POST /api/chats/{id}/messages.
func (d ChatDeps) AppendMessage(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		var req MessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		if req.Role == "" || req.Content == "" {
			writeError(w, http.StatusBadRequest, "role and content are required")
			return
		}
		msg, err := d.Chats.AppendMessage(r.Context(), uid, id, req.Role, req.Content, req.Sources)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "message insert failed")
			return
		}
		writeJSON(w, http.StatusCreated, msg)
	})
}

// ClearMessages handles DELETE /api/chats/{id}/messages.
func (d ChatDeps) ClearMessages(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		err := d.Chats.ClearMessages(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "message clear failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Messages cleared"})
	})
}

// DeleteMessage handles DELETE /api/chats/{id}/messages/{msgID}.
func (d ChatDeps) DeleteMessage(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		msgID, err := uuid.Parse(chi.URLParam(r, "msgID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid message id")
			return
		}
		err = d.Chats.DeleteMessage(r.Context(), uid, id, msgID)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "message not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "message delete failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Message deleted"})
	})
}

// ListMissions handles GET /api/chats/{id}/missions.
func (d ChatDeps) ListMissions(w http.ResponseWriter, r *http.Request) {
	d.ownedChat(w, r, func(uid int32, id uuid.UUID) {
		missions, err := d.Chats.ListMissions(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "mission list failed")
			return
		}
		if missions == nil {
			missions = []repo.MissionSummary{}
		}
		writeJSON(w, http.StatusOK, missions)
	})
}

// ownedChat parses {id} and dispatches to fn only when a valid UUID
// and authenticated user are present.
func (d ChatDeps) ownedChat(w http.ResponseWriter, r *http.Request, fn func(int32, uuid.UUID)) {
	uid := requireUserID(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chat id")
		return
	}
	fn(uid, id)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
