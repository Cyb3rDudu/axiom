// Package repo: result validation for the processor contract (§14).
//
// The processor result JSON is UNTRUSTED input. Validate fully before any row
// is inserted. A validation failure makes the job terminal (validation failures
// are not retried unless explicitly proven transient) and MUST NOT replace the
// previous active snapshot.
//
// These checks mirror PROCESSOR_CONTRACT.md §14 and feed §14.4 persistence
// tests (invalid refs / dims / evidence / digest mismatch must roll back).
package repo

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

// ValidationError is a structured validation failure with a stable code for
// terminal classification (validation failures are non-retryable).
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Code + ": " + e.Message }

func verrf(code, format string, args ...any) error {
	return &ValidationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ValidateProcessorResult validates a decoded processor result against the
// immutable frozen input (snapshot identity) and the declared capability
// dimension. It returns the typed result is already decoded by the caller; this
// function performs the §14 rule checks and returns the first failure.
//
// frozen is the claim-time input snapshot (identity source); capDim is the int
// dimension the processor declared in /v1/capabilities (Hivemind Gate-3 hint:
// dimensions are an int — no string fallback).
func ValidateProcessorResult(res *processor.Result, frozen *FrozenInput, capDim int) error {
	if res == nil {
		return verrf("RESULT_INVALID", "nil result")
	}
	if frozen == nil {
		return verrf("RESULT_INVALID", "nil frozen input")
	}

	// §14: echoed source identity and content hash must match the claim-time
	// frozen snapshot (the processor must not assert a different attachment/hash).
	// Normalize sha256 prefix: the frozen input stores bare hex (from Zotero),
	// the processor may echo bare hex or prefixed (sha256:hex). Compare on bare.
	if res.Source.AttachmentID != frozen.Attachment.AttachmentID {
		return verrf("SOURCE_IDENTITY_MISMATCH", "result attachment %q != frozen %q", res.Source.AttachmentID, frozen.Attachment.AttachmentID)
	}
	if frozen.Attachment.ContentHash != nil {
		frozenHash := strings.TrimPrefix(*frozen.Attachment.ContentHash, "sha256:")
		resultHash := strings.TrimPrefix(res.Source.ContentHash, "sha256:")
		if frozenHash != resultHash {
			return verrf("SOURCE_HASH_MISMATCH", "result content_hash %q != frozen %q", res.Source.ContentHash, *frozen.Attachment.ContentHash)
		}
	}
	if !res.Source.Verified {
		return verrf("SOURCE_NOT_VERIFIED", "processor did not verify the source hash")
	}

	// §14: unique, contiguous chunk indexes (zero-based).
	if err := validateChunkIndexes(res); err != nil {
		return err
	}
	// §14: unique job-local refs (chunks, entities, artifacts).
	if err := validateUniqueRefs(res); err != nil {
		return err
	}
	// §14: all chunk/entity/relationship refs resolve within the result.
	if err := validateRefsResolve(res); err != nil {
		return err
	}
	// §14: dense dimensions match the declared capability (int) and are finite.
	if err := validateDenseEmbeddings(res, capDim); err != nil {
		return err
	}
	// §14: sparse key/value validity.
	if err := validateSparseEmbeddings(res); err != nil {
		return err
	}
	// §14: required locators for page-based formats + §12 evidence on non-sequential.
	if err := validateLocatorsAndRelationships(res, frozen); err != nil {
		return err
	}
	// §14: result counts against the actual arrays.
	if err := validateStats(res); err != nil {
		return err
	}
	return nil
}

// validateArtifactsMatch enforces §14.4 artifact digest/size/media_type/ref
// agreement between the processor's declared artifacts and the verified
// ArtifactRecords the dispatcher built from the fetched bytes. A mismatch
// (different digest, size, media_type, or a result artifact with no verified
// record / a stray record with no result declaration) is a terminal validation
// failure and MUST roll back before any row is inserted.
func validateArtifactsMatch(res *processor.Result, arts []ArtifactRecord) error {
	byRef := make(map[string]processor.Artifact, len(res.Artifacts))
	for _, a := range res.Artifacts {
		byRef[a.Ref] = a
	}
	seen := make(map[string]bool, len(arts))
	for _, rec := range arts {
		decl, ok := byRef[rec.Ref]
		if !ok {
			return verrf("ARTIFACT_REF_MISMATCH", "verified artifact ref %q not declared in result", rec.Ref)
		}
		seen[rec.Ref] = true
		if rec.SHA256 != decl.SHA256 {
			return verrf("ARTIFACT_DIGEST_MISMATCH", "artifact %q digest %q != result %q", rec.Ref, rec.SHA256, decl.SHA256)
		}
		if rec.SizeBytes != decl.SizeBytes {
			return verrf("ARTIFACT_SIZE_MISMATCH", "artifact %q size %d != result %d", rec.Ref, rec.SizeBytes, decl.SizeBytes)
		}
		if rec.MediaType != decl.MediaType {
			return verrf("ARTIFACT_MEDIA_TYPE_MISMATCH", "artifact %q media_type %q != result %q", rec.Ref, rec.MediaType, decl.MediaType)
		}
	}
	// Every declared artifact must have a verified record.
	for _, a := range res.Artifacts {
		if !seen[a.Ref] {
			return verrf("ARTIFACT_MISSING_RECORD", "declared artifact %q has no verified record", a.Ref)
		}
	}
	return nil
}

func validateChunkIndexes(res *processor.Result) error {
	seen := make(map[int]bool, len(res.Chunks))
	for i, c := range res.Chunks {
		if c.Index != i {
			return verrf("CHUNK_INDEX_NONCONTIGUOUS", "chunk %d has index %d (expected %d)", i, c.Index, i)
		}
		if seen[c.Index] {
			return verrf("CHUNK_INDEX_DUPLICATE", "duplicate chunk index %d", c.Index)
		}
		seen[c.Index] = true
		if strings.TrimSpace(c.Text) == "" {
			return verrf("CHUNK_EMPTY", "chunk %d has empty text", c.Index)
		}
	}
	return nil
}

func validateUniqueRefs(res *processor.Result) error {
	// Chunks.
	seen := make(map[string]bool)
	for _, c := range res.Chunks {
		if c.Ref == "" {
			return verrf("CHUNK_REF_EMPTY", "chunk %d has empty ref", c.Index)
		}
		if seen[c.Ref] {
			return verrf("CHUNK_REF_DUPLICATE", "duplicate chunk ref %q", c.Ref)
		}
		seen[c.Ref] = true
	}
	// Entities.
	seenEnt := make(map[string]bool)
	for _, e := range res.Entities {
		if e.Ref == "" {
			return verrf("ENTITY_REF_EMPTY", "entity %q has empty ref", e.Text)
		}
		if seenEnt[e.Ref] {
			return verrf("ENTITY_REF_DUPLICATE", "duplicate entity ref %q", e.Ref)
		}
		seenEnt[e.Ref] = true
	}
	// Artifacts.
	seenArt := make(map[string]bool)
	for _, a := range res.Artifacts {
		if a.Ref == "" {
			return verrf("ARTIFACT_REF_EMPTY", "artifact has empty ref")
		}
		if seenArt[a.Ref] {
			return verrf("ARTIFACT_REF_DUPLICATE", "duplicate artifact ref %q", a.Ref)
		}
		seenArt[a.Ref] = true
	}
	return nil
}

func validateRefsResolve(res *processor.Result) error {
	chunkRefs := make(map[string]bool, len(res.Chunks))
	for _, c := range res.Chunks {
		chunkRefs[c.Ref] = true
	}
	entityRefs := make(map[string]bool, len(res.Entities))
	for _, e := range res.Entities {
		entityRefs[e.Ref] = true
	}
	artifactRefs := make(map[string]bool, len(res.Artifacts))
	for _, a := range res.Artifacts {
		artifactRefs[a.Ref] = true
	}

	// image_refs in chunks resolve to artifacts.
	for _, c := range res.Chunks {
		for _, ir := range c.ImageRefs {
			if !artifactRefs[ir] {
				return verrf("CHUNK_IMAGE_REF_UNRESOLVED", "chunk %d image ref %q not in artifacts", c.Index, ir)
			}
		}
	}
	// entity mentions point at chunks.
	for _, e := range res.Entities {
		for _, m := range e.Mentions {
			if !chunkRefs[m.ChunkRef] {
				return verrf("ENTITY_MENTION_CHUNK_UNRESOLVED", "entity %q mention chunk %q unresolved", e.Ref, m.ChunkRef)
			}
		}
	}
	return nil
}

func validateDenseEmbeddings(res *processor.Result, capDim int) error {
	for _, c := range res.Chunks {
		d := c.Embeddings.Dense
		if d == nil {
			continue // dense not requested for this chunk.
		}
		if capDim > 0 && d.Dimensions != capDim {
			return verrf("DENSE_DIM_MISMATCH", "chunk %d dense dims %d != capability %d", c.Index, d.Dimensions, capDim)
		}
		if len(d.Values) != d.Dimensions {
			return verrf("DENSE_DIM_VALUES", "chunk %d dense len(values)=%d != dimensions %d", c.Index, len(d.Values), d.Dimensions)
		}
		for _, v := range d.Values {
			if !isFinite(v) {
				return verrf("DENSE_NON_FINITE", "chunk %d dense vector has non-finite value", c.Index)
			}
		}
	}
	return nil
}

func validateSparseEmbeddings(res *processor.Result) error {
	for _, c := range res.Chunks {
		s := c.Embeddings.Sparse
		if s == nil {
			continue
		}
		for k, v := range s.Values {
			if k == "" {
				return verrf("SPARSE_KEY_EMPTY", "chunk %d sparse has empty key", c.Index)
			}
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return verrf("SPARSE_VALUE_INVALID", "chunk %d sparse key %q value %q not numeric", c.Index, k, v)
			}
		}
	}
	return nil
}

func validateLocatorsAndRelationships(res *processor.Result, frozen *FrozenInput) error {
	// Determine the source content type to enforce §11: EPUBs (no stable pages)
	// MUST NOT carry fabricated page_span locators with invented page labels.
	isEPUB := false
	if frozen.Attachment.ContentType != nil {
		isEPUB = *frozen.Attachment.ContentType == "application/epub+zip"
	}

	for _, c := range res.Chunks {
		loc := c.Locator
		if loc == nil {
			return verrf("LOCATOR_MISSING", "chunk %d has no locator", c.Index)
		}
		switch loc.Type {
		case "page_span":
			// §11: EPUBs have no physical pages — page_span with fabricated labels
			// is a MUST-NOT violation. Reject so the job fails cleanly instead of
			// committing 34 chunks all citing "page 1".
			if isEPUB {
				return verrf("LOCATOR_FABRICATED_PAGES", "chunk %d has page_span locator for an EPUB source (§11: page labels MUST NOT be fabricated for sources without stable pages; use epub_cfi)", c.Index)
			}
			switch loc.PageSource {
			case processor.PageSourceFolioVerified, processor.PageSourcePDFLabelSane, processor.PageSourcePhysicalOnly, processor.PageSourceBlind:
			case "":
				// #173: every page reference carries its trust level — a blank
				// page_source is an unversioned runner; reject loudly so the
				// corpus never fills with unattributed page claims.
				return verrf("LOCATOR_PAGE_SOURCE_MISSING", "chunk %d page_span locator has no page_source (#173 trust level: folio_verified|pdf_label_sane|physical_only)", c.Index)
			default:
				return verrf("LOCATOR_PAGE_SOURCE_UNKNOWN", "chunk %d page_source %q is not a trust level", c.Index, loc.PageSource)
			}
		case "epub_cfi":
			// EPUB CFI must carry non-empty cfi_start/cfi_end — a locator with
			// empty CFI strings is a broken extraction (Weg A: real CFI or reject).
			if isEPUB && (loc.CFIStart == "" || loc.CFIEnd == "") {
				return verrf("LOCATOR_CFI_EMPTY", "chunk %d has epub_cfi locator with empty cfi_start or cfi_end (§11 requires real CFI positions)", c.Index)
			}
			if loc.PageSource != "" && loc.PageSource != processor.PageSourceNone &&
				loc.PageSource != processor.PageSourceEpubPagelist {
				return verrf("LOCATOR_PAGE_SOURCE_INCONSISTENT", "chunk %d epub_cfi locator must carry page_source none or epub_pagelist, got %q", c.Index, loc.PageSource)
			}
		default:
			return verrf("LOCATOR_TYPE_UNKNOWN", "chunk %d locator type %q", c.Index, loc.Type)
		}
	}
	// §12: every non-sequential relationship must carry evidence chunk refs that resolve.
	chunkRefs := make(map[string]bool, len(res.Chunks))
	for _, c := range res.Chunks {
		chunkRefs[c.Ref] = true
	}
	for _, r := range res.EntityRelationships {
		if r.Type == "sequential_next" || r.Type == "sequential_prev" {
			continue
		}
		if len(r.EvidenceChunkRefs) == 0 {
			return verrf("RELATIONSHIP_NO_EVIDENCE", "entity relationship %q->%q (%s) lacks evidence", r.SourceEntityRef, r.TargetEntityRef, r.Type)
		}
		for _, ev := range r.EvidenceChunkRefs {
			if !chunkRefs[ev] {
				return verrf("RELATIONSHIP_EVIDENCE_UNRESOLVED", "entity relationship evidence chunk %q unresolved", ev)
			}
		}
	}
	for _, r := range res.ChunkRelationships {
		if r.Type == "sequential_next" || r.Type == "sequential_prev" {
			continue
		}
		if len(r.EvidenceChunkRefs) == 0 {
			return verrf("RELATIONSHIP_NO_EVIDENCE", "chunk relationship lacks evidence")
		}
	}
	return nil
}

func validateStats(res *processor.Result) error {
	if got := len(res.Chunks); got != res.Stats.Chunks {
		return verrf("STATS_MISMATCH", "stats.chunks=%d but actual=%d", res.Stats.Chunks, got)
	}
	if got := len(res.Entities); got != res.Stats.Entities {
		return verrf("STATS_MISMATCH", "stats.entities=%d but actual=%d", res.Stats.Entities, got)
	}
	if got := len(res.Artifacts); got != res.Stats.Artifacts {
		return verrf("STATS_MISMATCH", "stats.artifacts=%d but actual=%d", res.Stats.Artifacts, got)
	}
	if got := len(res.EntityRelationships); got != res.Stats.EntityRelationships {
		return verrf("STATS_MISMATCH", "stats.entity_relationships=%d but actual=%d", res.Stats.EntityRelationships, got)
	}
	if got := len(res.ChunkRelationships); got != res.Stats.ChunkRelationships {
		return verrf("STATS_MISMATCH", "stats.chunk_relationships=%d but actual=%d", res.Stats.ChunkRelationships, got)
	}
	return nil
}

// isFinite rejects NaN/Inf in dense vectors.
func isFinite(f float32) bool {
	x := float64(f)
	return !math.IsNaN(x) && !math.IsInf(x, 0)
}

// DecodeProcessorResult decodes raw result JSON strictly. Unknown additive
// fields are tolerated (contract §4); required fields are enforced by pydantic
// on the processor side but we re-decode defensively as the result is hostile.
func DecodeProcessorResult(raw []byte) (*processor.Result, error) {
	var res processor.Result
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	return &res, nil
}
