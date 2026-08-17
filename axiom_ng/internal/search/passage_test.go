// A1 #165 passage tests: the DoD field pin (one round-trip carries EVERY
// citation field), neighbor edges, NULL-metadata regression (#158 lesson),
// inactive-snapshot 404 hint, and the SourceView type identity across the
// three surfaces.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

type livenessDocs struct {
	fakeDocs
	liveness map[string]*repo.ChunkLiveness // nil entry = unknown chunk
}

func (l *livenessDocs) ChunkLiveness(ctx context.Context, chunkID string) (*repo.ChunkLiveness, error) {
	return l.liveness[chunkID], nil
}

func seedPassageChunks(t *testing.T) *osServer {
	t.Helper()
	os := newOSServer(t)
	att := "att-A"
	for i := 0; i < 3; i++ {
		fx := chunkFixture{
			ChunkID: chunkIDFor(i), DocumentID: "doc-1", SnapshotID: "snap-1",
			AttachmentID: att, ChunkIndex: i,
			Text:     textFor(i),
			Sections: []string{"2 Grundlagen", "2.1 Stakeholder"},
			Locator:  json.RawMessage(`{"type":"page_span","page_label_start":"2` + strconv.Itoa(i) + `","physical_page_start":` + strconv.Itoa(i+28) + `}`),
		}
		os.docChunks[fx.ChunkID] = fx
	}
	// a second book: same indices, different attachment — must never leak in
	fx := chunkFixture{ChunkID: "99999999-9999-4999-8999-999999999999", DocumentID: "doc-2", SnapshotID: "snap-2",
		AttachmentID: "att-B", ChunkIndex: 1, Text: "fremdes Buch"}
	os.docChunks[fx.ChunkID] = fx
	return os
}

func chunkIDFor(i int) string {
	return "00000000-0000-4000-8000-00000000000" + strconv.Itoa(i)
}

func textFor(i int) string { return "Fachtext Chunk " + strconv.Itoa(i) }

func richMeta() fakeDocs {
	y := 2020
	return fakeDocs{meta: map[string]repo.DocumentMeta{
		"doc-1": {Title: "CSR und Stakeholdermanagement", Authors: []string{"Rene Schmidpeter"}, Year: &y,
			Publisher: "Springer Gabler", Language: "de", Tags: []string{"neutral", "peer-reviewed"}},
	}}
}

// The DoD pin: one passage round-trip carries ALL citation fields.
func TestGetPassage_FullEvidence(t *testing.T) {
	os := seedPassageChunks(t)
	svc := newService(os.URL, &fakeProcessor{}, richMeta())
	p, err := svc.GetPassage(context.Background(), chunkIDFor(1))
	if err != nil {
		t.Fatal(err)
	}
	if p.ChunkID != chunkIDFor(1) || p.DocumentID != "doc-1" || p.SnapshotID != "snap-1" || p.AttachmentID != "att-A" || p.ChunkIndex != 1 {
		t.Fatalf("chunk identity wrong: %+v", p)
	}
	if p.Text != textFor(1) || len(p.Section) != 2 || p.Locator.Kind != "page" || p.Locator.Label == "" {
		t.Fatalf("chunk content wrong: %+v", p)
	}
	src := p.Source
	if src.DocID != "doc-1" || src.Title != "CSR und Stakeholdermanagement" || len(src.Authors) != 1 ||
		src.Year == nil || *src.Year != 2020 || src.Publisher != "Springer Gabler" || src.Language != "de" ||
		len(src.Tags) != 2 {
		t.Fatalf("source block incomplete: %+v", src)
	}
	// neighbors ±1, ordered by chunk index, same attachment only
	if len(p.Neighbors) != 2 {
		t.Fatalf("want 2 neighbors, got %d: %+v", len(p.Neighbors), p.Neighbors)
	}
	if p.Neighbors[0].ChunkIndex != 0 || p.Neighbors[1].ChunkIndex != 2 {
		t.Fatalf("neighbor order wrong: %+v", p.Neighbors)
	}
	// payload pin: a neighbor is a citation surface of its own — empty text,
	// section or locator would marshal as such and stay green unnoticed.
	for _, n := range p.Neighbors {
		if n.Text == "" || len(n.Section) == 0 || n.Locator.Label == "" {
			t.Fatalf("neighbor payload incomplete: %+v", n)
		}
		if n.ChunkID == "99999999-9999-4999-8999-999999999999" {
			t.Fatalf("cross-attachment leak: %+v", n)
		}
	}
}

// Edge: first chunk of a book has exactly ONE neighbor (+1).
func TestGetPassage_FirstChunkOneNeighbor(t *testing.T) {
	os := seedPassageChunks(t)
	svc := newService(os.URL, &fakeProcessor{}, richMeta())
	p, err := svc.GetPassage(context.Background(), chunkIDFor(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Neighbors) != 1 || p.Neighbors[0].ChunkIndex != 1 {
		t.Fatalf("first chunk must have exactly the +1 neighbor: %+v", p.Neighbors)
	}
}

// Edge: LAST chunk of a book has exactly ONE neighbor (−1).
func TestGetPassage_LastChunkOneNeighbor(t *testing.T) {
	os := seedPassageChunks(t)
	svc := newService(os.URL, &fakeProcessor{}, richMeta())
	p, err := svc.GetPassage(context.Background(), chunkIDFor(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Neighbors) != 1 || p.Neighbors[0].ChunkIndex != 1 {
		t.Fatalf("last chunk must have exactly the −1 neighbor: %+v", p.Neighbors)
	}
}

// #158 regression pin: NULL/missing metadata degrades to empty fields.
func TestGetPassage_NULLMetadataDegrades(t *testing.T) {
	os := seedPassageChunks(t)
	svc := newService(os.URL, &fakeProcessor{}, fakeDocs{}) // no meta at all
	p, err := svc.GetPassage(context.Background(), chunkIDFor(1))
	if err != nil {
		t.Fatalf("NULL metadata must not error: %v", err)
	}
	if p.Source.Title != "" || p.Source.Publisher != "" || p.Source.Year != nil || p.Source.Language != "" {
		t.Fatalf("missing meta must hydrate empty: %+v", p.Source)
	}
	if p.Source.DocID != "doc-1" || p.Source.Authors == nil || p.Source.Tags == nil {
		t.Fatalf("doc id + non-nil arrays must survive: %+v", p.Source)
	}
}

// Chunk from an inactive snapshot: 404 with the structured hint.
func TestGetPassage_InactiveSnapshotHint(t *testing.T) {
	os := newOSServer(t) // empty index: chunk unknown to OS
	docs := &livenessDocs{fakeDocs: fakeDocs{}, liveness: map[string]*repo.ChunkLiveness{
		"aaaaaaaa-0000-4000-8000-000000000001": {SnapshotID: "snap-old", AttachmentID: "att-A", Active: false},
	}}
	svc := newService(os.URL, &fakeProcessor{}, docs)
	_, err := svc.GetPassage(context.Background(), "aaaaaaaa-0000-4000-8000-000000000001")
	var inactive *InactiveSnapshotError
	if err == nil || !errors.As(err, &inactive) {
		t.Fatalf("want InactiveSnapshotError, got %v", err)
	}
	if inactive.SnapshotID != "snap-old" || inactive.AttachmentID != "att-A" {
		t.Fatalf("hint payload wrong: %+v", inactive)
	}
}

// Plain unknown chunk: 404 without hint.
func TestGetPassage_UnknownChunk404(t *testing.T) {
	os := newOSServer(t)
	svc := newService(os.URL, &fakeProcessor{}, &livenessDocs{fakeDocs: fakeDocs{}})
	_, err := svc.GetPassage(context.Background(), "bbbbbbbb-0000-4000-8000-000000000002")
	if err != ErrPassageNotFound {
		t.Fatalf("want ErrPassageNotFound, got %v", err)
	}
}

// KGSources: one mget + hydration → map keyed by chunk id.
func TestKGSources(t *testing.T) {
	os := seedPassageChunks(t)
	svc := newService(os.URL, &fakeProcessor{}, richMeta())
	src, err := svc.KGSources(context.Background(), []string{chunkIDFor(0), chunkIDFor(2), "unknown-id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(src) != 2 {
		t.Fatalf("2 known chunks must hydrate: %+v", src)
	}
	if src[chunkIDFor(0)].Title != "CSR und Stakeholdermanagement" || src[chunkIDFor(0)].DocID != "doc-1" {
		t.Fatalf("source view wrong: %+v", src[chunkIDFor(0)])
	}
	if _, ok := src["unknown-id"]; ok {
		t.Fatal("unknown chunk must be absent")
	}
}

// A1 contract pin: the SourceView JSON shape is IDENTICAL on all three
// surfaces — search hit, kg sources map, passage — because it IS one type.
// Pin the wire keys so an accidental field rename breaks loudly.
func TestSourceViewWireKeys(t *testing.T) {
	y := 2020
	b, err := json.Marshal(repo.DocumentMeta{
		Title: "T", Authors: []string{"A"}, Year: &y, Publisher: "P", Language: "de", Tags: []string{"x"},
	}.View("d"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"doc_id", "title", "authors", "year", "publisher", "language", "tags"}
	if len(m) != len(want) {
		t.Fatalf("wire keys drifted: %v", m)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing wire key %q in %v", k, m)
		}
	}
	// and the three surfaces actually USE the type (compile identity).
	var _ repo.SourceView = Hit{}.Source
	var _ repo.SourceView = Passage{}.Source
}
