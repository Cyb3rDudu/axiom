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
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Creators  json.RawMessage `json:"creators"`
	Year      *int            `json:"publication_year"`
	Publisher string          `json:"publisher"`
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
		SELECT id::text, title, creators, publication_year, publisher
		FROM zotero_documents
		WHERE id = ANY($1::uuid[]) AND NOT deleted`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row documentMetaRow
		if err := rows.Scan(&row.ID, &row.Title, &row.Creators, &row.Year, &row.Publisher); err != nil {
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
		out[row.ID] = DocumentMeta{Title: row.Title, Authors: authors, Year: row.Year, Publisher: row.Publisher}
	}
	return out, rows.Err()
}
