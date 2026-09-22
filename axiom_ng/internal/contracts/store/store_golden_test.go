// store_golden_test.go — DTO golden freeze of the Store contract
// (#297). The search/passage field names mirror the frozen v0.1.18
// public shapes (F01 witnesses) so the F11 HTTP adapter can serialize
// these DTOs without breaking the baseline. Goldens in testdata/;
// update flow BASELINE_UPDATE=1.
package store

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

var (
	at    = time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	year  = 2019
	pg    = 47
	pgEnd = 48
	para  = 12
)

var (
	goldenSource = Source{
		Bibliography: revision.Bibliography{
			RecordID: "rec-1", Title: "The Title", Authors: []string{"Ada Example"},
			Year: &year, Publisher: "Fixture Press", Language: "de",
			Tags: []string{"tag"}, CitationClass: "citable",
		},
		ContentType: revision.MediaTypePDF,
	}
	goldenLocator = Locator{
		Kind: "page", Label: "S. 47", Chapter: "Markets", ChapterNumber: newInt(3),
		PageSource: revision.TrustFolioVerified, PageStart: &pg, PageEnd: &pgEnd,
		ParagraphPages:     [][]string{{"120", "47"}, {"480", "48"}},
		ParagraphInChapter: &para, SectionTitle: "Market design",
	}
	goldenSearchRequest = SearchRequest{
		Query:   "market design",
		TopN:    5,
		Filters: &SearchFilters{DocumentIDs: []string{"rec-1"}},
	}
	goldenSearchResult = SearchResult{
		Query: "market design", TopN: 5, Reranked: false,
		Arms: SearchArms{Dense: true, BM25: true, Sparse: true},
		Hits: []SearchHit{{
			ChunkID: "chk-1", Text: "The passage text.", Score: 0.87,
			Source:                  goldenSource,
			Locator:                 goldenLocator,
			Section:                 []string{"Part I", "Markets"},
			CaptionText:             "[machine image caption: a chart]",
			Images:                  []Image{{Ref: "image-0001", Marker: "image_0.jpg", MachineCaption: "a chart", FigureCaption: "Figure 1: the chart"}},
			CollapsedNearDuplicates: 2,
		}},
		TookMS: 41,
	}
	goldenIngestRequest = IngestRevisionRequest{
		IdempotencyKey: "ingest-42",
		Revision: revision.SourceRevision{
			SourceID: "src-1", RevisionID: "3",
			ContentHash: revision.HashContent([]byte("store golden")),
			MediaType:   revision.MediaTypeEPUB,
			Bibliography: revision.Bibliography{
				RecordID: "rec-1", Title: "The Title", Authors: []string{"Ada Example"},
				Year: &year, CitationClass: "citable",
			},
			LocatorCapabilities: revision.LocatorCapabilities{EPubCFI: true},
			ContentTicket:       "ticket-1",
		},
	}
	goldenIngestJob = IngestJob{
		JobID: "job-1", Status: IngestRetryableFailed,
		RevisionID: "3", ContentHash: revision.HashContent([]byte("store golden")),
		Attempt: 2, MaxAttempts: 3,
		Failure:   &IngestFailure{Code: "RUNNER_UNAVAILABLE", Message: "compute worker unreachable"},
		UpdatedAt: at,
	}
	goldenPassage = Passage{
		ChunkID: "chk-1", DocumentID: "rec-1", SnapshotID: "snap-1", RenditionID: "ren-1",
		ChunkIndex: 4, Text: "The passage text.",
		Section: []string{"Markets"},
		Locator: goldenLocator,
		Source:  goldenSource,
		Neighbors: []PassageNeighbor{{
			ChunkID: "chk-0", ChunkIndex: 3, Text: "Previous paragraph.",
			Section: []string{"Markets"},
			Locator: Locator{Kind: "epub_cfi", Label: "Kap. 3", CFI: "epubcfi(/6/4!/4/10/2/1:0)", Chapter: "Markets"},
		}},
		ParagraphPages: goldenLocator.ParagraphPages,
		CaptionText:    "[machine image caption: a chart]",
		Images:         []Image{{Ref: "image-0002", Marker: "image_1.png", FigureCaption: "Figure 2: the table"}},
	}
)

func newInt(i int) *int { return &i }

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
		t.Fatalf("golden missing (%s) — run BASELINE_UPDATE=1 go test ./internal/contracts/store: %v", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("DTO golden %s drifted — field set/names are frozen (v1); deliberate change = update golden in the same PR:\n--- golden ---\n%s--- actual ---\n%s", name, want, got)
	}
}

func TestGoldenFreeze(t *testing.T) {
	if ContractVersion != "v1" {
		t.Fatalf("ContractVersion = %q, want v1 — a bump is a deliberate contract change", ContractVersion)
	}
	if MaxTopN != 64 {
		t.Fatalf("MaxTopN = %d, want the frozen cap 64", MaxTopN)
	}
	goldenCompare(t, "search_request.json", goldenSearchRequest)
	goldenCompare(t, "search_result.json", goldenSearchResult)
	goldenCompare(t, "ingest_revision_request.json", goldenIngestRequest)
	goldenCompare(t, "ingest_job.json", goldenIngestJob)
	goldenCompare(t, "passage.json", goldenPassage)
}

func TestRoundtripStability(t *testing.T) {
	values := []any{goldenSearchRequest, goldenSearchResult, goldenIngestRequest, goldenIngestJob, goldenPassage}
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

func newAny(v any) any {
	switch v.(type) {
	case SearchRequest:
		return &SearchRequest{}
	case SearchResult:
		return &SearchResult{}
	case IngestRevisionRequest:
		return &IngestRevisionRequest{}
	case IngestJob:
		return &IngestJob{}
	case Passage:
		return &Passage{}
	}
	panic("add the type to newAny")
}

// TestTimeFormIsUTCRFC3339Microsecond — the DM03-compatible time rule.
func TestTimeFormIsUTCRFC3339Microsecond(t *testing.T) {
	b, err := json.Marshal(goldenIngestJob)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"updated_at":"2026-01-02T03:04:05.123456Z"`) {
		t.Fatalf("updated_at drifted from UTC RFC3339 µs: %s", b)
	}
}

// TestGoldenDetectsFieldRename — in-suite mutation sonde (external
// twin: rename a tag in store.go, run the suite → red).
func TestGoldenDetectsFieldRename(t *testing.T) {
	frozen, err := json.MarshalIndent(goldenPassage, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mirror := struct {
		ChunkID    string `json:"chunk_identifier"` // RENAMED on purpose
		DocumentID string `json:"document_id"`
	}{goldenPassage.ChunkID, goldenPassage.DocumentID}
	renamed, err := json.MarshalIndent(mirror, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(renamed, frozen) {
		t.Fatal("renaming chunk_id did not change the marshaled form — the golden freeze has no teeth")
	}
}

// TestSourceContentTypeAlwaysOnWire — the frozen SourceView rule: a
// format factor must ALWAYS be present; empty string is the honest
// "unknown", absence is indistinguishable from an old server.
func TestSourceContentTypeAlwaysOnWire(t *testing.T) {
	b, err := json.Marshal(Source{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"content_type":""`) {
		t.Fatalf("content_type must always serialize (empty = unknown): %s", b)
	}
	if !strings.Contains(string(b), `"citation_class":""`) {
		t.Fatalf("citation_class must always serialize: %s", b)
	}
}
