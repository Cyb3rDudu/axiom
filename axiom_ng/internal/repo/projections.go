package repo

import (
	"context"
	"encoding/json"
	"fmt"

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

// deriveDocumentProjections builds the normalized zotero_documents and
// zotero_attachments projections from the canonical zotero_items rows, marks
// projections whose canonical item was removed as deleted, and returns a flag
// per document whose preferred processable attachment should be enqueued. Runs
// on the caller-provided transaction.
func (r *Repo) deriveDocumentProjections(ctx context.Context, tx pgx.Tx, sourceID string, items []zotero.CanonicalItem) ([]CanonicalDocFlag, error) {
	// Group canonical items by parent key, keeping parents separate.
	parents := map[string]zotero.NormalizedMetadata{} // parent key -> normalized
	parentVersion := map[string]int64{}

	attByParent := map[string][]attachmentProjection{}
	var parentOrder []string

	for i := range items {
		it := items[i]
		dims := zotero.ItemDims(it.Data)
		if dims.ParentKey == "" {
			// a top-level (parent) item
			parents[dims.Key] = zotero.Normalize(it.Data)
			parentVersion[dims.Key] = it.Version
			parentOrder = append(parentOrder, dims.Key)
			continue
		}
		attByParent[dims.ParentKey] = append(attByParent[dims.ParentKey], attachmentProjection{
			Key:         dims.Key,
			ItemType:    dims.ItemType,
			ContentType: itemContentType(it.Data),
			Filename:    itemFilename(it.Data),
			LocalPath:   itemLocalPath(it.Envelope),
		})
	}

	var flags []CanonicalDocFlag
	for _, parentKey := range parentOrder {
		nm := parents[parentKey]
		if !isBibliographic(nm.ItemType) {
			// e.g. note/annotation attach to a parent; there is no separate
			// top-level note, but guard anyway.
			continue
		}
		var atts []zotero.Attachment
		for _, a := range attByParent[parentKey] {
			atts = append(atts, zotero.Attachment{Key: a.Key, ContentType: a.ContentType, Filename: a.Filename, LocalPath: a.LocalPath})
		}
		pref := zotero.PreferredAttachment(atts)
		if pref == nil {
			continue
		}
		docID, err := r.ensureDocumentProjection(ctx, tx, sourceID, parentKey, parentVersion[parentKey], nm)
		if err != nil {
			return nil, err
		}
		if err := r.writeAttachments(ctx, tx, sourceID, parentKey, docID, atts); err != nil {
			return nil, err
		}
		flags = append(flags, CanonicalDocFlag{
			DocumentZoteroKey: parentKey,
			AttachmentKey:     pref.Key,
			LocalPath:         pref.LocalPath,
		})
	}
	return flags, nil
}

type attachmentProjection struct {
	Key         string
	ItemType    string
	ContentType string
	Filename    string
	LocalPath   string
}

// isBibliographic reports whether a canonical item type may become a document
// projection (a bibliographic parent that can carry a processable file).
func isBibliographic(itemType string) bool {
	switch itemType {
	case "book", "bookSection", "journalArticle", "conferencePaper", "report",
		"thesis", "preprint", "magazineArticle", "newspaperArticle", "encyclopediaArticle",
		"dictionaryEntry", "standard", "manuscript", "webpage":
		return true
	default:
		// note, annotation, attachment, artwork, film, etc. are not bibliographic docs
		return false
	}
}

// ensureDocumentProjection writes a normalized zotero_documents row from a
// canonical parent's data and returns its id. The projection is version-guarded
// so an older incoming item cannot overwrite a newer projection.
func (r *Repo) ensureDocumentProjection(ctx context.Context, tx pgx.Tx, sourceID, parentKey string, version int64, nm zotero.NormalizedMetadata) (string, error) {
	creators, _ := json.Marshal(nm.Creators)
	tags, _ := json.Marshal(nm.Tags)
	cols, _ := json.Marshal(nm.Collections)
	rels, _ := json.Marshal(nm.Relations)
	year := 0
	if nm.PublicationYear != nil {
		year = *nm.PublicationYear
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO zotero_documents (
			source_id, zotero_key, zotero_version, item_type, title, creators,
			publication_year, publication_date, publisher, isbn, doi, url,
			language, abstract_note, tags, collections, metadata
		) VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,$11,$12,$13,$14,$15,$16,
			jsonb_build_object('edition',$17::text,'volume',$18::text,'issue',$19::text,'pages',$20::text,'issn',$21::text,'extra',$22::text,'relations',$23::jsonb))
		ON CONFLICT (source_id, zotero_key) DO UPDATE SET
			zotero_version = GREATEST(zotero_documents.zotero_version, EXCLUDED.zotero_version),
			item_type=EXCLUDED.item_type, title=EXCLUDED.title, creators=EXCLUDED.creators,
			publication_year=EXCLUDED.publication_year, publication_date=EXCLUDED.publication_date,
			publisher=EXCLUDED.publisher, isbn=EXCLUDED.isbn, doi=EXCLUDED.doi, url=EXCLUDED.url,
			language=EXCLUDED.language, abstract_note=EXCLUDED.abstract_note, tags=EXCLUDED.tags,
			collections=EXCLUDED.collections, metadata=EXCLUDED.metadata, deleted=false, updated_at=now()
		WHERE EXCLUDED.zotero_version >= zotero_documents.zotero_version
		RETURNING id::text
	`, sourceID, parentKey, version, nm.ItemType, nm.Title, creators, year, nm.Date, nm.Publisher,
		nm.ISBN, nm.DOI, nm.URL, nm.Language, nm.Abstract, tags, cols,
		nm.Edition, nm.Volume, nm.Issue, nm.Pages, nm.ISSN, nm.Extra, rels).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			// Older version: DO UPDATE WHERE was suppressed; return existing id.
			if e2 := tx.QueryRow(ctx, `SELECT id::text FROM zotero_documents WHERE source_id=$1 AND zotero_key=$2`,
				sourceID, parentKey).Scan(&id); e2 != nil {
				return "", fmt.Errorf("lookup document projection %s: %w", parentKey, e2)
			}
			return id, nil
		}
		return "", fmt.Errorf("upsert document projection %s: %w", parentKey, err)
	}
	return id, nil
}

// writeAttachments writes the attachment projections for a parent (upsert).
func (r *Repo) writeAttachments(ctx context.Context, tx pgx.Tx, sourceID, parentKey, docID string, atts []zotero.Attachment) error {
	for _, att := range atts {
		native := zotero.LocalFilePath(att.LocalPath)
		if _, err := tx.Exec(ctx, `
			INSERT INTO zotero_attachments (
				source_id, document_id, zotero_key, zotero_version, parent_zotero_key,
				link_mode, content_type, filename, file_uri, local_path
			) VALUES ($1,$2,$3,0,$4,'imported_file',$5,$6,$7,$8)
			ON CONFLICT (source_id, zotero_key) DO UPDATE SET
				content_type=EXCLUDED.content_type, filename=EXCLUDED.filename,
				file_uri=EXCLUDED.file_uri, local_path=EXCLUDED.local_path,
				deleted=false, updated_at=now()
		`, sourceID, docID, att.Key, parentKey, att.ContentType, att.Filename, att.LocalPath, native); err != nil {
			return fmt.Errorf("upsert attachment projection %s: %w", att.Key, err)
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
