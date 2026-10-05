// Client-controlled ingest selection (#166): the projection stays a full
// mirror, only job creation is gated. No row = default (everything selected).
package mirror

import (
	"context"
	"fmt"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
)

// SelectionInput is one entry of a batch PUT: mode "included"/"excluded"
// upserts the row, mode "default" (or "") removes it (back to default).
type SelectionInput struct {
	DocumentID string `json:"document_id"`
	Mode       string `json:"mode"`
}

// SetSelections applies a document-only selection batch (the IT helper).
// Unknown document ids error (FK) — a client naming a nonexistent document
// should hear about it. Delegates to SetSelectionBatch: one code path, one
// transaction, no SQL drift between the singles and the combined write.
func (m *Repo) SetSelections(ctx context.Context, in []SelectionInput) error {
	return m.SetSelectionBatch(ctx, in, nil)
}

// SelectionModes returns the persisted selection map (absent = default).
func (m *Repo) SelectionModes(ctx context.Context) (map[string]string, error) {
	rows, err := m.pool.Query(ctx, `SELECT document_id::text, mode FROM zotero_selections`)
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
// #358: the row is a CODE merge of the Library's mirror truth and the
// Store's job/snapshot truth; #356 adds the serving outcome.
type ZoteroDocumentState struct {
	DocumentID string           `json:"document_id"`
	ZoteroKey  string           `json:"zotero_key"`
	Title      string           `json:"title"`
	ItemType   string           `json:"item_type"`
	SyncState  string           `json:"sync_state"` // synced | held | processing | pending | tombstoned
	JobStatus  string           `json:"job_status,omitempty"`
	Attachment *AttachmentState `json:"attachment,omitempty"`
	// AttachmentID is the preferred rendition's durable uuid — the join
	// key the listing merge resolves the Store's job truth by (internal).
	AttachmentID  string    `json:"-"`
	RepairStatus  string    `json:"repair_status,omitempty"` // newest repair_cases.status, live
	SelectionMode string    `json:"-"`                       // persisted selection mode (internal)
	Outcome       string    `json:"outcome"`                 // completed | serving | in_repair | needs_ocr | failed | removed | processing | pending | excluded
	OutcomeReason string    `json:"outcome_reason,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// DeriveOutcome projects job/snapshot/repair/selection truth into the
// human answer for "what happened to my document?" (#252, truth fix #356).
// No new truth — precedence:
//
//	excluded → running → pending → needs_ocr (textless scan, from the
//	job's quality_state pagination_state, set by the #254 preflight) →
//	live repair track → completed → serving (terminal job closure BUT an
//	active snapshot keeps the document served — cancelled waves, cleanup
//
// closures, skips) → failed+reason excerpt → never enqueued/served.
//
// #356 semantics: `failed` is reserved for documents with NO active
// snapshot whose last attempt failed; an administrative closure with a
// surviving snapshot derives `serving` with the closure named in the
// reason. The docs' API reference carries the semantics table.
func DeriveOutcome(selMode, jobStatus, errCode, errMsg, paginationState, repairStatus string, hasActiveSnapshot bool) (outcome, reason string) {
	switch {
	case selMode == "excluded":
		return "excluded", "selection-excluded"
	case jobStatus == "claimed" || jobStatus == "processing":
		return "processing", ""
	case jobStatus == "pending":
		return "pending", ""
	case paginationState == "needs_ocr":
		return "needs_ocr", "scan-ohne-textlayer: text-less scan — OCR rebuild heals it (scan_ocr_rebuild, #284)"
	case repairStatus == "rejected" || repairStatus == "queued" ||
		repairStatus == "in_repair" || repairStatus == "blocked_for_dudu":
		return "in_repair", "repair case: " + repairStatus
	case jobStatus == "completed":
		return "completed", ""
	case jobStatus == "failed" || jobStatus == "skipped" || jobStatus == "cancelled":
		if hasActiveSnapshot {
			r := "last job closed as " + jobStatus
			if errCode != "" {
				r += " (" + errCode + ")"
			}
			return "serving", r + " — the active snapshot keeps the document served"
		}
		r := errCode
		if errMsg != "" {
			// rune-safe cap: byte slicing could split a multi-byte rune mid-sequence
			if r := []rune(errMsg); len(r) > 160 {
				errMsg = string(r[:160]) + "…"
			}
			if r != "" {
				r += ": "
			}
			r += errMsg
		}
		if r == "" {
			r = jobStatus
		}
		return "failed", r
	default:
		if hasActiveSnapshot {
			// no job row at all (pruned history, lost bookkeeping): the
			// snapshot is the proof of processing — served, not phantom.
			return "serving", "no job row — active snapshot serves"
		}
		return "pending", "never enqueued"
	}
}

// TombstoneVisibility bounds how long a reconciled deletion stays visible
// in the documents listing (#358 held-row reconciliation): a Zotero-absent
// item surfaces ONCE as a tombstone (sync_state="tombstoned",
// outcome="removed") for this window after its mirror row flipped deleted,
// then drops out of the listing instead of lingering as a phantom.
const TombstoneVisibility = 7 * 24 * time.Hour

// ListDocumentsMirror is the listing's LIBRARY half (#358: no
// cross-database SQL): the full non-deleted mirror projection with
// per-document selection mode and repair status, plus FRESH tombstones
// (reconciled deletions inside the visibility window). Job truth and
// active-snapshot truth live on the Store database — the caller merges
// them in code via DocumentListing (which also owns the sync_state
// FILTER: live rows only learn their sync state in the merge).
func (m *Repo) ListDocumentsMirror(ctx context.Context) ([]ZoteroDocumentState, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT d.id::text, d.zotero_key, COALESCE(d.title,''), COALESCE(d.item_type,''), d.updated_at,
		       d.deleted,
		       COALESCE(a.id::text,''), COALESCE(a.zotero_key,''), COALESCE(a.filename,''), COALESCE(a.content_type,''), COALESCE(a.content_hash,''),
		       COALESCE(rc.status,''),
		       COALESCE(s.mode,'')
	FROM zotero_documents d
	LEFT JOIN zotero_attachments a ON a.document_id=d.id AND a.preferred AND NOT a.deleted
	LEFT JOIN LATERAL (
		SELECT status::text FROM repair_cases rc WHERE rc.attachment_id=a.id
		ORDER BY rc.updated_at DESC, rc.id DESC LIMIT 1
	) rc ON true
	LEFT JOIN zotero_selections s ON s.document_id=d.id
		WHERE NOT d.deleted OR (d.deleted AND d.updated_at > now() - make_interval(secs => $1))
		ORDER BY COALESCE(d.title,'')`, int64(TombstoneVisibility.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ZoteroDocumentState{}
	for rows.Next() {
		var z ZoteroDocumentState
		var attID, attKey, attName, attType, attHash, selMode, repairStatus string
		var deleted bool
		if err := rows.Scan(&z.DocumentID, &z.ZoteroKey, &z.Title, &z.ItemType, &z.UpdatedAt,
			&deleted,
			&attID, &attKey, &attName, &attType, &attHash,
			&repairStatus, &selMode); err != nil {
			return nil, err
		}
		z.AttachmentID = attID
		z.RepairStatus = repairStatus
		z.SelectionMode = selMode
		if deleted {
			z.SyncState = "tombstoned"
			z.Outcome = "removed"
			z.OutcomeReason = "deleted in Zotero — reconciled at " + z.UpdatedAt.Format(time.RFC3339)
		}
		_ = deleted // filtering happens in DocumentListing (post-merge)
		if attKey != "" {
			z.Attachment = &AttachmentState{ZoteroKey: attKey, Filename: attName, ContentType: attType, ContentHash: attHash}
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

// DocumentListing merges the listing's two halves in code (#358): the
// Library's mirror rows (with selection + repair truth) and the Store's
// job/snapshot truth per rendition. Tombstoned rows pass through
// untouched (their truth is mirror-only).
func DocumentListing(mirrorRows []ZoteroDocumentState, jobs map[string]repo.JobState, serving map[string]bool, syncState string) []ZoteroDocumentState {
	out := make([]ZoteroDocumentState, 0, len(mirrorRows))
	for _, z := range mirrorRows {
		if z.SyncState == "tombstoned" {
			out = append(out, z)
			continue
		}
		var job repo.JobState
		hasJob := false
		if z.AttachmentID != "" {
			job, hasJob = jobs[z.AttachmentID]
		}
		snap := serving[z.DocumentID]
		status := ""
		if hasJob {
			status = job.Status
		}
		switch {
		case z.SelectionMode == "excluded":
			z.SyncState = "held"
		case status == "completed":
			z.SyncState = "synced"
		case status == "claimed" || status == "running" || status == "processing":
			z.SyncState = "processing"
		case status == "pending":
			z.SyncState = "pending"
		case snap:
			// the snapshot is the serving truth (#356): an administratively
			// closed document that still serves is synced, not held
			z.SyncState = "synced"
		default:
			// no job at all and not explicitly excluded: never selected for
			// processing in this configuration (e.g. file was missing once)
			z.SyncState = "held"
		}
		z.JobStatus = status
		var pagination string
		if hasJob {
			pagination = job.PaginationState
		}
		z.Outcome, z.OutcomeReason = DeriveOutcome(z.SelectionMode, status, job.ErrorCode, job.ErrorMessage,
			pagination, z.RepairStatus, snap)
		if syncState == "" || syncState == z.SyncState {
			out = append(out, z)
		}
	}
	return out
}

// SetSelectionBatch writes document AND collection selections in ONE
// transaction (#166): a failure mid-batch rolls both back — a half-applied
// selection would silently flip sync semantics for the other layer.
func (m *Repo) SetSelectionBatch(ctx context.Context, docs []SelectionInput, colls []CollectionSelectionInput) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	applyDocs := func(in []SelectionInput) error {
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
		return nil
	}
	applyColls := func(in []CollectionSelectionInput) error {
		for _, s := range in {
			switch s.Mode {
			case "included", "excluded":
				if _, err := tx.Exec(ctx, `
					INSERT INTO zotero_collection_selections (collection_key, mode, updated_at)
					VALUES ($1, $2, now())
					ON CONFLICT (collection_key) DO UPDATE SET mode=EXCLUDED.mode, updated_at=now()`,
					s.CollectionKey, s.Mode); err != nil {
					return err
				}
			case "default", "":
				if _, err := tx.Exec(ctx, `DELETE FROM zotero_collection_selections WHERE collection_key=$1`, s.CollectionKey); err != nil {
					return err
				}
			default:
				return fmt.Errorf("invalid collection selection mode %q", s.Mode)
			}
		}
		return nil
	}
	if err := applyDocs(docs); err != nil {
		return err
	}
	if err := applyColls(colls); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
