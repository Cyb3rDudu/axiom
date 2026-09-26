// Atomic canonical apply: in ONE transaction writes canonical rows, applies
// deletions, derives projections from the full active zotero_items state,
// updates memberships, writes pending/failed jobs and the cursor.
package mirror

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5"
)

// AttachmentFileInfo holds pre-computed (pre-transaction) file facts for an
// attachment, used to write preferred/hash/stats and craft ingest jobs. Ids are
// resolved inside the transaction. When a file cannot be processed, ErrCode
// distinguishes FILE_NOT_FOUND (absent) from a retryable IO_ERROR.
type AttachmentFileInfo struct {
	LocalPath string
	Exists    bool
	Hash      string
	FileSize  int64
	MtimeMS   int64
	ErrCode   string
	ErrMsg    string
	Retryable bool
}

// CanonicalApplyResult summarises an atomic canonical apply.
type CanonicalApplyResult struct {
	Flags               []CanonicalDocFlag
	Enqueued            int
	FailedJobs          int
	DocumentProjections int
}

// ApplyCanonicalBatch atomically applies a canonical batch. markMissing of
// absent items only happens when batch.FullSnapshot is true (since==0);
// incremental batches never infer deletions from absence. Explicit
// deleteEvents are applied by resolving each key against documents or
// attachments. Projections are derived from the complete active state of
// zotero_items (not the delta). Pending and failed jobs plus the cursor are
// written in the same transaction via the caller-provided tx.
// The selection map gates job creation (#166): entries "excluded" suppress
// both pending and failed jobs for that document; nil = no gate (everything
// is selected — today's behavior). Projections stay a FULL mirror regardless.
// contextual (#255) is the boot-resolved rule set; citation_class is
// recomputed from memberships + tags inside this same transaction.
func (m *Repo) ApplyCanonicalBatch(ctx context.Context, tx pgx.Tx, sourceID string, batch zoteroprovider.CanonicalBatch, collections []zoteroprovider.CanonicalCollection, files map[string]AttachmentFileInfo, selection map[string]string, contextual ContextualRules) (CanonicalApplyResult, error) {
	var res CanonicalApplyResult

	// 1. Upsert canonical items (version guarded).
	presentKeys := make([]string, 0, len(batch.Items))
	for _, it := range batch.Items {
		if err := m.upsertCanonicalItem(ctx, tx, sourceID, it); err != nil {
			return res, err
		}
		presentKeys = append(presentKeys, it.Key)
	}
	// 2. Mark missing items only on a full snapshot.
	if batch.FullSnapshot {
		if err := m.markCanonicalItemsMissing(ctx, tx, sourceID, presentKeys); err != nil {
			return res, err
		}
	}
	// 3. Absent from a full snapshot's canonical items are deleted; for a delta
	// only the deleteEvents are applied.
	if err := m.applyCanonicalDeleteEvents(ctx, tx, sourceID, batch.DeleteEvents); err != nil {
		return res, err
	}

	// 4. Collections. ListCanonicalCollections always returns a complete
	// snapshot, so missing collections are reconciled (marked deleted) on every
	// run — regardless of whether the item batch was a full snapshot.
	presentCols := make([]string, 0, len(collections))
	for _, c := range collections {
		if err := m.upsertCanonicalCollection(ctx, tx, sourceID, c); err != nil {
			return res, err
		}
		presentCols = append(presentCols, c.Key)
	}
	if err := m.markCanonicalCollectionsMissing(ctx, tx, sourceID, presentCols); err != nil {
		return res, err
	}

	// 5. Memberships for collections referenced by active items.
	if err := m.rebuildMemberships(ctx, tx, sourceID); err != nil {
		return res, err
	}

	// 6. Derive projections from the full active state.
	proj, err := m.deriveFullProjections(ctx, tx, sourceID, files)
	if err != nil {
		return res, err
	}
	res.Flags = proj.flags
	res.DocumentProjections = len(proj.flags)

	// 6a. #255 contextual class projection: recompute citation_class from
	// the memberships (step 5) + tags of the JUST-projected documents, in
	// the same transaction — the class rides every subsequent consumer of
	// this sync atomically.
	if err := recomputeCitationClassTx(ctx, tx, sourceID, contextual); err != nil {
		return res, err
	}

	// 6b. Deleted attachments must stop serving: retire their active
	// snapshots (+ OS tombstones) in the same sync transaction. This runs
	// AFTER the projections — THEY write zotero_attachments.deleted, and a
	// retire placed earlier sees only the PREVIOUS sync's flag (the Mullins
	// zombie: the one sync that projected the fix-service deletion retired
	// nothing; every earlier heal survived only because follow-up syncs
	// ran). Same-tx same-sync is the contract the Durchpfad IT pins.
	if err := m.store.ReconcileAttachmentSnapshotsTx(ctx, tx); err != nil {
		return res, fmt.Errorf("reconcile attachment snapshots: %w", err)
	}

	// 7. Pending + failed jobs in the same transaction, gated by the
	// selection (#166): excluded documents get no jobs; everything else
	// keeps the ON CONFLICT (attachment_id, content_hash) dedup — a
	// re-selected document with no existing job row gets one WITHOUT any
	// Zotero-side change (the acceptance case: the full derivation offers
	// every doc on every sync; only the gate held it back).
	if selection != nil {
		gatedPending := make([]repo.PendingJob, 0, len(proj.pending))
		for _, p := range proj.pending {
			if !JobGated(selection, p.DocumentID) {
				gatedPending = append(gatedPending, p)
			}
		}
		gatedFailed := make([]repo.FailedJob, 0, len(proj.failed))
		for _, f := range proj.failed {
			if !JobGated(selection, f.DocumentID) {
				gatedFailed = append(gatedFailed, f)
			}
		}
		proj.pending, proj.failed = gatedPending, gatedFailed
	}
	enqueued, failed, err := m.store.WriteSyncJobsTx(ctx, tx, sourceID, proj.pending, proj.failed)
	if err != nil {
		return res, err
	}
	res.Enqueued = enqueued
	res.FailedJobs = failed

	return res, nil
}


// applyCanonicalDeleteEvents resolves each deleted key against documents or
// attachments: a document deletion removes the parent + attachments; a single
// attachment deletion removes only that file.
func (m *Repo) applyCanonicalDeleteEvents(ctx context.Context, tx pgx.Tx, sourceID string, events []zoteroprovider.DeleteEvent) error {
	for _, ev := range events {
		if ev.Key == "" {
			continue
		}
		var isDoc bool
		_ = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM zotero_items WHERE source_id=$1 AND zotero_key=$2 AND deleted=false AND parent_key IS NULL)`,
			sourceID, ev.Key).Scan(&isDoc)
		if isDoc {
			// Parent document deleted: remove doc-level item + its children.
			if _, err := tx.Exec(ctx, `UPDATE zotero_items SET deleted=true, updated_at=now()
				WHERE source_id=$1 AND (zotero_key=$2 OR parent_key=$2)`, sourceID, ev.Key); err != nil {
				return fmt.Errorf("canonical delete parent %s: %w", ev.Key, err)
			}
			continue
		}
		// Single attachment (or note) deletion.
		if _, err := tx.Exec(ctx, `UPDATE zotero_items SET deleted=true, updated_at=now()
			WHERE source_id=$1 AND zotero_key=$2 AND deleted=false`, sourceID, ev.Key); err != nil {
			return fmt.Errorf("canonical delete attachment %s: %w", ev.Key, err)
		}
	}
	return nil
}

func itemContentType(data json.RawMessage) string {
	var d struct {
		ContentType string `json:"contentType"`
	}
	_ = json.Unmarshal(data, &d)
	return d.ContentType
}

func itemFilename(data json.RawMessage) string {
	var d struct {
		Filename string `json:"filename"`
	}
	_ = json.Unmarshal(data, &d)
	return d.Filename
}

func itemLocalPath(env json.RawMessage) string {
	var e struct {
		Links struct {
			Enclosure struct {
				Href string `json:"href"`
			} `json:"enclosure"`
		} `json:"links"`
	}
	_ = json.Unmarshal(env, &e)
	return e.Links.Enclosure.Href
}

func itemLinkMode(data json.RawMessage) string {
	var d struct {
		LinkMode string `json:"linkMode"`
	}
	_ = json.Unmarshal(data, &d)
	return d.LinkMode
}

// isProjectable reports whether a canonical item type may become a document
// projection. Rather than maintaining an incomplete allow-list, every top-level
// item type is projectable EXCEPT the known non-document types (which have
// neither bibliographic metadata nor a processable file). Scientific types that
// carry a PDF/EPUB attachment — document, dataset, presentation, computerProgram,
// interview, patent, case, etc. — are therefore projected and enqueued.
func isProjectable(itemType string) bool {
	switch itemType {
	case "note", "attachment", "annotation":
		return false
	default:
		return true
	}
}

// rebuildMemberships (re)writes zotero_item_collections from active items'
// data.collections keys against the canonical collection keys. It loads the
// collection key->id map up front so it does not issue nested queries while a
// result set is open on the same transaction connection.
func (m *Repo) rebuildMemberships(ctx context.Context, tx pgx.Tx, sourceID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM zotero_item_collections
		WHERE item_id IN (SELECT id FROM zotero_items WHERE source_id=$1)`, sourceID); err != nil {
		return fmt.Errorf("clear memberships: %w", err)
	}
	// Collection key -> id map.
	colMap := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT zotero_key, id::text FROM zotero_collections WHERE source_id=$1`, sourceID)
	if err != nil {
		return fmt.Errorf("query collections: %w", err)
	}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			rows.Close()
			return err
		}
		colMap[k] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	parents, err := tx.Query(ctx, `
		SELECT i.id, i.raw_data
		FROM zotero_items i
		WHERE i.source_id=$1 AND i.deleted=false AND (i.parent_key IS NULL OR i.parent_key='')`, sourceID)
	if err != nil {
		return fmt.Errorf("query active parents for memberships: %w", err)
	}
	// Collect parent -> collection refs first (do not run nested queries on the
	// transaction connection while a result set is open).
	var refs []struct{ itemID, colID string }
	for parents.Next() {
		var id string
		var raw []byte
		if err := parents.Scan(&id, &raw); err != nil {
			parents.Close()
			return err
		}
		var d struct {
			Collections []string `json:"collections"`
		}
		_ = json.Unmarshal(raw, &d)
		for _, ck := range d.Collections {
			if colID, ok := colMap[ck]; ok {
				refs = append(refs, struct{ itemID, colID string }{id, colID})
			}
		}
	}
	parents.Close()
	if err := parents.Err(); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := tx.Exec(ctx, `INSERT INTO zotero_item_collections (item_id, collection_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, ref.itemID, ref.colID); err != nil {
			return fmt.Errorf("insert membership: %w", err)
		}
	}
	return nil
}
