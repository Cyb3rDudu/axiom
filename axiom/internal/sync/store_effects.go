// store_effects.go — the sync's Store-side phase (#358): after the
// mirror apply commits on the Library database, this phase turns the
// changed-rendition set into Store truth in ONE transaction on the Store
// database:
//
//  1. per (selection-gated) rendition: upsert the store_documents
//     projection row, then mint the revision intake (processable files)
//     or record the failed ingest row (missing/unreadable files);
//  2. mark projections deleted for renditions the mirror deactivated
//     (claims obsolesce, snapshot reconciliation retires them);
//  3. reconcile attachment snapshots (retire deleted renditions' active
//     snapshots, restore live-but-unserved ones).
//
// The per-source advisory xact lock (repo.LockKey) serializes this phase
// against claims on the same database — the claim's lock is the same key
// in the same namespace, so a claim never freezes a snapshot from a
// mid-flight projection write.
package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/mirror"
	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
)

// bibRow is the document-side bibliography block read from the mirror
// (post-commit, post-citation-class-recompute — the class is final).
type bibRow struct {
	Title         string
	Creators      []byte
	Year          *int
	Publisher     string
	Language      string
	Tags          []byte
	CitationClass string
	DocVersion    int64
}

// readBibliographies loads the bibliographic block for the changed
// documents from the mirror (one query; Library database).
func (s *Service) readBibliographies(ctx context.Context, sourceID string, renditions []mirror.SyncRendition) (map[string]bibRow, error) {
	ids := make([]string, 0, len(renditions))
	seen := map[string]bool{}
	for _, r := range renditions {
		if !seen[r.DocumentID] {
			seen[r.DocumentID] = true
			ids = append(ids, r.DocumentID)
		}
	}
	out := map[string]bibRow{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.mir().Pool().Query(ctx, `
		SELECT id::text, title, creators, publication_year,
		       COALESCE(publisher,''), COALESCE(language,''), tags,
		       COALESCE(citation_class,'citable'), zotero_version
		FROM zotero_documents
		WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var b bibRow
		if err := rows.Scan(&id, &b.Title, &b.Creators, &b.Year, &b.Publisher, &b.Language, &b.Tags, &b.CitationClass, &b.DocVersion); err != nil {
			return nil, err
		}
		out[id] = b
	}
	return out, rows.Err()
}

// authorStrings flattens the mirror's creator JSONB into the revision
// contract's author strings (the same projection recordMirrorRevision
// freezes into its revisions).
func authorStrings(creators []byte) []string {
	var cs []struct {
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
		Name      string `json:"name"`
	}
	if json.Unmarshal(creators, &cs) != nil {
		return nil
	}
	var out []string
	for _, c := range cs {
		name := c.Name
		if name == "" {
			name = c.FirstName + " " + c.LastName
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func tagStrings(tags []byte) []string {
	var ts []struct {
		Tag string `json:"tag"`
	}
	if json.Unmarshal(tags, &ts) != nil {
		return nil
	}
	var out []string
	for _, t := range ts {
		if t.Tag != "" {
			out = append(out, t.Tag)
		}
	}
	return out
}

// buildSyncRevision assembles the revision intake artifact for one
// rendition (the same shape recordMirrorRevision publishes, with the
// rendition's Zotero item version as the monotonic revision id).
func buildSyncRevision(sourceID string, r mirror.SyncRendition, b bibRow) revision.SourceRevision {
	class := b.CitationClass
	if class != revision.CitationClassContextual {
		class = revision.CitationClassCitable
	}
	media := r.ContentType
	if media == "" {
		media = revision.MediaTypePDF
	}
	rev := revision.SourceRevision{
		SourceID:    sourceID,
		RevisionID:  strconv.FormatInt(r.Version, 10),
		RenditionID: r.AttachmentKey,
		ContentHash: r.Hash,
		MediaType:   media,
		Bibliography: revision.Bibliography{
			RecordID:      r.DocumentKey,
			Title:         b.Title,
			Authors:       authorStrings(b.Creators),
			Year:          b.Year,
			Publisher:     b.Publisher,
			Language:      b.Language,
			Tags:          tagStrings(b.Tags),
			CitationClass: class,
		},
		LocatorCapabilities: revision.LocatorCapabilities{
			Page: &revision.PageCapability{Trust: revision.TrustPhysicalOnly},
		},
		ContentTicket: "zat:" + sourceID + ":" + r.AttachmentKey,
	}
	return rev
}

// revisionFingerprint digests the canonical revision JSON into a short
// stable key component (sha256, first 16 hex chars).
func revisionFingerprint(revJSON []byte) string {
	sum := sha256.Sum256(revJSON)
	return hex.EncodeToString(sum[:8])
}

// applyStoreEffects runs the sync's Store phase. Returns minted intake
// jobs and recorded failed rows.
func (s *Service) applyStoreEffects(ctx context.Context, sourceID, serverID string, res mirror.CanonicalApplyResult, selection map[string]string) (int, int, error) {
	if s.store == nil {
		return 0, 0, errors.New("sync: no store handle for the store-effect phase")
	}
	bibs, err := s.readBibliographies(ctx, sourceID, res.Renditions)
	if err != nil {
		return 0, 0, fmt.Errorf("read bibliographies: %w", err)
	}

	enqueued := 0
	var failed []repo.FailedJob
	tx, err := s.store.Pool().Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	// Serialize against claims for this source (same key, same database —
	// the claim's loadAndLockRevisionState takes the xact twin).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, repo.LockKey(sourceID)); err != nil {
		return 0, 0, fmt.Errorf("store-effect source lock: %w", err)
	}

	for _, r := range res.Renditions {
		if mirror.JobGated(selection, r.DocumentID) {
			continue // excluded: no projection churn, no jobs (#166)
		}
		b := bibs[r.DocumentID]
		var fileSize, mtime *int64
		if r.ErrCode == "" {
			fs, mt := r.FileSize, r.MtimeMS
			fileSize, mtime = &fs, &mt
		}
		proj := repo.DocumentProjection{
			DocumentID: r.DocumentID, AttachmentID: r.AttachmentID,
			SourceID: sourceID, ServerID: serverID,
			RecordKey: r.DocumentKey, RenditionKey: r.AttachmentKey,
			SourceVersion: r.Version,
			Title:         b.Title, Creators: authorStrings(b.Creators), Year: b.Year,
			Publisher: b.Publisher, Language: b.Language, Tags: tagStrings(b.Tags),
			CitationClass: b.CitationClass, ContentType: r.ContentType,
			ItemType: "", Filename: r.Filename, LocalPath: r.LocalPath,
			FileSize: fileSize, MtimeMS: mtime, LinkMode: r.LinkMode,
		}
		if r.ErrCode != "" {
			failed = append(failed, repo.FailedJob{
				SourceID: sourceID, DocumentID: r.DocumentID, AttachmentID: r.AttachmentID,
				ErrorCode: r.ErrCode, ErrorMessage: r.ErrMsg, Retryable: r.Retryable,
			})
			// A failed file still projects (identity + metadata + path):
			// the listing and the outcome read it.
			if err := s.store.UpsertDocumentProjectionTx(ctx, tx, proj); err != nil {
				return 0, 0, fmt.Errorf("upsert projection (failed file) %s: %w", r.AttachmentKey, err)
			}
			continue
		}
		proj.ContentHash = &r.Hash
		if err := s.store.UpsertDocumentProjectionTx(ctx, tx, proj); err != nil {
			return 0, 0, fmt.Errorf("upsert projection %s: %w", r.AttachmentKey, err)
		}
		rev := buildSyncRevision(sourceID, r, b)
		revJSON, err := json.Marshal(rev)
		if err != nil {
			return 0, 0, fmt.Errorf("marshal revision %s: %w", r.AttachmentKey, err)
		}
		// The key derives from the revision CONTENT: identical offers
		// replay cleanly, ANY change (content hash, rendition version,
		// bibliography — Zotero may re-offer metadata at an equal item
		// version; the projection upserts tolerate it) yields a new key and
		// the identity dedup / #294 suppression decide join-vs-mint. A key
		// mismatch is impossible by construction.
		intakeKey := "sync:" + sourceID + ":" + r.AttachmentKey + ":" + revisionFingerprint(revJSON)
		_, minted, err := s.store.EnqueueRevisionIntakeTx(ctx, tx, repo.IntakeRequest{
			IdempotencyKey:      intakeKey,
			RevisionSourceID:    sourceID,
			RevisionRecordID:    r.DocumentKey,
			RevisionRenditionID: r.AttachmentKey,
			RevisionNo:          rev.RevisionID,
			ContentHash:         r.Hash,
			RevisionJSON:        revJSON,
		})
		switch {
		case err == nil:
			if minted {
				enqueued++
				// Fresh work resolves the rendition's prior failures (the
				// intake-mint successor of the legacy pending resolution).
				if err := s.store.ResolveAttachmentFailuresTx(ctx, tx, r.AttachmentID); err != nil {
					return 0, 0, err
				}
			}
		case errors.Is(err, repo.ErrIntakeSuppressed):
			// #294: content already served — honest no-op.
		case errors.Is(err, repo.ErrIntakeKeyMismatch):
			return 0, 0, fmt.Errorf("sync intake key mismatch for %s: %w", r.AttachmentKey, err)
		default:
			return 0, 0, fmt.Errorf("sync intake %s: %w", r.AttachmentKey, err)
		}
	}

	failedWritten, err := s.store.WriteFailedJobsTx(ctx, tx, failed)
	if err != nil {
		return 0, 0, err
	}
	if err := s.store.MarkProjectionsDeletedTx(ctx, tx, res.DeletedAttachmentIDs); err != nil {
		return 0, 0, fmt.Errorf("mark deleted projections: %w", err)
	}
	if err := s.store.ReconcileAttachmentSnapshotsTx(ctx, tx); err != nil {
		return 0, 0, fmt.Errorf("reconcile attachment snapshots: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return enqueued, failedWritten, nil
}
