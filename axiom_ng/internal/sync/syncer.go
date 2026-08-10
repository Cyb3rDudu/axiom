// Package sync orchestrates pulling documents from a Zotero source, mirroring
// them into the PostgreSQL store and enqueuing processing work for their
// preferred attachments.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// Service coordinates a Zotero source with the ingest queue.
type Service struct {
	src     zotero.Source
	repo    *repo.Repo
	baseURL string
	libID   string
	log     *log.Logger
}

// New builds a sync service for one Zotero source and the ingest queue.
func New(src zotero.Source, r *repo.Repo, baseURL, libID string, log *log.Logger) *Service {
	return &Service{src: src, repo: r, baseURL: baseURL, libID: libID, log: log}
}

// Result is a summary of one sync run.
type Result struct {
	SourceID             string `json:"source_id"`
	ServerID             string `json:"server_id"`
	ScannedDocuments     int    `json:"scanned_documents"`
	PreferredAttachments int    `json:"preferred_attachments"`
	Enqueued             int    `json:"enqueued_jobs"`
	NewVersion           int64  `json:"library_version"`
}

// Run performs a full or incremental sync and enqueues preferred attachments.
func (s *Service) Run(ctx context.Context) (Result, error) {
	serverID := s.src.ServerID()
	if serverID == "" {
		return Result{}, errors.New("zotero source unreachable: no server id")
	}
	sourceID, err := s.repo.EnsureSource(ctx, s.baseURL, s.libID, serverID)
	if err != nil {
		return Result{}, err
	}
	// Serialise the whole sync per source so a slower, stale delta cannot
	// overwrite a newer run's reconciliation or cursor. The dedicated
	// connection holding the lock is released via the returned func on exit.
	release, err := s.repo.AcquireSourceLock(ctx, sourceID)
	if err != nil {
		return Result{}, err
	}
	defer release()

	since, err := s.repo.SourceVersion(ctx, sourceID)
	if err != nil {
		return Result{}, err
	}

	// Fetch the touched documents (plus affected/deleted parent keys) and any
	// items Zotero reports as deleted, so reconciliation is scoped to exactly
	// what changed.
	res, err := s.src.ListPDFItems(since)
	if err != nil {
		return Result{}, fmt.Errorf("list items: %w", err)
	}
	items := res.Items
	newVersion := res.NewVersion

	// Mirror every document (and all of its attachments) first so the ingest
	// jobs can reference the mirror rows by id.
	if err := s.mirror(ctx, sourceID, items); err != nil {
		return Result{}, err
	}

	// Resolve, hash and enqueue the preferred attachment per document. Also
	// collect the per-document attachment keys and preferred key so
	// reconciliation can mark removed/preferred-swapped attachments — scoped
	// only to the documents this run actually touched.
	preferred := 0
	var pending []repo.PendingJob
	seenAtts := map[string][]string{} // docKey -> attachment keys still present
	prefAtts := map[string]string{}   // docKey -> preferred attachment key
	processItem := func(it zotero.Item) error {
		var keys []string
		for _, att := range it.Attachments {
			keys = append(keys, att.Key)
		}
		seenAtts[it.Key] = keys
		pref := zotero.PreferredAttachment(it.Attachments)
		if pref == nil {
			return nil
		}
		job, err := s.preferredJob(ctx, sourceID, it, pref)
		if err != nil {
			// Record the file-resolution failure as a failed ingest job so it
			// is not silently lost (FILE_NOT_FOUND non-retryable, IO_ERROR
			// retryable). If even recording fails we must abort the sync,
			// otherwise the attachment is dropped and the cursor is advanced
			// over it.
			if rerr := s.enqueueFailure(ctx, sourceID, it, pref, err); rerr != nil {
				return fmt.Errorf("record failure for %s: %w", it.Key, rerr)
			}
			return nil
		}
		pending = append(pending, *job)
		prefAtts[it.Key] = pref.Key
		preferred++
		return nil
	}
	for i := range items {
		if err := processItem(items[i]); err != nil {
			return Result{}, err
		}
	}

	// Deleted keys: parents that 404'd during reconstruction plus structured
	// trash events. A deleted parent is removed with its attachments. A deleted
	// single attachment is removed individually, and its parent is re-processed
	// so a remaining sibling (e.g. EPUB) can become the new preferred and be
	// enqueued even though the parent was not otherwise changed.
	deletedKeys := append([]string(nil), res.DeletedKeys...)
	trashDeleted, _, derr := s.src.ListDeletedKeys(since)
	if derr != nil {
		// A failure reading deletions must not silently drop them; abort so the
		// cursor is not advanced.
		return Result{}, fmt.Errorf("list deleted items: %w", derr)
	}
	reprocessParents := map[string]bool{}
	for _, ev := range trashDeleted {
		deletedKeys = append(deletedKeys, ev.Key)
		if ev.ItemType == "attachment" && ev.ParentKey != "" {
			reprocessParents[ev.ParentKey] = true
		}
	}

	// Re-process parents whose (preferred) attachment was deleted: reconstruct
	// them and run the normal preferred/hash/enqueue path so a replacement
	// sibling is selected and a job is created.
	for parentKey := range reprocessParents {
		if _, already := seenAtts[parentKey]; already {
			continue // already processed in this run's items
		}
		parent, err := s.src.FetchParent(parentKey)
		if err != nil {
			return Result{}, fmt.Errorf("reprocess deleted-attachment parent %s: %w", parentKey, err)
		}
		if parent == nil {
			continue // parent gone entirely; reconcile deletion covers it
		}
		if err := processItem(*parent); err != nil {
			return Result{}, err
		}
		res.AffectedKeys = append(res.AffectedKeys, parentKey)
	}

	if err := s.repo.Reconcile(ctx, repo.ReconcileReq{
		SourceID:             sourceID,
		ReconcileAll:         since == 0,
		AffectedDocKeys:      res.AffectedKeys,
		DeletedDocKeys:       zotero.DedupKeys(deletedKeys),
		SeenAttachments:      seenAtts,
		PreferredAttachments: prefAtts,
	}); err != nil {
		return Result{}, err
	}

	// On a full sync, reconcile documents that no longer exist in Zotero at
	// all (not just attachments): their document row and attachments are marked
	// removed. AffectedKeys on a full sync is the complete set of present
	// parent keys.
	if since == 0 {
		if err := s.repo.MarkMissingDocumentsDeleted(ctx, sourceID, res.AffectedKeys); err != nil {
			return Result{}, err
		}
	}

	enqueued, err := s.repo.Enqueue(ctx, pending)
	if err != nil {
		return Result{}, err
	}
	if err := s.repo.SetSourceVersion(ctx, sourceID, newVersion); err != nil {
		return Result{}, err
	}

	return Result{
		SourceID:             sourceID,
		ServerID:             serverID,
		ScannedDocuments:     len(items),
		PreferredAttachments: preferred,
		Enqueued:             enqueued,
		NewVersion:           newVersion,
	}, nil
}

// preferredJob builds a pending job for one preferred attachment, hashing its
// local file to establish the idempotency key, and persists the resolved file
// info (hash, size, mtime, preferred) on the attachment row. The attachment
// path may be a file:// URI from Zotero and is normalised to a native path
// first.
func (s *Service) preferredJob(ctx context.Context, sourceID string, item zotero.Item, pref *zotero.Attachment) (*repo.PendingJob, error) {
	localPath := zotero.LocalFilePath(pref.LocalPath)
	if localPath == "" {
		return nil, fmt.Errorf("attachment %s has no local path", pref.Key)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("attachment %s: local file missing: %w", pref.Key, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("attachment %s: local path is not a regular file", pref.Key)
	}
	hash, err := zotero.ContentHash(localPath)
	if err != nil {
		return nil, fmt.Errorf("hash attachment %s: %w", pref.Key, err)
	}
	if err := s.repo.UpdateAttachmentFileInfo(ctx, sourceID, pref.Key, hash, info.Size(), info.ModTime().UnixMilli(), true); err != nil {
		return nil, err
	}
	docID, err := s.repo.DocumentID(ctx, sourceID, item.Key)
	if err != nil {
		return nil, err
	}
	attID, err := s.repo.AttachmentID(ctx, sourceID, pref.Key)
	if err != nil {
		return nil, err
	}
	return &repo.PendingJob{
		SourceID:     sourceID,
		DocumentID:   docID,
		AttachmentID: attID,
		ContentHash:  hash,
	}, nil
}

// enqueueFailure records a preferred attachment that could not be resolved as
// a failed ingest job so the error is visible and non-silently retained.
// Unrecoverable problems (missing file) are tagged FILE_NOT_FOUND; transient
// I/O errors are tagged IO_ERROR and left retryable for a later run.
func (s *Service) enqueueFailure(ctx context.Context, sourceID string, item zotero.Item, pref *zotero.Attachment, cause error) error {
	code := "IO_ERROR"
	retryable := true
	if errors.Is(cause, os.ErrNotExist) {
		code = "FILE_NOT_FOUND"
		retryable = false
	}
	docID, err := s.repo.DocumentID(ctx, sourceID, item.Key)
	if err != nil {
		return fmt.Errorf("document id for failed attachment %s: %w", pref.Key, err)
	}
	attID, err := s.repo.AttachmentID(ctx, sourceID, pref.Key)
	if err != nil {
		return err
	}
	return s.repo.EnqueueFailed(ctx, repo.FailedJob{
		SourceID:     sourceID,
		DocumentID:   docID,
		AttachmentID: attID,
		ErrorCode:    code,
		ErrorMessage: cause.Error(),
		Retryable:    retryable,
	})
}

func (s *Service) mirror(ctx context.Context, sourceID string, items []zotero.Item) error {
	return s.repo.SyncDocuments(ctx, sourceID, items)
}
