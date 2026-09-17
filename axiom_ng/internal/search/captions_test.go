// #276 caption observability: contract pins for caption_text + images[] on
// /api/search hits and /api/passage. The DoD probes: field present on a
// captioned fixture, absent on one without; multi-image alignment with the
// text markers (two markers, two captions, order preserved); degradation
// (no capability / hydration failure) never gates the response.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// fakeCaptionDocs is a DocSource WITH the #276 capability.
type fakeCaptionDocs struct {
	fakeDocs
	caps map[string]repo.ChunkCaptions
	err  error
}

func (f fakeCaptionDocs) ChunkCaptionsByIDs(ctx context.Context, ids []string) (map[string]repo.ChunkCaptions, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.caps, nil
}

// captionedFixture is the lecture-EPUB shape: two image markers in the
// text (document order), contract refs on the chunk, machine captions for
// both images, a document figure caption for the second.
const captionedText = "Einführung digitital ethics. ![Folie 3: •](image_0.jpg) Weiter im Text. ![Folie 4: Chart](image_1.jpg) Ende."

func captionedCaps() map[string]repo.ChunkCaptions {
	return map[string]repo.ChunkCaptions{
		"c-cap": {
			ImageRefs: []string{"image-0000", "image-0001"},
			Machine:   map[string]string{"image-0000": "Slide listing digital ethics frameworks", "image-0001": "Bar chart of survey results"},
			Figures:   map[string]string{"image-0001": "Abb. 2: Umfrageergebnisse"},
		},
	}
}

// TestSearch_CaptionsOnHits pins the DoD contract: the captioned hit
// carries caption_text AND an images[] block aligned with its markers
// (two markers, two entries, order preserved, marker filenames resolved);
// the uncaptioned sibling hit carries neither on the wire.
func TestSearch_CaptionsOnHits(t *testing.T) {
	srv := newOSServer(t)
	srv.bm25Hits = []osHit{
		hitCaption("c-cap", "d1", captionedText, "[machine image caption: Slide listing digital ethics frameworks] [machine image caption: Bar chart of survey results]"),
		hit("p1", "d1", "plain prose chunk without images"),
	}
	svc := newService(srv.URL, &fakeProcessor{embedVec: []float32{0.1}},
		fakeCaptionDocs{fakeDocs: fakeDocs{meta: map[string]repo.DocumentMeta{}}, caps: captionedCaps()})
	svc.Rerank = false
	res, err := svc.Search(context.Background(), Request{Query: "ethics", TopN: 2})
	if err != nil {
		t.Fatal(err)
	}
	var capHit, plainHit *Hit
	for i := range res.Hits {
		switch res.Hits[i].ChunkID {
		case "c-cap":
			capHit = &res.Hits[i]
		case "p1":
			plainHit = &res.Hits[i]
		}
	}
	if capHit == nil || plainHit == nil {
		t.Fatalf("both fixture hits must be served, got %+v", res.Hits)
	}

	// caption_text: the labeled string verbatim (the field the BM25 arm
	// ranks on — a client can now see WHY the chunk matched).
	if !strings.Contains(capHit.CaptionText, "[machine image caption: Slide listing digital ethics frameworks]") {
		t.Fatalf("caption_text missing on hit: %q", capHit.CaptionText)
	}

	// images[]: two markers, two entries, order preserved.
	if len(capHit.Images) != 2 {
		t.Fatalf("multi-image chunk must expose 2 images, got %d: %+v", len(capHit.Images), capHit.Images)
	}
	a, b := capHit.Images[0], capHit.Images[1]
	if a.Ref != "image-0000" || a.Marker != "image_0.jpg" || a.MachineCaption != "Slide listing digital ethics frameworks" {
		t.Fatalf("first image misaligned: %+v", a)
	}
	if b.Ref != "image-0001" || b.Marker != "image_1.jpg" ||
		b.MachineCaption != "Bar chart of survey results" || b.FigureCaption != "Abb. 2: Umfrageergebnisse" {
		t.Fatalf("second image misaligned: %+v", b)
	}

	// Wire form: captioned hit carries both fields, uncaptioned carries
	// neither (omitempty pins "absent", not null).
	for _, h := range []Hit{*capHit, *plainHit} {
		raw, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		_, hasCap := keys["caption_text"]
		_, hasImgs := keys["images"]
		if h.ChunkID == "c-cap" {
			if !hasCap || !hasImgs {
				t.Fatalf("captioned hit must expose caption_text + images on the wire: %s", raw)
			}
			// pin the entry keys structurally (substring checks are naive —
			// the fixture text itself contains "images")
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(keys["images"], &entries); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"ref", "marker", "machine_caption"} {
				if _, ok := entries[0][want]; !ok {
					t.Fatalf("image entry missing key %q: %s", want, raw)
				}
			}
			if _, ok := entries[1]["figure_caption"]; !ok {
				t.Fatalf("second entry must carry figure_caption: %s", raw)
			}
		} else if hasCap || hasImgs {
			t.Fatalf("uncaptioned hit must omit both fields: %s", raw)
		}
	}
}

// TestSearch_CaptionHydrationFailureDegrades: a DB error behind the
// hydration must never fail or empty the search — hits serve without
// images. (Mutation probe: make hydration an error path that returns no
// hits -> red.)
func TestSearch_CaptionHydrationFailureDegrades(t *testing.T) {
	srv := newOSServer(t)
	srv.bm25Hits = []osHit{hit("c1", "d1", "prose")}
	svc := newService(srv.URL, &fakeProcessor{embedVec: []float32{0.1}},
		fakeCaptionDocs{fakeDocs: fakeDocs{meta: map[string]repo.DocumentMeta{}}, err: errors.New("db down")})
	svc.Rerank = false
	res, err := svc.Search(context.Background(), Request{Query: "x", TopN: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ChunkID != "c1" {
		t.Fatalf("hits must survive hydration failure: %+v", res.Hits)
	}
	if res.Hits[0].Images != nil {
		t.Fatalf("failed hydration must serve no images, got %+v", res.Hits[0].Images)
	}
}

// TestSearch_CaptionsWithoutCapability: a DocSource without the optional
// interface (older deployment shape) keeps serving — no images, no error.
func TestSearch_CaptionsWithoutCapability(t *testing.T) {
	srv := newOSServer(t)
	srv.bm25Hits = []osHit{hit("c1", "d1", "prose")}
	svc := newService(srv.URL, &fakeProcessor{embedVec: []float32{0.1}}, fakeDocs{meta: map[string]repo.DocumentMeta{}})
	svc.Rerank = false
	res, err := svc.Search(context.Background(), Request{Query: "x", TopN: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hits[0].Images != nil {
		t.Fatalf("non-capability source must serve no images, got %+v", res.Hits[0].Images)
	}
}

// TestBuildImages_MarkerCountMismatch: when the text's marker occurrences
// disagree with the refs (Z2 ligature-misread class), the positional
// marker guess is dropped — captions still serve, aligned by ref order.
func TestBuildImages_MarkerCountMismatch(t *testing.T) {
	cc := repo.ChunkCaptions{
		ImageRefs: []string{"image-0000", "image-0001"},
		Machine:   map[string]string{"image-0000": "A", "image-0001": "B"},
	}
	got := buildImages("text ![x](image_0.jpg) mid ![y](image_1.jpg) tail ![z](stray.jpg)", cc)
	if len(got) != 2 || got[0].Marker != "" || got[1].Marker != "" {
		t.Fatalf("mismatched counts must drop marker names, keep captions in ref order: %+v", got)
	}
	if got[0].MachineCaption != "A" || got[1].MachineCaption != "B" {
		t.Fatalf("captions must stay ref-aligned: %+v", got)
	}
}

// TestGetPassage_CaptionsExposed pins the passage side of the #276
// contract: caption_text from the index doc + images[] hydrated for the
// center chunk; a captionless passage omits both.
func TestGetPassage_CaptionsExposed(t *testing.T) {
	srv := seedPassageChunks(t)
	// Make chunk 1 the captioned lecture chunk.
	fx := srv.docChunks[chunkIDFor(1)]
	fx.Text = captionedText
	fx.CaptionText = "[machine image caption: Slide listing digital ethics frameworks]"
	srv.docChunks[chunkIDFor(1)] = fx

	svc := newService(srv.URL, nil,
		fakeCaptionDocs{fakeDocs: richMeta(), caps: map[string]repo.ChunkCaptions{
			chunkIDFor(1): {
				ImageRefs: []string{"image-0000", "image-0001"},
				Machine:   map[string]string{"image-0000": "Slide listing digital ethics frameworks", "image-0001": "Bar chart of survey results"},
			},
		}})

	p, err := svc.GetPassage(context.Background(), chunkIDFor(1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.CaptionText, "Slide listing digital ethics frameworks") {
		t.Fatalf("passage must expose caption_text, got %q", p.CaptionText)
	}
	if len(p.Images) != 2 || p.Images[0].Marker != "image_0.jpg" || p.Images[1].Marker != "image_1.jpg" {
		t.Fatalf("passage images must align with markers: %+v", p.Images)
	}
	// neighbors stay caption-free (context only, #276 scope is the chunk).
	for _, n := range p.Neighbors {
		if n.ChunkID == chunkIDFor(1) {
			t.Fatal("center chunk leaked into neighbors")
		}
	}

	// captionless passage: both fields omitted.
	p0, err := svc.GetPassage(context.Background(), chunkIDFor(0))
	if err != nil {
		t.Fatal(err)
	}
	if p0.CaptionText != "" || p0.Images != nil {
		t.Fatalf("captionless passage must omit both, got %q %+v", p0.CaptionText, p0.Images)
	}
}
