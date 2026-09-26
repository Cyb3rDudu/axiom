// library_golden_test.go — DTO golden freeze of the Library contract
// (#297). Goldens in testdata/; update flow BASELINE_UPDATE=1 (same
// convention as internal/baseline). Roundtrip stability proves
// optionalität survives the wire; the rename sonde proves the freeze.
package library

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
)

// µs — the DM03-compatible time form (UTC RFC3339 microsecond).
var (
	at         = time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	year       = 2019
	yes        = true
	no         = false
	acc        = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	capturedAt = time.Date(2026, 1, 1, 8, 30, 0, 0, time.UTC)
	pg         = 47
	para       = 12
	chapNo     = 3
)

var (
	goldenSource = Source{
		SourceID: "src-seed-1", Provider: "zotero", LibraryID: "users/0", SyncedAt: &at,
	}
	goldenImportRequest = ImportRequest{
		IdempotencyKey: "import-42",
		RecordType:     "book",
		Target:         ImportTarget{LibraryID: "users/0", CollectionPath: []string{"Semester 3", "VWL"}, CreateMissing: false},
		Source:         &ImportSourceInfo{OriginalURL: "https://example.org/paper", AccessedAt: &acc, CapturedAt: &capturedAt},
		MetadataHints:  MetadataHints{DOI: "10.1000/xyz", ISBN: "978-3-16-148410-0", Title: "The Title"},
		Enrichment:     EnrichmentFlags{Crossref: &yes, OpenLibrary: &no},
	}
	// minimal form: unfiled target addressed by CollectionID — the XOR
	// twin of the CollectionPath fixture, so both stay witnessed.
	goldenImportRequestCollectionID = ImportRequest{
		IdempotencyKey: "import-43",
		RecordType:     "book",
		Target:         ImportTarget{LibraryID: "users/0", CollectionID: "COLL_8XQ2"},
	}
	goldenImportOperation = ImportOperation{
		ImportID: "imp-1",
		Status:   ImportCommitted,
		Result: &ImportResult{
			RecordID:    "rec-1",
			RenditionID: "ren-1",
			Revision: revision.SourceRevision{
				SourceID: "src-1", RevisionID: "1", RenditionID: "rend-1",
				ContentHash: revision.HashContent([]byte("library golden")),
				MediaType:   revision.MediaTypePDF,
				Bibliography: revision.Bibliography{
					RecordID: "rec-1", Title: "The Title", Authors: []string{"Ada Example"},
					Year: &year, Publisher: "Fixture Press", CitationClass: "citable",
				},
				LocatorCapabilities: revision.LocatorCapabilities{Page: &revision.PageCapability{Trust: revision.TrustFolioVerified}},
				ContentTicket:       "ticket-1",
			},
		},
		UpdatedAt: at,
	}
	// awaiting_confirmation shape — witnesses Decisions/Candidates.
	goldenImportAwaiting = ImportOperation{
		ImportID: "imp-2",
		Status:   ImportAwaitingConfirm,
		Decisions: []ImportDecision{{
			DecisionID: "dec-1",
			Subject:    "bibliography",
			Candidates: []ImportCandidate{
				{CandidateID: "cand-1", Origin: "document", Summary: "Title from the document itself"},
				{CandidateID: "cand-2", Origin: "crossref", Summary: "DOI hit: The Title (Fixture Press, 2019)"},
				{CandidateID: "cand-3", Origin: "open_library", Summary: "ISBN hit: The Title, 2nd ed."},
			},
		}},
		UpdatedAt: at,
	}
	// committed shape WITH the F06 per-field provenance trail — witnesses
	// the additive Fields form (v1: absent on provenance-free operations).
	goldenImportProvenance = ImportOperation{
		ImportID: "imp-4",
		Status:   ImportCommitted,
		Result: &ImportResult{
			RecordID: "rec-4", RenditionID: "ren-4",
			Revision: revision.SourceRevision{
				SourceID: "src-1", RevisionID: "1", RenditionID: "rend-1",
				ContentHash: revision.HashContent([]byte("library golden provenance")),
				MediaType:   revision.MediaTypePDF,
				Bibliography: revision.Bibliography{
					RecordID: "rec-4", Title: "The Title", CitationClass: "citable",
				},
				LocatorCapabilities: revision.LocatorCapabilities{Page: &revision.PageCapability{Trust: revision.TrustFolioVerified}},
				ContentTicket:       "ticket-4",
			},
		},
		Fields: []FieldProvenance{
			{Field: "title", Source: "document", Confidence: 1.0, Applied: true, Value: "The Title", At: &at},
			{Field: "title", Source: "crossref", ResolverVersion: "fake-v1", Confidence: 0.5, Applied: false, Value: "A Weaker Title", At: &at},
			{Field: "publisher", Source: "identifier", ResolverVersion: "fake-v1", Confidence: 0.9, Applied: true, Value: "Fixture Press", At: &at},
		},
		UpdatedAt: at,
	}
	// terminal-failure shape — witnesses ImportFailure.
	goldenImportFailed = ImportOperation{
		ImportID:  "imp-3",
		Status:    ImportTerminalFailed,
		Failure:   &ImportFailure{Code: "SOURCE_NOT_FOUND", Message: "attachment path outside the allowed roots"},
		UpdatedAt: at,
	}
	goldenCitationRequest = CitationRequest{
		RecordID: "rec-1",
		Locator: CitationLocator{
			Kind: "page", PageStart: &pg, PageEnd: &pg, Chapter: "Markets",
			ChapterNumber: &chapNo, ParagraphInChapter: &para, SectionTitle: "Market design",
			PageSource: revision.TrustFolioVerified,
		},
	}
	// epub_cfi locator — the field-set twin of the page fixture above:
	// CFI is only applicable to this kind, page fields only to that one;
	// two fixtures keep BOTH field sets witnessed.
	goldenCitationLocatorCFI = CitationLocator{
		Kind: "epub_cfi", CFI: "epubcfi(/6/4[chap03ref]!/4[body01]/10/2/1:0)",
		Chapter: "Markets", ChapterNumber: &chapNo, SectionTitle: "Market design",
		ParagraphInChapter: &para,
	}
	goldenCitationProjection = CitationProjection{
		RecordID:  "rec-1",
		Citation:  "(Example, 2019, S. 47)",
		Reference: "Example, A. (2019). The Title. Fixture Press.",
		Style:     "apa-7",
		Locator:   goldenCitationRequest.Locator,
	}
)

func goldenCompare(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if os.Getenv("BASELINE_UPDATE") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden missing (%s) — run BASELINE_UPDATE=1 go test ./internal/contracts/library: %v", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("DTO golden %s drifted — field set/names are frozen (v1); deliberate change = update golden in the same PR:\n--- golden ---\n%s--- actual ---\n%s", name, want, got)
	}
}

func TestGoldenFreeze(t *testing.T) {
	if ContractVersion != "v1" {
		t.Fatalf("ContractVersion = %q, want v1 — a bump is a deliberate contract change", ContractVersion)
	}
	goldenCompare(t, "source.json", goldenSource)
	goldenCompare(t, "import_request.json", goldenImportRequest)
	goldenCompare(t, "import_request_collection_id.json", goldenImportRequestCollectionID)
	goldenCompare(t, "import_operation.json", goldenImportOperation)
	goldenCompare(t, "import_operation_awaiting.json", goldenImportAwaiting)
	goldenCompare(t, "import_operation_failed.json", goldenImportFailed)
	goldenCompare(t, "import_operation_provenance.json", goldenImportProvenance)
	goldenCompare(t, "citation_request.json", goldenCitationRequest)
	goldenCompare(t, "citation_locator_epub_cfi.json", goldenCitationLocatorCFI)
	goldenCompare(t, "citation_projection.json", goldenCitationProjection)
}

func TestRoundtripStability(t *testing.T) {
	values := []any{goldenSource, goldenImportRequest, goldenImportRequestCollectionID, goldenImportOperation, goldenImportAwaiting, goldenImportFailed, goldenImportProvenance, goldenCitationRequest, goldenCitationLocatorCFI, goldenCitationProjection}
	for _, v := range values {
		b1, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		back := newAny(v)
		if err := json.Unmarshal(b1, back); err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		b2, err := json.Marshal(back)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("%T: roundtrip unstable:\n%s\n%s", v, b1, b2)
		}
	}
}

// newAny allocates a zero value of v's type for roundtrip decoding.
func newAny(v any) any {
	switch v.(type) {
	case Source:
		return &Source{}
	case ImportRequest:
		return &ImportRequest{}
	case ImportOperation:
		return &ImportOperation{}
	case CitationLocator:
		return &CitationLocator{}
	case CitationRequest:
		return &CitationRequest{}
	case CitationProjection:
		return &CitationProjection{}
	}
	panic("add the type to newAny")
}

// TestTimeFormIsUTCRFC3339Microsecond — the DM03-compatible time rule:
// every time field marshals as UTC RFC3339 with exactly microseconds.
func TestTimeFormIsUTCRFC3339Microsecond(t *testing.T) {
	b, err := json.Marshal(goldenImportOperation)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"updated_at":"2026-01-02T03:04:05.123456Z"`) {
		t.Fatalf("updated_at drifted from UTC RFC3339 µs: %s", b)
	}
	var op ImportOperation
	if err := json.Unmarshal(b, &op); err != nil {
		t.Fatal(err)
	}
	if op.UpdatedAt.Location() != time.UTC || op.UpdatedAt.Nanosecond()%1000 != 0 {
		t.Fatalf("time did not roundtrip as UTC µs: %v", op.UpdatedAt)
	}
}

// TestGoldenDetectsFieldRename — in-suite mutation sonde (external
// twin: rename the tag in library.go, run the suite → red). The mirror
// carries the FULL field set of ImportOperation: only then does a byte
// difference prove the RENAME moved the bytes — a subset mirror differs
// trivially and proves nothing. The control (identical tags) must equal
// the frozen bytes, proving the mirror is faithful.
func TestGoldenDetectsFieldRename(t *testing.T) {
	frozen, err := json.MarshalIndent(goldenImportOperation, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	control := struct {
		ImportID  string           `json:"import_id"`
		Status    ImportStatus     `json:"status"`
		Decisions []ImportDecision `json:"decisions,omitempty"`
		Result    *ImportResult    `json:"result,omitempty"`
		Failure   *ImportFailure   `json:"failure,omitempty"`
		UpdatedAt time.Time        `json:"updated_at"`
	}{goldenImportOperation.ImportID, goldenImportOperation.Status, goldenImportOperation.Decisions, goldenImportOperation.Result, goldenImportOperation.Failure, goldenImportOperation.UpdatedAt}
	controlBytes, err := json.MarshalIndent(control, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(controlBytes, frozen) {
		t.Fatalf("full-field mirror is not faithful — fix the mirror before trusting the sonde:\n%s\n%s", controlBytes, frozen)
	}
	renamed := struct {
		ImportID  string           `json:"import_identifier"` // RENAMED on purpose
		Status    ImportStatus     `json:"status"`
		Decisions []ImportDecision `json:"decisions,omitempty"`
		Result    *ImportResult    `json:"result,omitempty"`
		Failure   *ImportFailure   `json:"failure,omitempty"`
		UpdatedAt time.Time        `json:"updated_at"`
	}{goldenImportOperation.ImportID, goldenImportOperation.Status, goldenImportOperation.Decisions, goldenImportOperation.Result, goldenImportOperation.Failure, goldenImportOperation.UpdatedAt}
	renamedBytes, err := json.MarshalIndent(renamed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(renamedBytes, frozen) {
		t.Fatal("renaming import_id did not change the marshaled form — the golden freeze has no teeth")
	}
}

// TestOptionalitaetDistinguishesAbsence — the documented pointer
// policies must be observable on the wire: nil enrichment flag = absent
// (consumer default true, documented on the field); explicit true/false
// marshaled; nil SyncedAt absent, not null.
func TestOptionalitaetDistinguishesAbsence(t *testing.T) {
	b, err := json.Marshal(ImportRequest{IdempotencyKey: "k", RecordType: "book", Target: ImportTarget{}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, absent := range []string{`"crossref"`, `"open_library"`, `"source"`, `"collection_id"`} {
		if strings.Contains(s, absent) {
			t.Fatalf("nil optionals must be absent from the wire (default-true is documented, not serialized): %s", s)
		}
	}
	explicitNo, _ := json.Marshal(ImportRequest{Enrichment: EnrichmentFlags{Crossref: &no}})
	if !strings.Contains(string(explicitNo), `"crossref":false`) {
		t.Fatalf("explicit false not distinguishable from explicit true: %s", explicitNo)
	}
	explicitYes, _ := json.Marshal(ImportRequest{Enrichment: EnrichmentFlags{Crossref: &yes}})
	if !strings.Contains(string(explicitYes), `"crossref":true`) {
		t.Fatalf("explicit true missing: %s", explicitYes)
	}
	src, _ := json.Marshal(goldenSource)
	if !strings.Contains(string(src), `"synced_at":"`) {
		t.Fatalf("set SyncedAt missing from wire: %s", src)
	}
	absentSync, _ := json.Marshal(Source{SourceID: "s"})
	if strings.Contains(string(absentSync), "synced_at") {
		t.Fatalf("nil SyncedAt must be absent, not null: %s", absentSync)
	}
}
