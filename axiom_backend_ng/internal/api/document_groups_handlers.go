package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// DocumentGroupStore is the subset of repo.DocumentGroups the handlers need.
type DocumentGroupStore interface {
	Create(ctx context.Context, userID int32, name, description string) (repo.DocumentGroup, error)
	List(ctx context.Context, userID int32, skip, limit int) ([]repo.DocumentGroupSummary, error)
	Get(ctx context.Context, userID int32, id uuid.UUID) (repo.DocumentGroup, error)
	Update(ctx context.Context, userID int32, id uuid.UUID, name, description string, setDescription bool) (repo.DocumentGroup, error)
	Delete(ctx context.Context, userID int32, id uuid.UUID) error
	AddDocument(ctx context.Context, userID int32, groupID, docID uuid.UUID) error
	RemoveDocument(ctx context.Context, userID int32, groupID, docID uuid.UUID) error
	BulkAddDocuments(ctx context.Context, userID int32, groupID uuid.UUID, docIDs []uuid.UUID) ([]uuid.UUID, map[uuid.UUID]error)
	BulkRemoveDocuments(ctx context.Context, userID int32, groupID uuid.UUID, docIDs []uuid.UUID) ([]uuid.UUID, map[uuid.UUID]error)
}

// DocumentGroupDeps wires the store.
type DocumentGroupDeps struct {
	Groups    DocumentGroupStore
	Documents DocumentStore
}

// GroupCreateRequest is the POST /api/document-groups/ body.
type GroupCreateRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

// GroupUpdateRequest is the PUT /api/document-groups/{id} body.
type GroupUpdateRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

// List handles GET /api/document-groups/.
func (d DocumentGroupDeps) List(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	groups, err := d.Groups.List(r.Context(), uid, atoi(r.URL.Query().Get("skip")), atoi(r.URL.Query().Get("limit")))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "group list failed")
		return
	}
	if groups == nil {
		groups = []repo.DocumentGroupSummary{}
	}
	writeJSON(w, http.StatusOK, groups)
}

// Create handles POST /api/document-groups/.
func (d DocumentGroupDeps) Create(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	var req GroupCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	desc := ""
	if req.Description != nil {
		desc = *req.Description
	}
	g, err := d.Groups.Create(r.Context(), uid, req.Name, desc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "group create failed")
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// Get handles GET /api/document-groups/{group_id}.
func (d DocumentGroupDeps) Get(w http.ResponseWriter, r *http.Request) {
	d.ownedGroup(w, r, func(uid int32, id uuid.UUID) {
		g, err := d.Groups.Get(r.Context(), uid, id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "document group not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "group fetch failed")
			return
		}
		writeJSON(w, http.StatusOK, g)
	})
}

// Update handles PUT /api/document-groups/{group_id}.
func (d DocumentGroupDeps) Update(w http.ResponseWriter, r *http.Request) {
	d.ownedGroup(w, r, func(uid int32, id uuid.UUID) {
		var req GroupUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		desc := ""
		setDesc := req.Description != nil
		if setDesc {
			desc = *req.Description
		}
		g, err := d.Groups.Update(r.Context(), uid, id, req.Name, desc, setDesc)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "document group not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "group update failed")
			return
		}
		writeJSON(w, http.StatusOK, g)
	})
}

// Delete handles DELETE /api/document-groups/{group_id}.
func (d DocumentGroupDeps) Delete(w http.ResponseWriter, r *http.Request) {
	d.ownedGroup(w, r, func(uid int32, id uuid.UUID) {
		err := d.Groups.Delete(r.Context(), uid, id)
		switch {
		case errors.Is(err, repo.ErrNotFound):
			writeError(w, http.StatusNotFound, "document group not found")
			return
		case errors.Is(err, repo.ErrGroupHasActiveMissions):
			writeError(w, http.StatusConflict, "group is used by active missions")
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, "group delete failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// ListDocuments handles GET /api/document-groups/{group_id}/documents/.
func (d DocumentGroupDeps) ListDocuments(w http.ResponseWriter, r *http.Request) {
	d.ownedGroup(w, r, func(uid int32, id uuid.UUID) {
		opt := documentListOptionsFromQuery(r)
		opt.GroupID = &id
		page, err := d.Documents.List(r.Context(), uid, opt)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "group document list failed")
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
}

// AddDocument handles POST /api/document-groups/{group_id}/add-document/{doc_id}.
func (d DocumentGroupDeps) AddDocument(w http.ResponseWriter, r *http.Request) {
	d.withGroupAndDoc(w, r, func(uid int32, groupID, docID uuid.UUID) {
		err := d.Groups.AddDocument(r.Context(), uid, groupID, docID)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "group or document not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "add document failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "document added to group"})
	})
}

// RemoveDocument handles DELETE /api/document-groups/{group_id}/documents/{doc_id}.
func (d DocumentGroupDeps) RemoveDocument(w http.ResponseWriter, r *http.Request) {
	d.withGroupAndDoc(w, r, func(uid int32, groupID, docID uuid.UUID) {
		err := d.Groups.RemoveDocument(r.Context(), uid, groupID, docID)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "group or document not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "remove document failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "document removed from group"})
	})
}

// BulkAdd handles POST /api/document-groups/{group_id}/bulk-add-documents.
func (d DocumentGroupDeps) BulkAdd(w http.ResponseWriter, r *http.Request) {
	d.ownedGroup(w, r, func(uid int32, groupID uuid.UUID) {
		var ids []uuid.UUID
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		ok, fail := d.Groups.BulkAddDocuments(r.Context(), uid, groupID, ids)
		writeJSON(w, http.StatusOK, map[string]any{
			"added_count":      len(ok),
			"failed_additions": failedIDs(fail),
			"message":          "bulk add completed",
		})
	})
}

// BulkRemove handles POST /api/document-groups/{group_id}/bulk-remove-documents.
func (d DocumentGroupDeps) BulkRemove(w http.ResponseWriter, r *http.Request) {
	d.ownedGroup(w, r, func(uid int32, groupID uuid.UUID) {
		var ids []uuid.UUID
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		ok, fail := d.Groups.BulkRemoveDocuments(r.Context(), uid, groupID, ids)
		writeJSON(w, http.StatusOK, map[string]any{
			"removed_count":   len(ok),
			"failed_removals": failedIDs(fail),
			"message":         "bulk remove completed",
		})
	})
}

func (d DocumentGroupDeps) ownedGroup(w http.ResponseWriter, r *http.Request, fn func(int32, uuid.UUID)) {
	uid := requireUserID(r)
	id, err := uuid.Parse(chi.URLParam(r, "group_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	fn(uid, id)
}

func (d DocumentGroupDeps) withGroupAndDoc(w http.ResponseWriter, r *http.Request, fn func(int32, uuid.UUID, uuid.UUID)) {
	uid := requireUserID(r)
	groupID, err := uuid.Parse(chi.URLParam(r, "group_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	docID, err := uuid.Parse(chi.URLParam(r, "doc_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid doc id")
		return
	}
	fn(uid, groupID, docID)
}

func failedIDs(fail map[uuid.UUID]error) []map[string]string {
	out := make([]map[string]string, 0, len(fail))
	for id, err := range fail {
		out = append(out, map[string]string{"id": id.String(), "error": err.Error()})
	}
	return out
}
