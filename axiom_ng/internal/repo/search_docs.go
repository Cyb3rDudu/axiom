package repo

import (
	"context"
	"encoding/json"
	"strings"
)

// documentMetaRow is the search source-hydration query shape (R3 #133):
// OS hits carry only document_id; title/authors/year/publisher live in
// zotero_documents.
type documentMetaRow struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Creators    json.RawMessage `json:"creators"`
	Year        *int            `json:"publication_year"`
	Publisher   string          `json:"publisher"`
	Language    string          `json:"language"`
	Tags        json.RawMessage `json:"tags"`
	ContentType string          `json:"content_type"`
}

// zoteroCreator matches zotero.Creator's persisted JSONB shape.
type zoteroCreator struct {
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Name        string `json:"name"`
	CreatorType string `json:"creatorType"`
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
}

// DocumentMetaByIDs returns metadata for the given zotero_documents ids.
// Missing ids are simply absent from the map (search degrades the source
// block, not the hit).
func (r *Repo) DocumentMetaByIDs(ctx context.Context, ids []string) (map[string]DocumentMeta, error) {
	out := make(map[string]DocumentMeta, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
			SELECT d.id::text, d.title, d.creators, d.publication_year, COALESCE(d.publisher, ''),
			       COALESCE(d.language, ''), COALESCE(d.tags, '[]'::jsonb), COALESCE(act.content_type, '')
			FROM zotero_documents d
			LEFT JOIN LATERAL (
				SELECT a.content_type
				FROM processing_snapshots s
				JOIN zotero_attachments a ON a.id = s.attachment_id
				WHERE s.document_id = d.id AND s.active
				LIMIT 1
			) act ON true
				WHERE d.id = ANY($1::uuid[]) AND NOT d.deleted`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row documentMetaRow
		if err := rows.Scan(&row.ID, &row.Title, &row.Creators, &row.Year, &row.Publisher, &row.Language, &row.Tags, &row.ContentType); err != nil {
			return nil, err
		}
		var cs []zoteroCreator
		_ = json.Unmarshal(row.Creators, &cs)
		authors := make([]string, 0, len(cs))
		for _, c := range cs {
			switch {
			case c.Name != "":
				authors = append(authors, c.Name)
			case c.FirstName != "" || c.LastName != "":
				authors = append(authors, strings.TrimSpace(c.FirstName+" "+c.LastName))
			}
		}
		var ts []struct {
			Tag string `json:"tag"`
		}
		_ = json.Unmarshal(row.Tags, &ts)
		tags := make([]string, 0, len(ts))
		for _, t := range ts {
			if t.Tag != "" {
				tags = append(tags, t.Tag)
			}
		}
		out[row.ID] = DocumentMeta{Title: row.Title, Authors: authors, Year: row.Year, Publisher: row.Publisher, Language: row.Language, Tags: tags, ContentType: row.ContentType}
	}
	return out, rows.Err()
}
