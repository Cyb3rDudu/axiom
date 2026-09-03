package processor

// Result types for GET /v1/jobs/{id}/result (PROCESSOR_CONTRACT.md §10).
//
// Gate 2 only modelled the request/acceptance envelopes; the full result payload
// is decoded and validated by Gate 4 (the result is untrusted input — contract
// §14). Unknown additive fields are tolerated by Go's json (contract §4); the
// required fields below are the ones §14 validates and §4.4 persists.
//
// All refs are job-local (the processor's own ids); axiom-ng maps them to
// durable ids while persisting.

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Result is the top-level processor result payload.
type Result struct {
	ContractVersion     string               `json:"contract_version"`
	JobID               string               `json:"job_id"`
	Status              string               `json:"status"`
	Source              ResultSource         `json:"source"`
	Processor           ResultProcessor      `json:"processor"`
	Artifacts           []Artifact           `json:"artifacts"`
	Manifest            map[string]any       `json:"manifest"`
	Chunks              []Chunk              `json:"chunks"`
	Entities            []Entity             `json:"entities"`
	ChunkRelationships  []ChunkRelationship  `json:"chunk_relationships"`
	EntityRelationships []EntityRelationship `json:"entity_relationships"`
	Stats               Stats                `json:"stats"`
	Warnings            []map[string]any     `json:"warnings"`
}

// ResultSource echoes the verified source identity (§10/§14).
type ResultSource struct {
	AttachmentID string `json:"attachment_id"`
	ContentHash  string `json:"content_hash"`
	Verified     bool   `json:"verified"`
}

// ResultProcessor is the processor/profile/model provenance block.
type ResultProcessor struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Profile     string            `json:"profile"`
	ProfileHash string            `json:"profile_hash"`
	Models      map[string]string `json:"models"`
}

// Artifact is a durable derived artifact with a VERIFIED digest (§13).
type Artifact struct {
	Ref       string `json:"ref"`
	Kind      string `json:"kind"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Retention string `json:"retention"`
	// Attributes (#230): additive artifact metadata — machine_caption /
	// caption_model / caption_path ride here for extracted_image artifacts.
	// MUST stay in this struct: the persist boundary re-marshals from the
	// typed struct and silently drops unknown fields (W9 lesson).
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Chunk is an ordered text span with provenance (§11).
type Chunk struct {
	Ref        string         `json:"ref"`
	Index      int            `json:"index"`
	Text       string         `json:"text"`
	Locator    *Locator       `json:"locator"`
	Structure  ChunkStructure `json:"structure"`
	TokenCount int            `json:"token_count"`
	ImageRefs  []string       `json:"image_refs"`
	// ImageCaptions (#230): machine captions of the referenced images
	// (artifact ref → caption). ADDITIONAL indexable text — never part of
	// Text (a caption is a machine claim, not citation prose). MUST stay
	// in this struct: the persist boundary re-marshals from the typed
	// struct and silently drops unknown fields (W9 lesson).
	ImageCaptions map[string]string `json:"image_captions,omitempty"`
	Embeddings    ChunkEmbeddings   `json:"embeddings"`
	Metadata      map[string]any    `json:"metadata"`
}

// #173 page_source trust levels — stamped by the runner's page_trust
// pipeline (never guessed); validation and rendering compare against these.
const (
	PageSourceFolioVerified = "folio_verified"
	PageSourcePDFLabelSane  = "pdf_label_sane"
	PageSourcePhysicalOnly  = "physical_only"
	// PageSourceBlind (v2.1): the page has NO text layer at all — a scan
	// needing an OCR rebuild. Not a print-page claim of any kind; the
	// runner classifies, it never executes OCR.
	PageSourceBlind = "blind"
	PageSourceNone  = "none"
	// #223/#226: EPUB print-page anchors. print_verified = proven book-
	// internally (printed TOC matches chapter-start markers). derived_from_
	// sibling = page map derived from the PDF sibling and INJECTED by the
	// #222 tool (declared via the OPF axiom-page-source meta — the anchor
	// shape mimics native format, provenance is explicit). print_unverified
	// = monotone markers without proof — never silently upgraded; divergent
	// maps are refused entirely.
	PageSourcePrintVerified      = "print_verified"
	PageSourceDerivedFromSibling = "derived_from_sibling"
	PageSourcePrintUnverified    = "print_unverified"
)

// Locator is the source position (§11). page_span for PDFs (physical+logical),
// epub_cfi for pageless EPUBs. PageSource (#173) is the trust level of the
// page reference — folio_verified | pdf_label_sane | physical_only | none:
// only folio_verified may be cited as a printed page; the contract is
// "never guess — every page reference carries its level".
type Locator struct {
	Type              string `json:"type"`
	PhysicalPageStart *int   `json:"physical_page_start,omitempty"`
	PhysicalPageEnd   *int   `json:"physical_page_end,omitempty"`
	PageLabelStart    string `json:"page_label_start,omitempty"`
	PageLabelEnd      string `json:"page_label_end,omitempty"`
	Source            string `json:"source"`
	PageSource        string `json:"page_source,omitempty"`
	// PageStart/PageEnd (#220): print pages on epub_cfi locators from a
	// monotone publisher anchor map (pagebreak/class/id/page-map.xml),
	// page_source = print_verified (TOC-proven, #223) or print_unverified.
	// MUST stay in the struct — the persist boundary re-marshals the
	// Locator and drops unknown fields (W9 lesson).
	PageStart *int `json:"page_start,omitempty"`
	PageEnd   *int `json:"page_end,omitempty"`
	// Chapter (W12): 1-based chapter ordinal on corroborated chapter-
	// relative books (folios restart per chapter; healed anchor label
	// sections corroborated by the folio runs). The runner stamps it;
	// rendering composes "Kap. N, S. X" (W4). MUST stay in the struct: the
	// persist boundary re-marshals the Locator, and an unknown JSON field
	// is silently dropped there — the W9 wave lost every stamp this way.
	Chapter  *int   `json:"chapter,omitempty"`
	CFIStart string `json:"cfi_start,omitempty"`
	CFIEnd   string `json:"cfi_end,omitempty"`
	// ParagraphPages (#194): per-paragraph page map [[charOffset, label], …]
	// — boundaries where the chunk's print page changes (first entry always
	// (0, first page)). Consumers derive the exact page of a hit position;
	// the span stays the honest envelope. MUST stay in the struct for the
	// same re-marshal reason as Chapter (the W9 lesson above).
	ParagraphPages [][]any `json:"paragraph_pages,omitempty"`
	// APA-7 citation fields (#245), frozen at EPUB ingest from the book's
	// own heading structure: ChapterNumber = ordinal of the level-1
	// heading, SectionTitle = deepest section at the chunk,
	// ParagraphInChapter = paragraphs from the chapter heading to the
	// chunk start (1-based). Absent when the book has no chapter
	// structure. MUST stay in the struct — the persist boundary re-marshals
	// the Locator and silently drops unknown fields (W9 lesson; review C1
	// of this strand proved the drop end-to-end).
	ChapterNumber      *int   `json:"chapter_number,omitempty"`
	SectionTitle       string `json:"section_title,omitempty"`
	ParagraphInChapter *int   `json:"paragraph_in_chapter,omitempty"`
}

// ChunkStructure carries the ordered heading hierarchy and paragraph range.
type ChunkStructure struct {
	SectionTitles       []string `json:"section_titles"`
	StartParagraphIndex *int     `json:"start_paragraph_index,omitempty"`
	EndParagraphIndex   *int     `json:"end_paragraph_index,omitempty"`
}

// ChunkEmbeddings holds optional dense/sparse vectors for a chunk.
type ChunkEmbeddings struct {
	Dense  *DenseEmbedding  `json:"dense,omitempty"`
	Sparse *SparseEmbedding `json:"sparse,omitempty"`
}

// DenseEmbedding is a fixed-dimension real-valued vector.
type DenseEmbedding struct {
	Model      string    `json:"model"`
	Dimensions int       `json:"dimensions"`
	Values     []float32 `json:"values"`
}

// SparseEmbedding maps bucketed string keys to numeric weights. §10
// serializes the weights as strings; the in-memory shape stays string
// (validate + persist operate on strings).
type SparseEmbedding struct {
	Model  string       `json:"model"`
	Values SparseValues `json:"values"`
}

// SparseValues accepts BOTH §10 string weights and native JSON numbers —
// the reference backend emits numbers (its fill path bypasses the
// stringification), and map[string]string alone made those results
// terminally fail persist (R5 review, verified live). Marshal keeps the
// §10 string form, so the wire shape is unchanged for conforming runners.
type SparseValues map[string]string

func (v *SparseValues) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(SparseValues, len(raw))
	for k, r := range raw {
		if string(r) == "null" {
			return fmt.Errorf("sparse token %q: weight is null", k)
		}
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			out[k] = s
			continue
		}
		var f float64
		if err := json.Unmarshal(r, &f); err == nil {
			out[k] = strconv.FormatFloat(f, 'g', -1, 64)
			continue
		}
		return fmt.Errorf("sparse token %q: weight is neither string nor number", k)
	}
	*v = out
	return nil
}

// Entity is an extracted entity with its chunk mentions.
type Entity struct {
	Ref           string          `json:"ref"`
	Text          string          `json:"text"`
	CanonicalForm string          `json:"canonical_form"`
	Type          string          `json:"type"`
	Description   string          `json:"description"`
	Mentions      []EntityMention `json:"mentions"`
}

// EntityMention is an entity occurrence anchored in a chunk span.
type EntityMention struct {
	ChunkRef   string  `json:"chunk_ref"`
	StartChar  int     `json:"start_char"`
	EndChar    int     `json:"end_char"`
	Confidence float64 `json:"confidence"`
}

// ChunkRelationship relates two chunks; non-sequential types need evidence.
type ChunkRelationship struct {
	SourceChunkRef    string   `json:"source_chunk_ref"`
	TargetChunkRef    string   `json:"target_chunk_ref"`
	Type              string   `json:"type"`
	Strength          float64  `json:"strength"`
	EvidenceChunkRefs []string `json:"evidence_chunk_refs"`
}

// EntityRelationship relates two entities; non-sequential types need evidence (§12).
type EntityRelationship struct {
	SourceEntityRef   string   `json:"source_entity_ref"`
	TargetEntityRef   string   `json:"target_entity_ref"`
	Type              string   `json:"type"`
	Strength          float64  `json:"strength"`
	EvidenceChunkRefs []string `json:"evidence_chunk_refs"`
	Extractor         string   `json:"extractor"`
}

// Stats declares counts that must match the actual arrays (§14).
type Stats struct {
	Pages               int `json:"pages"`
	Chunks              int `json:"chunks"`
	Artifacts           int `json:"artifacts"`
	Entities            int `json:"entities"`
	EntityRelationships int `json:"entity_relationships"`
	ChunkRelationships  int `json:"chunk_relationships"`
}
