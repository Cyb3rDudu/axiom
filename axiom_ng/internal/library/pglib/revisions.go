// revisions.go — the source revision Mits-Schrieb (F06 #300 Ziel 8),
// PostgreSQL dialect (the legacy Zotero-sync lane is a PostgreSQL-profile
// feature: it reads the zotero_* mirror on the shared database; the
// SQLite profile has no mirror and does not wire this lane).
// Revisions are published at the points where Zotero state changes are
// observed today: sync completion (RecordSyncRevisions) and heal/custody
// (RecordAttachmentRevision — both the fixer auto-apply and the manual
// custody route run through repair.Apply). Additive only: the Store turns
// onto revision intake in F09; until then these rows are the historized
// publication ledger.
//
// Idempotence: a revision row is minted only when the rendition's content
// hash (or rendition identity) CHANGED — a re-sync that changes nothing
// rewrites nothing. Revision ids stay monotonic per (source, record).
package pglib

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
)

// RecordSyncRevisions walks the active attachments of the source and
// publishes a revision per rendition whose state changed. The count is
// MINTED revisions only — an idempotent re-sync that changes nothing
// walks its attachments but mints (and reports) zero.
func (s *Store) RecordSyncRevisions(ctx context.Context, sourceID string) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.zotero_key, a.zotero_key, COALESCE(a.content_hash,''), COALESCE(a.content_type,''),
		       COALESCE(a.filename,''), COALESCE(d.citation_class,'citable')
		FROM zotero_attachments a
		JOIN zotero_documents d ON d.id = a.document_id
		WHERE a.source_id::text = $1 AND a.deleted = false AND d.deleted = false
		  AND a.content_hash IS NOT NULL AND a.content_hash <> ''`, sourceID)
	if isMissingRelation(err) {
		// A library-only database (own DSN, F12 split) has no Zotero
		// mirror — the documented strangler absence, not an error.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type att struct {
		docKey, attKey, hash, ct, fn, class string
	}
	var atts []att
	for rows.Next() {
		var a att
		if err := rows.Scan(&a.docKey, &a.attKey, &a.hash, &a.ct, &a.fn, &a.class); err != nil {
			return 0, err
		}
		atts = append(atts, a)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	published := 0
	for _, a := range atts {
		minted, err := s.recordMirrorRevision(ctx, sourceID, a.docKey, a.attKey, a.hash, mediaTypeFromContent(a.ct), "sync")
		if err != nil {
			return published, err
		}
		if minted {
			published++
		}
	}
	return published, nil
}

// RecordAttachmentRevision publishes one attachment's revision (heal).
func (s *Store) RecordAttachmentRevision(ctx context.Context, sourceID, documentKey, attachmentKey, contentHash, mediaType string) error {
	if mediaType == "" {
		mediaType = revision.MediaTypePDF
	}
	_, err := s.recordMirrorRevision(ctx, sourceID, documentKey, attachmentKey, contentHash, mediaType, "heal")
	return err
}

// recordMirrorRevision derives the bibliography from the Zotero mirror
// and publishes the revision (idempotent on unchanged content); it
// reports whether a NEW revision was minted.
func (s *Store) recordMirrorRevision(ctx context.Context, sourceID, documentKey, attachmentKey, contentHash, mediaType, origin string) (bool, error) {
	if contentHash == "" {
		return false, nil // nothing verifiable to publish — skip honestly
	}
	var (
		title, publisher, language, class string
		creators                          []byte
		year                              *int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT d.title, COALESCE(d.publisher,''), COALESCE(d.language,''), d.creators, d.publication_year,
		       COALESCE(d.citation_class,'citable')
		FROM zotero_documents d
		WHERE d.source_id::text = $1 AND d.zotero_key = $2 AND d.deleted = false`,
		sourceID, documentKey).Scan(&title, &publisher, &language, &creators, &year, &class)
	if isMissingRelation(err) {
		return false, nil // no mirror — absence, not an error (see above)
	}
	if err != nil {
		return false, fmt.Errorf("revision mitschrieb document %s: %w", documentKey, err)
	}
	bib := revision.Bibliography{
		RecordID: documentKey, Title: title, Publisher: publisher, Language: language,
		Year: year, CitationClass: class,
	}
	if len(creators) > 0 {
		var cs []library.Creator
		if json.Unmarshal(creators, &cs) == nil {
			for _, c := range cs {
				name := c.Name
				if name == "" {
					name = c.FirstName + " " + c.LastName
				}
				if name != "" {
					bib.Authors = append(bib.Authors, name)
				}
			}
		}
	}
	_, minted, err := s.PublishRevision(ctx, library.SourceRevisionDomain{
		SourceID:     sourceID,
		RecordID:     documentKey,
		RenditionID:  attachmentKey,
		ContentHash:  contentHash,
		MediaType:    mediaType,
		Bibliography: bib,
		LocatorCapabilities: revision.LocatorCapabilities{
			Page: &revision.PageCapability{Trust: revision.TrustPhysicalOnly},
		},
		ContentTicket: "zat:" + sourceID + ":" + attachmentKey,
		Origin:        origin,
		CreatedAt:     time.Now(),
	})
	return minted, err
}

// mediaTypeFromContent maps the mirror's content_type onto the revision
// media vocabulary.
func mediaTypeFromContent(ct string) string {
	if ct == "" {
		return revision.MediaTypePDF
	}
	return ct
}

// (NewRevisionPublisher was removed with F12: the composition wires the
// concrete engine directly — the adapter had no callers.)
