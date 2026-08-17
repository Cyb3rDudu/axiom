// Package sync orchestrates pulling documents from a Zotero source, mirroring
// them into the PostgreSQL store and enqueuing processing work for their
// preferred attachments.
package sync

import (
	"context"
	"encoding/json"
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

// Result is a summary of one canonical sync run exposed by POST /api/zotero/sync.
type Result struct {
	SourceID    string `json:"source_id"`
	Items       int    `json:"canonical_items"`
	Collections int    `json:"canonical_collections"`
	Documents   int    `json:"document_projections"`
	Enqueued    int    `json:"enqueued_jobs"`
	NewVersion  int64  `json:"library_version"`
}

// Run performs the lossless canonical sync for the Zotero source — the single
// sync path. It writes zotero_items and zotero_collections losslessly, derives
// the normalized document/attachment projections, and enqueues ingest jobs for
// preferred processable attachments (never for notes/annotations or
// non-bibliographic parents). A metadata-only change with an identical
// attachment hash does not create a new job.
// SyncOverride is the one-run selection override from the sync request body
// (#166): include/exclude document-id lists applied ON TOP of the persisted
// selection for THIS run only (never persisted).
type SyncOverride struct {
	Include []string
	Exclude []string
}

func (s *Service) Run(ctx context.Context, override *SyncOverride) (Result, error) {
	serverID := s.src.ServerID()
	if serverID == "" {
		return Result{}, errors.New("zotero source unreachable")
	}
	sourceID, err := s.repo.EnsureSource(ctx, s.baseURL, s.libID, serverID)
	if err != nil {
		return Result{}, err
	}
	release, err := s.repo.AcquireSourceLock(ctx, sourceID)
	if err != nil {
		return Result{}, err
	}
	defer release()

	since, err := s.repo.CanonicalCursor(ctx, sourceID)
	if err != nil {
		return Result{}, err
	}
	batch, err := s.src.ListCanonicalItems(since)
	if err != nil {
		return Result{}, fmt.Errorf("list canonical items: %w", err)
	}
	collections, err := s.src.ListCanonicalCollections()
	if err != nil {
		return Result{}, fmt.Errorf("list canonical collections: %w", err)
	}

	// Pre-compute file facts (hash/size/mtime/existence) for every active
	// attachment BEFORE opening the apply transaction, so no long DB
	// transaction holds an open file handle. Missing files are recorded as
	// failed jobs by the apply.
	files, err := s.prepareAttachmentFiles(ctx, sourceID, batch.Items)
	if err != nil {
		return Result{}, err
	}

	// Selection read BEFORE the apply tx: fail without a transaction, and
	// don't hold a second pool connection during the apply.
	persisted, err := s.repo.SelectionModes(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("loading selections: %w", err)
	}
	var ovInclude, ovExclude []string
	if override != nil {
		ovInclude, ovExclude = override.Include, override.Exclude
	}
	selection := repo.EffectiveSelection(persisted, ovInclude, ovExclude)

	// One atomic transaction: canonical rows + deletions + projections +
	// memberships + pending/failed jobs + cursor.
	tx, err := s.repo.Pool().Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)

	applyRes, err := s.repo.ApplyCanonicalBatch(ctx, tx, sourceID, batch, collections, files, selection)
	if err != nil {
		return Result{}, err
	}
	if err := s.repo.SetCanonicalCursorTx(ctx, tx, sourceID, batch.NewVersion); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}

	return Result{
		SourceID:    sourceID,
		Items:       len(batch.Items),
		Collections: len(collections),
		Documents:   applyRes.DocumentProjections,
		Enqueued:    applyRes.Enqueued,
		NewVersion:  batch.NewVersion,
	}, nil
}

// prepareAttachmentFiles hashes/stats the local file of the AUTHORITATIVE
// (version-merged) attachment set — the committed store state first, then only
// batch items that are absent from the store or strictly newer. An older,
// rejected delta attachment can therefore never override a newer projection's
// path, hash or job. Runs before the apply transaction.
func (s *Service) prepareAttachmentFiles(ctx context.Context, sourceID string, batch []zotero.CanonicalItem) (map[string]repo.AttachmentFileInfo, error) {
	out := map[string]repo.AttachmentFileInfo{}

	// 1. Committed store state: attachment key -> version + envelope path.
	storeVer := map[string]int64{}
	rows, err := s.repo.Pool().Query(ctx, `
		SELECT zotero_key, zotero_version, raw_envelope::text
		FROM zotero_items WHERE source_id=$1 AND deleted=false AND item_type='attachment'`, sourceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key, env string
		var ver int64
		if err := rows.Scan(&key, &ver, &env); err != nil {
			rows.Close()
			return nil, err
		}
		storeVer[key] = ver
		path := zotero.LocalFilePath(itemLocalPathFromEnv([]byte(env)))
		out[key] = repo.AttachmentFileInfo{LocalPath: path, Exists: statFile(path)}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2. Batch items: use their path only if absent in store or newer version.
	for _, it := range batch {
		dims := zotero.ItemDims(it.Data)
		if dims.ParentKey == "" || dims.ItemType != "attachment" {
			continue
		}
		if sv, ok := storeVer[dims.Key]; ok && sv > it.Version {
			continue // rejected older delta: keep committed path/hash
		}
		path := zotero.LocalFilePath(itemLocalPathFor(it))
		out[dims.Key] = repo.AttachmentFileInfo{LocalPath: path, Exists: statFile(path)}
	}

	// 3. Hash/stat every attachment file.
	res := map[string]repo.AttachmentFileInfo{}
	for key, fi := range out {
		res[key] = s.statAndHash(fi)
	}
	return res, nil
}

func statFile(path string) bool {
	i, err := os.Stat(path)
	return err == nil && i.Mode().IsRegular()
}

func (s *Service) statAndHash(fi repo.AttachmentFileInfo) repo.AttachmentFileInfo {
	info, err := os.Stat(fi.LocalPath)
	if err != nil {
		return classifyFileError(fi.LocalPath, err, nil)
	}
	if !info.Mode().IsRegular() {
		return repo.AttachmentFileInfo{LocalPath: fi.LocalPath, Exists: false,
			ErrCode: "FILE_NOT_FOUND", ErrMsg: "not a regular file", Retryable: false}
	}
	hash, herr := zotero.ContentHash(fi.LocalPath)
	if herr != nil {
		return classifyFileError(fi.LocalPath, herr, info)
	}
	return repo.AttachmentFileInfo{
		LocalPath: fi.LocalPath, Exists: true, Hash: hash,
		FileSize: info.Size(), MtimeMS: info.ModTime().UnixMilli(),
	}
}

// classifyFileError maps a concrete os.Stat/read error onto FILE_NOT_FOUND
// (absent, non-retryable) or IO_ERROR (permission / transient I/O, retryable)
// so a temporary failure is not permanently dropped from the job queue.
func classifyFileError(path string, err error, info os.FileInfo) repo.AttachmentFileInfo {
	base := repo.AttachmentFileInfo{LocalPath: path, Exists: false}
	if info != nil {
		base.FileSize = info.Size()
	}
	if os.IsNotExist(err) {
		return repo.AttachmentFileInfo{LocalPath: path, Exists: false,
			ErrCode: "FILE_NOT_FOUND", ErrMsg: fmt.Sprintf("file missing: %s", path), Retryable: false}
	}
	return repo.AttachmentFileInfo{LocalPath: path, Exists: false,
		ErrCode: "IO_ERROR", ErrMsg: fmt.Sprintf("read file %s: %v", path, err), Retryable: true}
}

func itemLocalPathFromEnv(env []byte) string {
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

func itemLocalPathFor(it zotero.CanonicalItem) string {
	var e struct {
		Links struct {
			Enclosure struct {
				Href string `json:"href"`
			} `json:"enclosure"`
		} `json:"links"`
	}
	_ = json.Unmarshal(it.Envelope, &e)
	return e.Links.Enclosure.Href
}
