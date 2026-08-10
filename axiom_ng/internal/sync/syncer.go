// Package sync orchestrates pulling documents from a Zotero source, mirroring
// them into the PostgreSQL store and enqueuing processing work for their
// preferred attachments.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log"

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
			s.log.Printf("sync: %s %q: %v", it.Key, it.Title, err)
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
// local file to establish the idempotency key.
func (s *Service) preferredJob(ctx context.Context, sourceID string, item zotero.Item, pref *zotero.Attachment) (*repo.PendingJob, error) {
	if pref.LocalPath == "" {
		return nil, fmt.Errorf("attachment %s has no local path", pref.Key)
	}
	hash, err := zotero.ContentHash(pref.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("hash attachment %s: %w", pref.Key, err)
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

func (s *Service) mirror(ctx context.Context, sourceID string, items []zotero.Item) error {
	return s.repo.SyncDocuments(ctx, sourceID, items)
}
