// Deterministic, LLM-free normalization of a Zotero item's raw data object into
// the fields projected onto zotero_documents. Raw JSON stays authoritative; the
// projection is a best-effort map used for querying.
package zotero

import "encoding/json"

// NormalizedMetadata is the structured projection of a Zotero item's data
// object. Missing values remain nil (NULL); publication_year is only set when
// unambiguously derivable and never 0.
type NormalizedMetadata struct {
	ItemType        string
	Title           string
	Date            string
	PublicationYear *int
	Publisher       string
	Edition         string
	Volume          string
	Issue           string
	Pages           string
	DOI             string
	ISBN            string
	ISSN            string
	URL             string
	Language        string
	Abstract        string
	Extra           string
	Creators        []Creator
	Tags            []Tag
	Collections     []string
	Relations       map[string]any
}

type rawItemData struct {
	ItemType     string `json:"itemType"`
	Title        string `json:"title"`
	NameOfAct    string `json:"nameOfAct,omitempty"`   // statute (methodology §2)
	DateEnacted  string `json:"dateEnacted,omitempty"` // statute
	Institution  string `json:"institution,omitempty"` // report (methodology §4)
	Date         string `json:"date,omitempty"`
	Publisher    string `json:"publisher,omitempty"`
	Edition      string `json:"edition,omitempty"`
	Volume       string `json:"volume,omitempty"`
	Issue        string `json:"issue,omitempty"`
	Pages        string `json:"pages,omitempty"`
	DOI          string `json:"DOI,omitempty"`
	ISBN         string `json:"ISBN,omitempty"`
	ISSN         string `json:"ISSN,omitempty"`
	URL          string `json:"url,omitempty"`
	Language     string `json:"language,omitempty"`
	AbstractNote string `json:"abstractNote,omitempty"`
	Extra        string `json:"extra,omitempty"`
	Creators     []struct {
		FirstName   string `json:"firstName"`
		LastName    string `json:"lastName"`
		Name        string `json:"name"`
		CreatorType string `json:"creatorType"`
	} `json:"creators,omitempty"`
	Tags        []json.RawMessage `json:"tags,omitempty"`
	Collections []string          `json:"collections,omitempty"`
	Relations   map[string]any    `json:"relations,omitempty"`
}

// ItemDims extracts key/itemType/parentKey from an item's raw data object.
type ItemDimsResult struct {
	Key       string
	ItemType  string
	ParentKey string
}

func ItemDims(data json.RawMessage) ItemDimsResult {
	var d struct {
		Key        string `json:"key"`
		ItemType   string `json:"itemType"`
		ParentItem string `json:"parentItem,omitempty"`
	}
	_ = json.Unmarshal(data, &d)
	return ItemDimsResult{Key: d.Key, ItemType: d.ItemType, ParentKey: d.ParentItem}
}

// Normalize parses a Zotero data object into the normalized projection. Year is
// only derived when the date string starts with a 4-digit, plausible year.
func Normalize(data json.RawMessage) NormalizedMetadata {
	var raw rawItemData
	_ = json.Unmarshal(data, &raw)

	nm := NormalizedMetadata{
		ItemType:    raw.ItemType,
		Title:       raw.Title,
		Date:        raw.Date,
		Publisher:   raw.Publisher,
		Edition:     raw.Edition,
		Volume:      raw.Volume,
		Issue:       raw.Issue,
		Pages:       raw.Pages,
		DOI:         raw.DOI,
		ISBN:        raw.ISBN,
		ISSN:        raw.ISSN,
		URL:         raw.URL,
		Language:    raw.Language,
		Abstract:    raw.AbstractNote,
		Extra:       raw.Extra,
		Collections: raw.Collections,
		Relations:   raw.Relations,
	}
	for _, c := range raw.Creators {
		nm.Creators = append(nm.Creators, Creator{
			FirstName:   c.FirstName,
			LastName:    c.LastName,
			Name:        c.Name,
			CreatorType: c.CreatorType,
		})
	}
	for _, tag := range raw.Tags {
		var t struct {
			Tag string `json:"tag"`
		}
		if err := json.Unmarshal(tag, &t); err == nil && t.Tag != "" {
			nm.Tags = append(nm.Tags, Tag{Tag: t.Tag})
		}
	}
	nm.PublicationYear = ParseYear(raw.Date)
	// Methodology-aware fallbacks (dudu's Zotero guide): statutes carry
	// nameOfAct/dateEnacted instead of title/date, reports carry institution
	// instead of publisher. Fills only — never overwrites a present field.
	if raw.ItemType == "statute" {
		if nm.Title == "" && raw.NameOfAct != "" {
			nm.Title = raw.NameOfAct
		}
		if nm.Date == "" && raw.DateEnacted != "" {
			nm.Date = raw.DateEnacted
			nm.PublicationYear = ParseYear(raw.DateEnacted)
		}
	}
	if raw.ItemType == "report" && nm.Publisher == "" && raw.Institution != "" {
		nm.Publisher = raw.Institution
	}
	return nm
}
