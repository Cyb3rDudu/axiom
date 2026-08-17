// Client-controlled ingest selection (#166): the projection stays a full
// mirror, only job creation is gated. No row = default (everything selected).
package repo

import (
	"context"
	"fmt"
	"time"
)

// SelectionInput is one entry of a batch PUT: mode "included"/"excluded"
// upserts the row, mode "default" (or "") removes it (back to default).
type SelectionInput struct {
	DocumentID string `json:"document_id"`
	Mode       string `json:"mode"`
}

// SetSelections applies a selection batch in one transaction: upserts for
// included/excluded, row deletion for default. Unknown document ids error
// (FK) — a client naming a nonexistent document should hear about it.
func (r *Repo) SetSelections(ctx context.Context, in []SelectionInput) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, s := range in {
		switch s.Mode {
		case "included", "excluded":
			if _, err := tx.Exec(ctx, `
				INSERT INTO zotero_selections (document_id, mode, updated_at)
				VALUES ($1::uuid, $2, now())
				ON CONFLICT (document_id) DO UPDATE SET mode=EXCLUDED.mode, updated_at=now()`,
				s.DocumentID, s.Mode); err != nil {
				return err
			}
		case "default", "":
			if _, err := tx.Exec(ctx, `DELETE FROM zotero_selections WHERE document_id=$1::uuid`, s.DocumentID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid selection mode %q", s.Mode)
		}
	}
	return tx.Commit(ctx)
}

// SelectionModes returns the persisted selection map (absent = default).
func (r *Repo) SelectionModes(ctx context.Context) (map[string]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT document_id::text, mode FROM zotero_selections`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, mode string
		if err := rows.Scan(&id, &mode); err != nil {
			return nil, err
		}
		out[id] = mode
	}
	return out, rows.Err()
}

// EffectiveSelection merges the persisted selection with a one-run override
// (the sync request body's include/exclude lists; override wins for this run
// only and is never persisted). Returns the map the job gate consults.
func EffectiveSelection(persisted map[string]string, overrideInclude, overrideExclude []string) map[string]string {
	if len(persisted) == 0 && len(overrideInclude) == 0 && len(overrideExclude) == 0 {
		return nil // no gate at all — today's behavior
	}
	m := make(map[string]string, len(persisted)+len(overrideInclude)+len(overrideExclude))
	for k, v := range persisted {
		m[k] = v
	}
	for _, id := range overrideInclude {
		m[id] = "included"
	}
	for _, id := range overrideExclude {
		m[id] = "excluded"
	}
	return m
}

// JobGated reports whether the document's pending job must be suppressed.
func JobGated(selection map[string]string, documentID string) bool {
	return selection != nil && selection[documentID] == "excluded"
}

// AttachmentState is the preferred attachment's client-facing info in the
// sync-state listing (nil when the document has no attachment).
type AttachmentState struct {
	ZoteroKey   string `json:"zotero_key"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ContentHash string `json:"content_hash,omitempty"`
}

// ZoteroDocumentState is one row of the client's sync-state listing (#166
// Ziel 4): Zotero bestand + ingest status + preferred attachment info.
type ZoteroDocumentState struct {
	DocumentID string           `json:"document_id"`
	ZoteroKey  string           `json:"zotero_key"`
	Title      string           `json:"title"`
	ItemType   string           `json:"item_type"`
	SyncState  string           `json:"sync_state"` // synced | held | processing | pending
	JobStatus  string           `json:"job_status,omitempty"`
	Attachment *AttachmentState `json:"attachment,omitempty"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// ListZoteroDocuments returns the full non-deleted Zotero projection with
// per-document sync state: synced = a completed job exists for the preferred
// attachment; held = selection-excluded or no job ever; processing/pending
// from the newest job's status. syncState filter ("") returns everything.
func (r *Repo) ListZoteroDocuments(ctx context.Context, syncState string) ([]ZoteroDocumentState, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT d.id::text, d.zotero_key, COALESCE(d.title,''), COALESCE(d.item_type,''), d.updated_at,
		       COALESCE(a.zotero_key,''), COALESCE(a.filename,''), COALESCE(a.content_type,''), COALESCE(a.content_hash,''),
		       COALESCE(j.status::text,''),
		       COALESCE(s.mode,'')
		FROM zotero_documents d
		LEFT JOIN zotero_attachments a ON a.document_id=d.id AND a.preferred AND NOT a.deleted
		LEFT JOIN LATERAL (
			SELECT status FROM ingest_jobs j WHERE j.attachment_id=a.id
			ORDER BY j.updated_at DESC, j.id DESC LIMIT 1
		) j ON true
		LEFT JOIN zotero_selections s ON s.document_id=d.id
		WHERE NOT d.deleted
		ORDER BY COALESCE(d.title,'')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ZoteroDocumentState{}
	for rows.Next() {
		var z ZoteroDocumentState
		var attKey, attName, attType, attHash, jobStatus, selMode string
		if err := rows.Scan(&z.DocumentID, &z.ZoteroKey, &z.Title, &z.ItemType, &z.UpdatedAt,
			&attKey, &attName, &attType, &attHash, &jobStatus, &selMode); err != nil {
			return nil, err
		}
		switch {
		case selMode == "excluded":
			z.SyncState = "held"
		case jobStatus == "completed":
			z.SyncState = "synced"
		case jobStatus == "claimed" || jobStatus == "running" || jobStatus == "processing":
			z.SyncState = "processing"
		case jobStatus == "pending":
			z.SyncState = "pending"
		default:
			// no job at all and not explicitly excluded: never selected for
			// processing in this configuration (e.g. file was missing once)
			z.SyncState = "held"
		}
		z.JobStatus = jobStatus
		if attKey != "" {
			z.Attachment = &AttachmentState{ZoteroKey: attKey, Filename: attName, ContentType: attType, ContentHash: attHash}
		}
		if syncState == "" || syncState == z.SyncState {
			out = append(out, z)
		}
	}
	return out, rows.Err()
}
