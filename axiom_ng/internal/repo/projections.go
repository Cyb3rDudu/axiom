// Normalized document/attachment projections derived from the FULL active
// state of zotero_items, with version guards, preferred/hash/stat writing and
// SQL-NULL semantics.
package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
	"github.com/jackc/pgx/v5"
)

// CanonicalDocFlag identifies a document/attachment pair to enqueue after a
// canonical apply (the preferred, processable attachment of a bibliographic
// parent).
type CanonicalDocFlag struct {
	DocumentZoteroKey string
	AttachmentKey     string
	LocalPath         string
}

// fullProjections is the outcome of deriving projections from zotero_items.
type fullProjections struct {
	flags   []CanonicalDocFlag
	pending []PendingJob
	failed  []FailedJob
}

// attMeta holds canonical attachment-item dimensions used for projection.
type attMeta struct {
	key         string
	parent      string
	version     int64
	itemType    string
	deleted     bool
	fileName    string
	contentType string
	localPath   string
	linkMode    string
}

// deriveFullProjections reads all active zotero_items, groups parents vs
// attachments, writes normalized document/attachment projections (version
// guarded, preferred/hash/stats), marks documents that lost their preferred
// processable attachment and removed attachments as deleted, and builds the
// pending (or failed, for missing files) ingest jobs.
func (r *Repo) deriveFullProjections(ctx context.Context, tx pgx.Tx, sourceID string, files map[string]AttachmentFileInfo) (fullProjections, error) {
	rows, err := tx.Query(ctx, `
		SELECT zotero_key, COALESCE(parent_key,''), COALESCE(item_type,''), raw_data::text, raw_envelope::text, zotero_version, deleted
		FROM zotero_items WHERE source_id=$1`, sourceID)
	if err != nil {
		return fullProjections{}, fmt.Errorf("query items: %w", err)
	}
	defer rows.Close()

	type parent struct {
		key     string
		version int64
		deleted bool
		nm      zotero.NormalizedMetadata
	}
	parents := map[string]parent{}
	var parentOrder []string
	atts := map[string][]zotero.Attachment{}
	allAtts := map[string]attMeta{}

	for rows.Next() {
		var key, pkey, itype, rawData, rawEnv string
		var ver int64
		var deleted bool
		if err := rows.Scan(&key, &pkey, &itype, &rawData, &rawEnv, &ver, &deleted); err != nil {
			return fullProjections{}, err
		}
		if pkey == "" {
			// top-level: parent items (including deleted ones so their existing
			// document/attachment projections can be deactivated).
			parentOrder = append(parentOrder, key)
			parents[key] = parent{key: key, version: ver, deleted: deleted, nm: zotero.Normalize(json.RawMessage(rawData))}
			continue
		}
		// Children. Only item_type='attachment' may become an attachment
		// projection; notes/annotations are canonical-only and never projected.
		am := attMeta{
			key:         key,
			parent:      pkey,
			version:     ver,
			itemType:    itype,
			deleted:     deleted,
			fileName:    itemFilename(json.RawMessage(rawData)),
			contentType: itemContentType(json.RawMessage(rawData)),
			localPath:   itemLocalPath(json.RawMessage(rawEnv)),
			linkMode:    itemLinkMode(json.RawMessage(rawData)),
		}
		allAtts[key] = am
		if itype != "attachment" {
			continue // note / annotation / etc.: never projected into zotero_attachments
		}
		atts[pkey] = append(atts[pkey], zotero.Attachment{
			Key: key, Version: ver, ParentKey: pkey,
			ContentType: am.contentType, Filename: am.fileName, LocalPath: am.localPath,
		})
	}

	var out fullProjections
	for _, parentKey := range parentOrder {
		p := parents[parentKey]
		projectable := isProjectable(p.nm.ItemType) && !p.deleted
		if !projectable {
			// Deleted parent OR type no longer projectable (e.g. book -> note):
			// deactivate any stale document + attachment projections so a former
			// book does not keep an active, preferred projection after becoming
			// a note. No-ops when no projection existed.
			if err := r.deactivateDocument(ctx, tx, sourceID, parentKey); err != nil {
				return fullProjections{}, err
			}
			if err := r.deactivateDocumentAttachments(ctx, tx, sourceID, parentKey); err != nil {
				return fullProjections{}, err
			}
			continue
		}
		// Preferred only from ACTIVE attachment items (deleted ones are excluded),
		// deterministically ordered so selection is stable.
		pref := preferredActive(atts[parentKey], allAtts)
		if pref == nil {
			// No active processable attachment remains: mark the doc projection
			// deleted (along with any attachment projections).
			if err := r.deactivateDocument(ctx, tx, sourceID, parentKey); err != nil {
				return fullProjections{}, err
			}
			if err := r.deactivateDocumentAttachments(ctx, tx, sourceID, parentKey); err != nil {
				return fullProjections{}, err
			}
			continue
		}
		docID, err := r.ensureDocumentProjection(ctx, tx, sourceID, parentKey, p.version, p.nm)
		if err != nil {
			return fullProjections{}, err
		}
		// Write all attachment projections (version-guarded); only the preferred
		// becomes preferred=true; deleted attachments are marked deleted.
		attIDs := map[string]string{}
		for _, att := range atts[parentKey] {
			deleted := allAtts[att.Key].deleted
			lm := allAtts[att.Key].linkMode
			if lm == "" {
				lm = "imported_file"
			}
			aid, err := r.upsertAttachmentProjection(ctx, tx, sourceID, docID, parentKey, att, deleted, lm)
			if err != nil {
				return fullProjections{}, err
			}
			attIDs[att.Key] = aid
		}
		// Exactly one preferred per document.
		fin := files[pref.Key]
		if err := r.setPreferredWithStats(ctx, tx, sourceID, docID, pref, fin); err != nil {
			return fullProjections{}, err
		}
		if err := r.clearSiblingPreferred(ctx, tx, sourceID, docID, pref.Key); err != nil {
			return fullProjections{}, err
		}
		if fin.Exists && fin.Hash != "" {
			out.pending = append(out.pending, PendingJob{
				SourceID: sourceID, DocumentID: docID, AttachmentID: attIDs[pref.Key], ContentHash: fin.Hash,
			})
		} else {
			code, msg, retryable := "FILE_NOT_FOUND", "local file missing", false
			if fin.ErrCode != "" {
				code, msg = fin.ErrCode, fin.ErrMsg
				retryable = fin.Retryable
			}
			out.failed = append(out.failed, FailedJob{
				SourceID: sourceID, DocumentID: docID, AttachmentID: attIDs[pref.Key],
				ErrorCode: code, ErrorMessage: msg, Retryable: retryable,
			})
		}
		out.flags = append(out.flags, CanonicalDocFlag{DocumentZoteroKey: parentKey, AttachmentKey: pref.Key, LocalPath: pref.LocalPath})
	}
	return out, nil
}

// preferredActive picks the preferred attachment from ACTIVE (not deleted)
// attachment items. It filters out deleted attachments, sorts the remaining by
// key for a deterministic order, then reuses the central
// zotero.PreferredAttachment selection (PDF over EPUB; detects
// application/epub, application/epub+zip, vendor MIME, and .epub filename).
func preferredActive(atts []zotero.Attachment, deleted map[string]attMeta) *zotero.Attachment {
	var active []zotero.Attachment
	for _, a := range atts {
		if meta, ok := deleted[a.Key]; ok && meta.deleted {
			continue // deleted attachments are never preferred
		}
		active = append(active, a)
	}
	// Deterministic order: the central selector takes the first processable match.
	sort.SliceStable(active, func(i, j int) bool { return active[i].Key < active[j].Key })
	return zotero.PreferredAttachment(active)
}

// deactivateDocumentAttachments marks all attachment projections of a document
// as deleted (used when a parent is deleted or loses its processable file).
func (r *Repo) deactivateDocumentAttachments(ctx context.Context, tx pgx.Tx, sourceID, parentKey string) error {
	_, err := tx.Exec(ctx, `
		UPDATE zotero_attachments
		SET deleted=true, preferred=false, updated_at=now()
		WHERE source_id=$1 AND parent_zotero_key=$2 AND deleted=false
	`, sourceID, parentKey)
	if err != nil {
		return fmt.Errorf("deactivate document attachments %s: %w", parentKey, err)
	}
	return nil
}

// ensureDocumentProjection writes a normalized, version-guarded zotero_documents
// projection (missing optional values as SQL NULL, never 0/”).
func (r *Repo) ensureDocumentProjection(ctx context.Context, tx pgx.Tx, sourceID, parentKey string, version int64, nm zotero.NormalizedMetadata) (string, error) {
	creators, _ := json.Marshal(nm.Creators)
	tags, _ := json.Marshal(nm.Tags)
	cols, _ := json.Marshal(nm.Collections)
	year := (*int)(nil)
	if nm.PublicationYear != nil {
		year = nm.PublicationYear
	}
	// NULL for empty optional strings.
	strNull := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	meta := map[string]any{}
	put := func(k string, v any) {
		if x, ok := v.(string); ok && x == "" {
			meta[k] = nil
		} else {
			meta[k] = v
		}
	}
	put("edition", strNull(nm.Edition))
	put("volume", strNull(nm.Volume))
	put("issue", strNull(nm.Issue))
	put("pages", strNull(nm.Pages))
	put("issn", strNull(nm.ISSN))
	put("extra", strNull(nm.Extra))
	if nm.Relations != nil {
		// Use the raw map so it serializes as a JSON object, NOT a base64 string.
		meta["relations"] = nm.Relations
	} else {
		meta["relations"] = nil
	}
	metaJSON, _ := json.Marshal(meta)

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO zotero_documents (
			source_id, zotero_key, zotero_version, item_type, title, creators,
			publication_year, publication_date, publisher, isbn, doi, url,
			language, abstract_note, tags, collections, metadata, canonical_item_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
			(SELECT id FROM zotero_items WHERE source_id=$1 AND zotero_key=$2))
		ON CONFLICT (source_id, zotero_key) DO UPDATE SET
			zotero_version = GREATEST(zotero_documents.zotero_version, EXCLUDED.zotero_version),
			item_type=EXCLUDED.item_type, title=EXCLUDED.title, creators=EXCLUDED.creators,
			publication_year=EXCLUDED.publication_year, publication_date=EXCLUDED.publication_date,
			publisher=EXCLUDED.publisher, isbn=EXCLUDED.isbn, doi=EXCLUDED.doi, url=EXCLUDED.url,
			language=EXCLUDED.language, abstract_note=EXCLUDED.abstract_note, tags=EXCLUDED.tags,
			collections=EXCLUDED.collections, metadata=EXCLUDED.metadata,
			canonical_item_id=EXCLUDED.canonical_item_id, deleted=false, updated_at=now()
		WHERE EXCLUDED.zotero_version >= zotero_documents.zotero_version
		RETURNING id::text
	`, sourceID, parentKey, version, nm.ItemType, nm.Title, creators, year, strNull(nm.Date), strNull(nm.Publisher),
		strNull(nm.ISBN), strNull(nm.DOI), strNull(nm.URL), strNull(nm.Language), strNull(nm.Abstract), tags, cols, metaJSON).Scan(&id)
	if err == pgx.ErrNoRows {
		// Older version: return the existing id.
		if e2 := tx.QueryRow(ctx, `SELECT id::text FROM zotero_documents WHERE source_id=$1 AND zotero_key=$2`,
			sourceID, parentKey).Scan(&id); e2 != nil {
			return "", fmt.Errorf("lookup document projection %s: %w", parentKey, e2)
		}
		return id, nil
	}
	if err != nil {
		return "", fmt.Errorf("upsert document projection %s: %w", parentKey, err)
	}
	return id, nil
}

// upsertAttachmentProjection writes a version-guarded attachment projection and
// returns its id. linkMode comes from the Zotero raw data (imported_file,
// linked_file, imported_url, ...).
func (r *Repo) upsertAttachmentProjection(ctx context.Context, tx pgx.Tx, sourceID, docID, parentKey string, att zotero.Attachment, deleted bool, linkMode string) (string, error) {
	native := zotero.LocalFilePath(att.LocalPath)
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO zotero_attachments (
			source_id, document_id, zotero_key, zotero_version, parent_zotero_key,
			link_mode, content_type, filename, file_uri, local_path, preferred, deleted, canonical_item_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false,$11,
			(SELECT id FROM zotero_items WHERE source_id=$1 AND zotero_key=$3))
		ON CONFLICT (source_id, zotero_key) DO UPDATE SET
			zotero_version=GREATEST(zotero_attachments.zotero_version, EXCLUDED.zotero_version),
			document_id=EXCLUDED.document_id,
			link_mode=EXCLUDED.link_mode, content_type=EXCLUDED.content_type,
			filename=EXCLUDED.filename, file_uri=EXCLUDED.file_uri, local_path=EXCLUDED.local_path,
			deleted=EXCLUDED.deleted,
			canonical_item_id=EXCLUDED.canonical_item_id, updated_at=now()
		WHERE EXCLUDED.zotero_version >= zotero_attachments.zotero_version
		RETURNING id::text
	`, sourceID, docID, att.Key, att.Version, parentKey, linkMode, att.ContentType, att.Filename, att.LocalPath, native, deleted).Scan(&id)
	if err == pgx.ErrNoRows {
		// Older version: return existing id.
		if e2 := tx.QueryRow(ctx, `SELECT id::text FROM zotero_attachments WHERE source_id=$1 AND zotero_key=$2`,
			sourceID, att.Key).Scan(&id); e2 != nil {
			return "", fmt.Errorf("lookup attachment projection %s: %w", att.Key, e2)
		}
		return id, nil
	}
	if err != nil {
		return "", fmt.Errorf("upsert attachment projection %s: %w", att.Key, err)
	}
	return id, nil
}

// setPreferredWithStats marks one attachment as the document's preferred file
// and writes its hash/size/mtime (version-guarded is implicit via the preferred
// flag being idempotent).
func (r *Repo) setPreferredWithStats(ctx context.Context, tx pgx.Tx, sourceID, docID string, att *zotero.Attachment, fin AttachmentFileInfo) error {
	var sz, mtm *int64
	var hash *string
	if fin.Exists {
		sz = &fin.FileSize
		mtm = &fin.MtimeMS
		hash = &fin.Hash
	}
	_, err := tx.Exec(ctx, `
		UPDATE zotero_attachments
		SET preferred=true, content_hash=$3, file_size=$4, mtime_ms=$5, updated_at=now()
		WHERE source_id=$1 AND zotero_key=$2
	`, sourceID, att.Key, hash, sz, mtm)
	if err != nil {
		return fmt.Errorf("set preferred for %s: %w", att.Key, err)
	}
	return nil
}

// clearSiblingPreferred sets all other attachments of a document to preferred=false.
func (r *Repo) clearSiblingPreferred(ctx context.Context, tx pgx.Tx, sourceID, docID, keepKey string) error {
	_, err := tx.Exec(ctx, `
		UPDATE zotero_attachments SET preferred=false, updated_at=now()
		WHERE source_id=$1 AND document_id=$2 AND preferred=true AND zotero_key <> $3
	`, sourceID, docID, keepKey)
	if err != nil {
		return fmt.Errorf("clear sibling preferred: %w", err)
	}
	return nil
}

// deactivateDocument marks an active document projection deleted when it has no
// remaining processable attachment.
func (r *Repo) deactivateDocument(ctx context.Context, tx pgx.Tx, sourceID, parentKey string) error {
	_, err := tx.Exec(ctx, `
		UPDATE zotero_documents SET deleted=true, updated_at=now()
		WHERE source_id=$1 AND zotero_key=$2 AND deleted=false
	`, sourceID, parentKey)
	if err != nil {
		return fmt.Errorf("deactivate document %s: %w", parentKey, err)
	}
	return nil
}
