// publicadapt.go — the api-process public adapters (F11 #305): in the
// split topology the public routes stay byte-identical while their
// service bindings switch to the HTTP clients. These adapters implement
// the server package's per-route service interfaces (structural —
// search.Request/Response, search.Passage, the F06 import surface) on
// top of the F03 HTTP clients, owning the ADR-0001 field-name
// translation the contract DTOs froze: record_id → doc_id,
// rendition_id → attachment_id (the reverse of internal/store's
// forward mapping — the byte-identity parity test pins the pair).
package bindings

import (
	"context"
	"io"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
	axlibrary "github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

// PublicLibrary is the public import surface over the Library binding
// (the F06 libraryAPI shape: the replay signal, the decision surface,
// the multipart bound).
type PublicLibrary struct {
	Lib       library.Library // HTTPLibraryClient in split wiring
	MaxBytes  int64           // <=0 → the F06 default
	confirm   func(ctx context.Context, importID, decisionID, candidateID string) (library.ImportOperation, error)
	retry     func(ctx context.Context, importID string) (library.ImportOperation, error)
	detailed  func(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, bool, error)
	maxSource interface{ MaxImportBytes() int64 }
}

// NewPublicLibrary binds the public import surface over lib (an
// HTTPLibraryClient or any Library). maxImportBytes mirrors the
// deployment's AXIOM_LIBRARY_IMPORT_MAX_BYTES (0 = F06 default).
func NewPublicLibrary(lib library.Library, maxImportBytes int64) *PublicLibrary {
	p := &PublicLibrary{Lib: lib, MaxBytes: maxImportBytes}
	if p.MaxBytes <= 0 {
		p.MaxBytes = axlibrary.DefaultImportByteLimit
	}
	if ext, ok := lib.(extendedLibrary); ok {
		p.detailed = ext.StartImportDetailed
		p.confirm = ext.ConfirmImport
		p.retry = ext.RetryImport
	}
	return p
}

// MaxImportBytes bounds the public multipart body (content limit +
// headroom), mirroring the local wiring.
func (p *PublicLibrary) MaxImportBytes() int64 { return p.MaxBytes }

// StartImportDetailed starts (or replays) an import through the binding.
func (p *PublicLibrary) StartImportDetailed(ctx context.Context, req library.ImportRequest, content io.Reader) (library.ImportOperation, bool, error) {
	if p.detailed != nil {
		return p.detailed(ctx, req, content)
	}
	op, err := p.Lib.StartImport(ctx, req, content)
	return op, false, err
}

// GetImport polls the operation state.
func (p *PublicLibrary) GetImport(ctx context.Context, ref library.ImportRef) (library.ImportOperation, error) {
	return p.Lib.GetImport(ctx, ref)
}

// ConfirmImport answers a decision (NotFound when the binding has no
// F06 decision surface — capability-honest).
func (p *PublicLibrary) ConfirmImport(ctx context.Context, importID, decisionID, candidateID string) (library.ImportOperation, error) {
	if p.confirm == nil {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "confirm surface not available over this binding")
	}
	return p.confirm(ctx, importID, decisionID, candidateID)
}

// RetryImport re-drives a retryable-failed import.
func (p *PublicLibrary) RetryImport(ctx context.Context, importID string) (library.ImportOperation, error) {
	if p.retry == nil {
		return library.ImportOperation{}, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassNotFound, "retry surface not available over this binding")
	}
	return p.retry(ctx, importID)
}

// PublicSearch adapts the Store binding to the public search route's
// service shape (search.Request → *search.Response).
type PublicSearch struct {
	Store store.Store
}

// Search runs retrieval through the binding; contract argument errors
// surface as search.ErrBadRequest (the public 400 mapping), everything
// else as the generic error the public route logs and 503s.
func (a PublicSearch) Search(ctx context.Context, req search.Request) (*search.Response, error) {
	sreq := store.SearchRequest{Query: req.Query, TopN: req.TopN}
	if req.Filters != nil {
		sreq.Filters = &store.SearchFilters{DocumentIDs: req.Filters.DocumentIDs}
	}
	res, err := a.Store.Search(ctx, sreq)
	if err != nil {
		if class, ok := contracterr.ClassOf(err); ok && class == contracterr.ClassInvalidArgument {
			return nil, search.ErrBadRequest(err.Error())
		}
		return nil, err
	}
	return searchResponseFromDTO(res), nil
}

// PublicPassage adapts the Store binding to the public passage route's
// service shape. Known delta vs the local wiring, documented with the
// design comment: the inactive-snapshot HINT degrades to the plain 404
// (the contract carries one NotFound class; the hint body is 0.1.x
// legacy sugar the F12+ contract work can re-add as data if it matters).
type PublicPassage struct {
	Store store.Store
}

// GetPassage resolves one chunk through the binding.
func (a PublicPassage) GetPassage(ctx context.Context, chunkID string) (*search.Passage, error) {
	p, err := a.Store.GetPassage(ctx, store.PassageRef{ChunkID: chunkID})
	if err != nil {
		if class, ok := contracterr.ClassOf(err); ok && class == contracterr.ClassNotFound {
			return nil, search.ErrPassageNotFound
		}
		return nil, err
	}
	out := passageFromDTO(p)
	return out, nil
}

// --- reverse DTO mapping (store contract → search stack) ---------------

func searchResponseFromDTO(res store.SearchResult) *search.Response {
	out := &search.Response{
		Query:    res.Query,
		TopN:     res.TopN,
		Reranked: res.Reranked,
		Arms: search.Arms{
			Dense:  res.Arms.Dense,
			BM25:   res.Arms.BM25,
			Sparse: res.Arms.Sparse,
		},
		Hits:   make([]search.Hit, 0, len(res.Hits)),
		TookMS: res.TookMS,
	}
	for _, h := range res.Hits {
		hit := search.Hit{
			ChunkID:                 h.ChunkID,
			Text:                    h.Text,
			Score:                   h.Score,
			Source:                  sourceViewFromDTO(h.Source),
			Locator:                 locatorViewFromDTO(h.Locator),
			Section:                 h.Section,
			CaptionText:             h.CaptionText,
			CollapsedNearDuplicates: h.CollapsedNearDuplicates,
		}
		if len(h.Images) > 0 {
			hit.Images = make([]search.ImageView, 0, len(h.Images))
			for _, img := range h.Images {
				hit.Images = append(hit.Images, search.ImageView{
					Ref: img.Ref, Marker: img.Marker,
					MachineCaption: img.MachineCaption, FigureCaption: img.FigureCaption,
				})
			}
		}
		out.Hits = append(out.Hits, hit)
	}
	return out
}

func sourceViewFromDTO(src store.Source) repo.SourceView {
	return repo.SourceView{
		DocID:         src.RecordID, // ADR-0001 reverse: record_id → doc_id
		Title:         src.Title,
		Authors:       src.Authors,
		Year:          src.Year,
		Publisher:     src.Publisher,
		Language:      src.Language,
		Tags:          src.Tags,
		ContentType:   src.ContentType,
		CitationClass: src.CitationClass,
	}
}

func locatorViewFromDTO(l store.Locator) search.LocatorView {
	return search.LocatorView{
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

func passageFromDTO(p store.Passage) *search.Passage {
	out := &search.Passage{
		ChunkID:        p.ChunkID,
		DocumentID:     p.DocumentID,
		SnapshotID:     p.SnapshotID,
		AttachmentID:   p.RenditionID, // ADR-0001 reverse: rendition_id → attachment_id
		ChunkIndex:     p.ChunkIndex,
		Text:           p.Text,
		Section:        p.Section,
		Locator:        locatorViewFromDTO(p.Locator),
		Source:         sourceViewFromDTO(p.Source),
		ParagraphPages: p.ParagraphPages,
		CaptionText:    p.CaptionText,
		Neighbors:      make([]search.PassageNeighbor, 0, len(p.Neighbors)),
	}
	for _, n := range p.Neighbors {
		out.Neighbors = append(out.Neighbors, search.PassageNeighbor{
			ChunkID: n.ChunkID, ChunkIndex: n.ChunkIndex, Text: n.Text,
			Section: n.Section, Locator: locatorViewFromDTO(n.Locator),
		})
	}
	if len(p.Images) > 0 {
		out.Images = make([]search.ImageView, 0, len(p.Images))
		for _, img := range p.Images {
			out.Images = append(out.Images, search.ImageView{
				Ref: img.Ref, Marker: img.Marker,
				MachineCaption: img.MachineCaption, FigureCaption: img.FigureCaption,
			})
		}
	}
	return out
}
