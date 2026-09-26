// dto.go — the search-stack ↔ contract-DTO mapping. Field-for-field
// copies between shape-identical types (the contract DTOs are the frozen
// public API field names; search.Response/Passage serialize the same
// shapes) — explicit code over reflection so a drift fails the compile,
// not production.
package store

import (
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

func searchResponseDTO(res *search.Response) store.SearchResult {
	if res == nil {
		return store.SearchResult{}
	}
	out := store.SearchResult{
		Query:    res.Query,
		TopN:     res.TopN,
		Reranked: res.Reranked,
		Arms: store.SearchArms{
			Dense:  res.Arms.Dense,
			BM25:   res.Arms.BM25,
			Sparse: res.Arms.Sparse,
		},
		Hits:   make([]store.SearchHit, 0, len(res.Hits)),
		TookMS: res.TookMS,
	}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, searchHitDTO(h))
	}
	return out
}

func searchHitDTO(h search.Hit) store.SearchHit {
	hit := store.SearchHit{
		ChunkID:                 h.ChunkID,
		Text:                    h.Text,
		Score:                   h.Score,
		Source:                  sourceDTO(h.Source),
		Locator:                 locatorDTO(h.Locator),
		Section:                 h.Section,
		CaptionText:             h.CaptionText,
		CollapsedNearDuplicates: h.CollapsedNearDuplicates,
	}
	for _, img := range h.Images {
		hit.Images = append(hit.Images, store.Image{
			Ref: img.Ref, Marker: img.Marker,
			MachineCaption: img.MachineCaption, FigureCaption: img.FigureCaption,
		})
	}
	return hit
}

func sourceDTO(sv repo.SourceView) store.Source {
	return store.Source{
		Bibliography: revision.Bibliography{
			RecordID:      sv.DocID,
			Title:         sv.Title,
			Authors:       sv.Authors,
			Year:          sv.Year,
			Publisher:     sv.Publisher,
			Language:      sv.Language,
			Tags:          sv.Tags,
			CitationClass: sv.CitationClass,
		},
		ContentType: sv.ContentType,
	}
}

func locatorDTO(l search.LocatorView) store.Locator {
	return store.Locator{
		Kind:               l.Kind,
		Label:              l.Label,
		Chapter:            l.Chapter,
		CFI:                l.CFI,
		ChapterNumber:      l.ChapterNumber,
		PageSource:         l.PageSource,
		PageStart:          l.PageStart,
		PageEnd:            l.PageEnd,
		ParagraphPages:     l.ParagraphPages,
		ParagraphInChapter: l.ParagraphInChapter,
		SectionTitle:       l.SectionTitle,
	}
}

func passageDTO(p *search.Passage) store.Passage {
	if p == nil {
		return store.Passage{}
	}
	out := store.Passage{
		ChunkID:        p.ChunkID,
		DocumentID:     p.DocumentID,
		SnapshotID:     p.SnapshotID,
		RenditionID:    p.AttachmentID, // ADR-0001: attachment_id → rendition_id
		ChunkIndex:     p.ChunkIndex,
		Text:           p.Text,
		Section:        p.Section,
		Locator:        locatorDTO(p.Locator),
		Source:         sourceDTO(p.Source),
		ParagraphPages: p.ParagraphPages,
		CaptionText:    p.CaptionText,
	}
	for _, n := range p.Neighbors {
		out.Neighbors = append(out.Neighbors, store.PassageNeighbor{
			ChunkID: n.ChunkID, ChunkIndex: n.ChunkIndex, Text: n.Text,
			Section: n.Section, Locator: locatorDTO(n.Locator),
		})
	}
	for _, img := range p.Images {
		out.Images = append(out.Images, store.Image{
			Ref: img.Ref, Marker: img.Marker,
			MachineCaption: img.MachineCaption, FigureCaption: img.FigureCaption,
		})
	}
	return out
}
