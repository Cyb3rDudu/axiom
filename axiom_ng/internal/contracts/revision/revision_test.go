// revision_test.go — SourceRevision checks (#297): intake validation
// rules and the JSON golden freeze of the Library→Store bridge DTO.
//
// Golden update flow (same convention as internal/baseline):
// BASELINE_UPDATE=1 go test ./internal/contracts/revision — deliberate
// changes land with the updated golden in the same PR.
package revision

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAcceptsAndRejects(t *testing.T) {
	valid := SourceRevision{
		SourceID: "src-1", RevisionID: "7", RenditionID: "rend-1",
		ContentHash: HashContent([]byte("x")),
		MediaType:   MediaTypePDF, ContentTicket: "ticket-1",
		Bibliography: Bibliography{RecordID: "rec-1", CitationClass: CitationClassCitable},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid revision rejected: %v", err)
	}
	bad := []struct {
		name string
		mut  func(*SourceRevision)
	}{
		{"empty source_id", func(r *SourceRevision) { r.SourceID = "" }},
		{"empty revision_id", func(r *SourceRevision) { r.RevisionID = "" }},
		{"short hash", func(r *SourceRevision) { r.ContentHash = "abc" }},
		{"uppercase hash", func(r *SourceRevision) { r.ContentHash = strings.ToUpper(r.ContentHash) }},
		{"empty media_type", func(r *SourceRevision) { r.MediaType = "" }},
		{"empty ticket", func(r *SourceRevision) { r.ContentTicket = "" }},
		{"empty bibliography.record_id", func(r *SourceRevision) { r.Bibliography.RecordID = "" }},
		{"unknown citation_class", func(r *SourceRevision) { r.Bibliography.CitationClass = "maybe" }},
	}
	for _, c := range bad {
		r := valid
		c.mut(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: invalid revision accepted", c.name)
		}
	}
	// the closed vocabulary accepts both members explicitly
	contextual := valid
	contextual.Bibliography.CitationClass = CitationClassContextual
	if err := contextual.Validate(); err != nil {
		t.Fatalf("contextual citation class rejected: %v", err)
	}
}

var (
	year           = 1984
	goldenRevision = SourceRevision{
		SourceID:    "src-seed-1",
		RevisionID:  "12",
		RenditionID: "rend-seed-1",
		ContentHash: HashContent([]byte("golden bytes")),
		MediaType:   MediaTypeEPUB,
		Bibliography: Bibliography{
			RecordID:      "rec-seed-1",
			Title:         "The Golden Fixture",
			Authors:       []string{"Ada Example", "Bob Sample"},
			Year:          &year,
			Publisher:     "Fixture Press",
			Language:      "de",
			Tags:          []string{"contract", "golden"},
			CitationClass: "citable",
		},
		LocatorCapabilities: LocatorCapabilities{
			Page:    &PageCapability{Trust: TrustFolioInterpolated},
			EPubCFI: true,
		},
		ContentTicket: "ticket-seed-1",
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
		t.Fatalf("golden missing (%s) — run BASELINE_UPDATE=1 go test ./internal/contracts/revision: %v", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("DTO golden %s drifted — field set/names are frozen (v1); deliberate change = update golden in the same PR:\n--- golden ---\n%s--- actual ---\n%s", name, want, got)
	}
}

func TestSourceRevisionGoldenFrozen(t *testing.T) {
	if ContractVersion != "v1" {
		t.Fatalf("ContractVersion = %q, want v1 — a bump is a deliberate contract change", ContractVersion)
	}
	goldenCompare(t, "source_revision.json", goldenRevision)
}

func TestSourceRevisionRoundtripStable(t *testing.T) {
	b1, err := json.Marshal(goldenRevision)
	if err != nil {
		t.Fatal(err)
	}
	var back SourceRevision
	if err := json.Unmarshal(b1, &back); err != nil {
		t.Fatal(err)
	}
	b2, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("roundtrip unstable:\n%s\n%s", b1, b2)
	}
	if back.Bibliography.Year == nil || *back.Bibliography.Year != year {
		t.Fatal("optional Year lost through roundtrip (nil-vs-value must be distinguishable)")
	}
	if back.LocatorCapabilities.Page == nil || back.LocatorCapabilities.Page.Trust != TrustFolioInterpolated {
		t.Fatal("optional page capability lost through roundtrip")
	}
}

// TestGoldenDetectsFieldRename — in-suite mutation sonde: renaming a
// field must change the marshaled bytes, i.e. the frozen compare above
// could not still pass. The external probe (rename the tag in source,
// run the suite) exercises the same path.
func TestGoldenDetectsFieldRename(t *testing.T) {
	// local mirror with one json tag renamed on purpose
	mirror := struct {
		SourceID            string              `json:"source_id"`
		RevisionID          string              `json:"revision_identifier"` // RENAMED on purpose
		ContentHash         string              `json:"content_hash"`
		MediaType           string              `json:"media_type"`
		Bibliography        Bibliography        `json:"bibliography"`
		LocatorCapabilities LocatorCapabilities `json:"locator_capabilities"`
		ContentTicket       string              `json:"content_ticket"`
	}{
		SourceID: goldenRevision.SourceID, RevisionID: goldenRevision.RevisionID,
		ContentHash: goldenRevision.ContentHash, MediaType: goldenRevision.MediaType,
		Bibliography: goldenRevision.Bibliography, LocatorCapabilities: goldenRevision.LocatorCapabilities,
		ContentTicket: goldenRevision.ContentTicket,
	}
	renamedBytes, err := json.MarshalIndent(mirror, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	frozenBytes, err := json.MarshalIndent(goldenRevision, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(renamedBytes) == string(frozenBytes) {
		t.Fatal("renaming revision_id did not change the marshaled form — the golden freeze has no teeth")
	}
}

// TestHashContentIsSha256Hex pins the digest form both sides of the
// seam rely on.
func TestHashContentIsSha256Hex(t *testing.T) {
	got := HashContent([]byte("golden bytes"))
	if len(got) != 64 || strings.ToLower(got) != got {
		t.Fatalf("HashContent = %q, want 64 lowercase hex chars", got)
	}
}
