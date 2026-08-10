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
	since, err := s.repo.SourceVersion(ctx, sourceID)
	if err != nil {
		return Result{}, err
	}

	items, newVersion, err := s.src.ListPDFItems(since)
	if err != nil {
		return Result{}, fmt.Errorf("list items: %w", err)
	}

	// Mirror every document (and all of its attachments) first so the ingest
	// jobs can reference the mirror rows by id.
	if err := s.mirror(ctx, sourceID, items); err != nil {
		return Result{}, err
	}

	// Resolve, hash and enqueue the preferred attachment per document.
	preferred := 0
	var pending []repo.PendingJob
	for _, it := range items {
		pref := zotero.PreferredAttachment(it.Attachments)
		if pref == nil {
			continue
		}
		job, err := s.preferredJob(ctx, sourceID, it, pref)
		if err != nil {
			// Persist the failure as a failed ingest job so it is not silently
			// lost: a retryable file error (FILE_NOT_FOUND is non-retryable,
			// other I/O errors are) will be re-attempted on a later run once a
			// lease-based retry path exists. Never silently drop it.
			if failed := s.enqueueFailure(ctx, sourceID, it, pref, err); failed != nil {
				s.log.Printf("sync: %s %q: %v", it.Key, it.Title, err)
			}
			continue
		}
		pending = append(pending, *job)
		preferred++
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
