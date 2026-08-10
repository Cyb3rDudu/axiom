package zotero

import (
	"encoding/json"
	"testing"
)

func TestNormalizeCreatorsRolesAndOrder(t *testing.T) {
	data := json.RawMessage(`{
		"itemType":"book","title":"T","date":"2010",
		"creators":[
			{"firstName":"Ada","lastName":"Lovelace","creatorType":"author"},
			{"name":"Example Corp","creatorType":"institution"}
		]
	}`)
	nm := Normalize(data)
	if len(nm.Creators) != 2 {
		t.Fatalf("expected 2 creators, got %d", len(nm.Creators))
	}
	if nm.Creators[0].FirstName != "Ada" || nm.Creators[0].CreatorType != "author" {
		t.Errorf("creator[0] role/name wrong: %+v", nm.Creators[0])
	}
	if nm.Creators[1].Name != "Example Corp" || nm.Creators[1].CreatorType != "institution" {
		t.Errorf("corporate/single-field creator wrong: %+v", nm.Creators[1])
	}
	if nm.PublicationYear == nil || *nm.PublicationYear != 2010 {
		t.Errorf("year = %v, want 2010", nm.PublicationYear)
	}
}

func TestNormalizeNoYearZero(t *testing.T) {
	// Missing/ambiguous date -> year nil (never 0).
	for _, date := range []string{"", "n.d.", "circa"} {
		data := json.RawMessage(`{"itemType":"book","title":"T","date":"` + date + `"}`)
		nm := Normalize(data)
		if nm.PublicationYear != nil {
			t.Errorf("date %q: year = %d, want nil", date, *nm.PublicationYear)
		}
	}
}

func TestNormalizeFullFields(t *testing.T) {
	data := json.RawMessage(`{
		"itemType":"journalArticle","title":"T","date":"2021-05",
		"DOI":"10.1/x","ISBN":"111","ISSN":"222","publisher":"P",
		"edition":"2","volume":"5","issue":"3","pages":"10-20",
		"language":"de","abstractNote":"Abs","extra":"e","url":"http://x",
		"collections":["C1"],"relations":{"dc:creator":{}}
	}`)
	nm := Normalize(data)
	if nm.DOI != "10.1/x" || nm.ISSN != "222" || nm.Edition != "2" || nm.Volume != "5" ||
		nm.Issue != "3" || nm.Pages != "10-20" || nm.Publisher != "P" ||
		nm.Language != "de" || nm.Abstract != "Abs" || nm.URL != "http://x" {
		t.Errorf("normalized fields missing: %+v", nm)
	}
	if len(nm.Collections) != 1 || nm.Collections[0] != "C1" {
		t.Errorf("collections: %v", nm.Collections)
	}
	if nm.Relations == nil {
		t.Errorf("relations must be preserved")
	}
	if nm.PublicationYear == nil || *nm.PublicationYear != 2021 {
		t.Errorf("year = %v, want 2021", nm.PublicationYear)
	}
}

func TestNormalizeUnknownFieldsRoundtripViaRaw(t *testing.T) {
	// The projection never drops unknown fields because zotero_items keeps raw_data;
	// here we at least assert Normalize does not panic and keeps recognized ones.
	data := json.RawMessage(`{"itemType":"book","title":"T","weird":"field","nested":{"a":1}}`)
	nm := Normalize(data)
	if nm.Title != "T" || nm.ItemType != "book" {
		t.Errorf("basic fields not normalized: %+v", nm)
	}
	_ = nm
}
