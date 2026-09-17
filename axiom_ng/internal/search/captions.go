// #276 caption observability: the image surface of a chunk in the client
// contract. Captions already rank (BM25 field, rerank prefix) but were
// invisible to API clients — this exposes them on /api/search hits and
// /api/passage without touching ranking or the index.
package search

import (
	"context"
	"path"
	"regexp"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// ImageView is one image of a chunk, in TEXT/MARKER order (#276). The two
// caption kinds stay separate fields — machine captions are model claims,
// figure captions are document text (#257); clients must never blend them.
type ImageView struct {
	// Ref is the durable contract ref (e.g. "image-0001") — the artifacts
	// table key.
	Ref string `json:"ref"`
	// Marker is the filename exactly as the text marker carries it (e.g.
	// "image_0.jpg"), resolved positionally (i-th marker occurrence ↔
	// i-th ref — the ingest contract reextract_figure_captions verified).
	// Empty when the occurrence count disagrees with the refs: an unsafe
	// positional guess is dropped, never guessed.
	Marker string `json:"marker,omitempty"`
	// MachineCaption (#230): model description of the image — never citable.
	MachineCaption string `json:"machine_caption,omitempty"`
	// FigureCaption (#257): caption text from the document itself — citable.
	FigureCaption string `json:"figure_caption,omitempty"`
}

// mdImageOccurrence matches a markdown image marker (`![alt](path)`) — the
// shape the chunker leaves in chunk text for every extracted image.
var mdImageOccurrence = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

// buildImages aligns a chunk's caption maps with its text markers: entries
// follow ImageRefs order (= text order), marker names resolve positionally.
// Returns nil when the chunk carries no caption at all (the field omits —
// an uncaptioned chunk exposes no images block, same omit rule as #230).
func buildImages(text string, cc repo.ChunkCaptions) []ImageView {
	if len(cc.ImageRefs) == 0 {
		return nil
	}
	occ := mdImageOccurrence.FindAllStringSubmatch(text, -1)
	paired := len(occ) == len(cc.ImageRefs)
	var out []ImageView
	for i, ref := range cc.ImageRefs {
		iv := ImageView{
			Ref:            ref,
			MachineCaption: cc.Machine[ref],
			FigureCaption:  cc.Figures[ref],
		}
		if paired {
			iv.Marker = path.Base(occ[i][1])
		}
		if iv.MachineCaption == "" && iv.FigureCaption == "" && iv.Marker == "" {
			continue // no caption AND no resolvable marker — nothing to observe
		}
		out = append(out, iv)
	}
	return out
}

// hydrateCaptions batch-loads per-chunk captions (#276). The DocSource
// capability is optional (older sources keep serving without images);
// failure degrades to no images — captions are an enhancement, never a gate.
func (s *Service) hydrateCaptions(ctx context.Context, chunkIDs []string) map[string]repo.ChunkCaptions {
	caps, ok := s.docs.(repo.ChunkCaptionSource)
	if !ok || len(chunkIDs) == 0 {
		return nil
	}
	cc, err := caps.ChunkCaptionsByIDs(ctx, chunkIDs)
	if err != nil {
		s.log.Printf("captions: hydration failed (serving without images): %v", err)
		return nil
	}
	return cc
}

// imagesFor builds the images block for one chunk (nil = omit).
func imagesFor(text string, cc map[string]repo.ChunkCaptions, chunkID string) []ImageView {
	if cc == nil {
		return nil
	}
	return buildImages(text, cc[chunkID])
}
