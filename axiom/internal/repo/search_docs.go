package repo

import (
	"context"
	"encoding/json"
)

// documentMetaRow is the search source-hydration query shape (R3 #133):
// OS hits carry only document_id; the bibliographic block lives in the
// Store's own store_documents projection (#358 — the Zotero mirror is
// Library-side; titles travel at intake time).
type documentMetaRow struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Creators      json.RawMessage `json:"creators"`
	Year          *int            `json:"publication_year"`
	Publisher     string          `json:"publisher"`
	Language      string          `json:"language"`
	Tags          json.RawMessage `json:"tags"`
	ContentType   string          `json:"content_type"`
	CitationClass string          `json:"citation_class"`
}

// DocumentMeta is the bibliographic block for one document.
type DocumentMeta struct {
	Title     string
	Authors   []string
	Year      *int
	Publisher string
	Language  string
	Tags      []string
	// ContentType: the ACTIVE snapshot's attachment format ("" when
	// unknown) — feeds SourceView.ContentType (#196/#245).
	ContentType string
	// CitationClass (#255): citable | contextual — feeds
	// SourceView.CitationClass; "citable" when unknown (the column default
	// and the honest legacy answer).
	CitationClass string
}

// DocumentMetaByIDs returns metadata for the given document ids from the
// Store's projection. Missing ids are simply absent from the map (search
// degrades the source block, not the hit). A document with several
// rendition rows resolves to its preferred (then newest) row.
func (r *Repo) DocumentMetaByIDs(ctx context.Context, ids []string) (map[string]DocumentMeta, error) {
	out := make(map[string]DocumentMeta, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
	SELECT DISTINCT ON (document_id) d.document_id::text, d.title, d.creators, d.publication_year, d.publisher,
	       d.language, d.tags, COALESCE(act.content_type, ''), COALESCE(d.citation_class, 'citable')
			FROM store_documents d
			LEFT JOIN LATERAL (
				-- the ACTIVE snapshot's format: the projection row of the
				-- attachment the snapshot was processed from
				SELECT p2.content_type
				FROM processing_snapshots s
				JOIN store_documents p2 ON p2.attachment_id = s.attachment_id
				WHERE s.document_id = d.document_id AND s.active
				LIMIT 1
			) act ON true
				WHERE d.document_id = ANY($1::uuid[]) AND NOT d.deleted
				ORDER BY document_id, d.preferred DESC, d.updated_at DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row documentMetaRow
		if err := rows.Scan(&row.ID, &row.Title, &row.Creators, &row.Year, &row.Publisher, &row.Language, &row.Tags, &row.ContentType, &row.CitationClass); err != nil {
			return nil, err
		}
		var authors, tags []string
		_ = json.Unmarshal(row.Creators, &authors)
		_ = json.Unmarshal(row.Tags, &tags)
		if authors == nil {
			authors = []string{}
		}
		if tags == nil {
			tags = []string{}
		}
		out[row.ID] = DocumentMeta{Title: row.Title, Authors: authors, Year: row.Year, Publisher: row.Publisher, Language: row.Language, Tags: tags, ContentType: row.ContentType, CitationClass: row.CitationClass}
	}
	return out, rows.Err()
}
