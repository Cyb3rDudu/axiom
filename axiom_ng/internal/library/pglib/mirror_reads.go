// mirror_reads.go — the Zotero-mirror strangler reads (PostgreSQL
// dialect, F12 #306): the zotero_* tables are Library-owned but live on
// the shared legacy database. A missing mirror (standalone library DB)
// reads as absence — the neutral contract the service degrades on.
package pglib

import (
	"context"
	"errors"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
)

// mirrorAbsent folds the strangler reads' absence signals: no-rows AND a
// missing mirror relation (42P01 — the Library schema runs standalone on
// a library-only database). Library-owned rows NEVER take this path (a
// missing library_* relation stays a raw error — schema fault, not data
// absence; see absent()).
func mirrorAbsent(err error) error {
	if isMissingRelation(err) {
		return library.ErrRowAbsent
	}
	return absent(err)
}

// LastSyncAt resolves the mirror's last sync time for the source (nil,
// nil when the source is unknown or no mirror exists).
func (s *Store) LastSyncAt(ctx context.Context, sourceID string) (*time.Time, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT last_sync_at FROM zotero_sources WHERE id::text = $1`, sourceID).Scan(&t)
	if err != nil {
		// mirrorAbsent folds the missing mirror (42P01) and the absent
		// row into the one sentinel — both degrade to "never synced".
		if errors.Is(mirrorAbsent(err), library.ErrRowAbsent) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// MirrorRenditionPath resolves the local staging path of a mirror
// rendition ("" + library.ErrRowAbsent when unknown or no mirror).
func (s *Store) MirrorRenditionPath(ctx context.Context, sourceID, attachmentKey string) (string, error) {
	var local string
	err := s.pool.QueryRow(ctx,
		`SELECT local_path FROM zotero_attachments WHERE source_id::text = $1 AND zotero_key = $2 AND deleted = false`,
		sourceID, attachmentKey).Scan(&local)
	if err != nil {
		return "", mirrorAbsent(err)
	}
	return local, nil
}

// MirrorCitation loads the mirror's citation projection of a record
// (nil, false, nil when unknown or no mirror).
func (s *Store) MirrorCitation(ctx context.Context, sourceID, recordID string) (*library.MirrorCitation, bool, error) {
	var mc library.MirrorCitation
	err := s.pool.QueryRow(ctx, `
		SELECT d.title, COALESCE(d.publisher,''), COALESCE(d.language,''), d.creators, d.publication_year,
			COALESCE(d.citation_class, 'citable')
		FROM zotero_documents d
		WHERE d.zotero_key = $1 AND d.source_id::text = $2 AND d.deleted = false`,
		recordID, sourceID).Scan(&mc.Title, &mc.Publisher, &mc.Language, &mc.Creators, &mc.Year, &mc.CitationClass)
	if err != nil {
		if errors.Is(mirrorAbsent(err), library.ErrRowAbsent) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &mc, true, nil
}

// ResumeInflightIDs lists imports still in an inflight status (the
// boot-resume scan).
func (s *Store) ResumeInflightIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT import_id FROM library_imports
		WHERE status NOT IN ('committed','retryable_failed','terminal_failed','awaiting_confirmation')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReferencedStagingHashes lists the content hashes imports still
// reference (the retention set).
func (s *Store) ReferencedStagingHashes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT staging_sha256 FROM library_imports`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	referenced := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return referenced, err
		}
		referenced[sha] = true
	}
	return referenced, rows.Err()
}
