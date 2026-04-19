package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/gpuworker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// GLiNERLabels is the zero-shot label set the Python extractor sends
// to GLiNER. Order matches axiom_backend/ai_researcher/core_rag/
// entity_extractor.py:43-50 so the GPU worker returns the same hits.
var GLiNERLabels = []string{
	"person",
	"organization",
	"location",
	"concept",
	"book or journal",
	"research method",
}

// glinerTypeMap projects GLiNER labels to the canonical entity types
// stored in document_entities.entity_type. Unlisted labels are
// dropped. Mirrors entity_extractor.py:52-60.
var glinerTypeMap = map[string]string{
	"person":          "PERSON",
	"organization":    "ORGANIZATION",
	"location":        "LOCATION",
	"concept":         "CONCEPT",
	"book or journal": "WORK",
	"research method": "METHOD",
}

// DefaultGLiNERThreshold matches Python's default.
const DefaultGLiNERThreshold = 0.45

// noiseRe drops the "et al" suffix that academic text injects; same
// filter Python applies.
var noiseRe = regexp.MustCompile(`(?i)\bet\s+al\.?$`)

// genericWords are one-token strings that pass GLiNER's threshold but
// add noise. Matches entity_extractor.py:66-69.
var genericWords = map[string]struct{}{
	"firm": {}, "firms": {}, "workers": {}, "government": {}, "governments": {},
	"countries": {}, "borrowers": {}, "savers": {}, "lenders": {}, "households": {},
}

// punctRe strips anything that isn't a letter, digit, or whitespace —
// used to build canonical_form. Python uses re.sub(r'[^\w\s]', ”, ...).
var punctRe = regexp.MustCompile(`[^\p{L}\p{N}\s]`)

// ExtractEntitiesClient is the subset of gpuworker.Client EntityProcessor
// depends on. Kept narrow so tests can stub it without the full GPU
// transport.
type ExtractEntitiesClient interface {
	ExtractEntities(ctx context.Context, text string, labels []string, threshold float64, multiLabel bool) ([]gpuworker.Entity, error)
}

// EntityStore is the subset of repo.Entities used by EntityProcessor.
type EntityStore interface {
	UpsertEntity(ctx context.Context, in repo.EntityUpsert) (uuid.UUID, error)
	LinkChunk(ctx context.Context, in repo.OccurrenceLink) error
	DeleteForDoc(ctx context.Context, docID uuid.UUID) error
}

// EntityProcessor runs the GPU worker's GLiNER extractor over every
// chunk persisted by ChunkProcessor and writes the results to
// document_entities + entity_chunk_occurrences.
//
// Parity target: axiom_backend/ai_researcher/core_rag/
// entity_extractor.py (_extract_with_gliner).
type EntityProcessor struct {
	Chunks    ChunkReader
	Entities  EntityStore
	GPU       ExtractEntitiesClient
	Threshold float64 // 0 → DefaultGLiNERThreshold
	Logger    *slog.Logger
}

// Process implements Processor.
func (p EntityProcessor) Process(ctx context.Context, job Job) error {
	if p.Chunks == nil {
		return fmt.Errorf("entity_processor: Chunks not configured")
	}
	if p.Entities == nil {
		return fmt.Errorf("entity_processor: Entities not configured")
	}
	if p.GPU == nil {
		// GPU worker unavailable — treat as disabled. Same pattern the
		// ChunkProcessor uses for SkipEmbeddings.
		return nil
	}

	chunks, err := p.Chunks.ListForDoc(ctx, job.DocID)
	if err != nil {
		return fmt.Errorf("entity_processor: read chunks: %w", err)
	}
	// Delete old occurrences so a reprocessed document doesn't
	// accumulate stale entity→chunk edges. Entity rows themselves are
	// shared across documents by canonical_form, so we only clear the
	// per-chunk edges here.
	if err := p.Entities.DeleteForDoc(ctx, job.DocID); err != nil {
		return fmt.Errorf("entity_processor: clear occurrences: %w", err)
	}

	threshold := p.Threshold
	if threshold <= 0 {
		threshold = DefaultGLiNERThreshold
	}
	log := p.logger()
	var total int
	for _, chunk := range chunks {
		entities, err := p.GPU.ExtractEntities(ctx, chunk.Text, GLiNERLabels, threshold, true)
		if err != nil {
			return fmt.Errorf("entity_processor: extract for chunk %s: %w", chunk.ChunkID, err)
		}
		accepted := filterAndDedupe(entities)
		for _, ent := range accepted {
			canonical := normalizeCanonical(ent.Text)
			entityID, err := p.Entities.UpsertEntity(ctx, repo.EntityUpsert{
				Text:          ent.Text,
				Type:          ent.Type,
				CanonicalForm: canonical,
			})
			if err != nil {
				return fmt.Errorf("entity_processor: upsert %q: %w", ent.Text, err)
			}
			if err := p.Entities.LinkChunk(ctx, repo.OccurrenceLink{
				EntityID:        entityID,
				ChunkID:         chunk.ChunkID,
				DocID:           job.DocID,
				PositionInChunk: int32(ent.Position),
				RelevanceScore:  ent.Confidence,
				ContextSnippet:  "",
			}); err != nil {
				return fmt.Errorf("entity_processor: link %q: %w", ent.Text, err)
			}
			total++
		}
	}
	log.Info("entities extracted",
		slog.String("doc_id", job.DocID.String()),
		slog.Int("chunks", len(chunks)),
		slog.Int("occurrences", total),
	)
	return nil
}

func (p EntityProcessor) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// extractedEntity is the project-level view after Python-parity
// filtering (label→type map, noise/generic/length filters, dedup).
type extractedEntity struct {
	Text       string
	Type       string
	Position   int
	Confidence float64
}

// filterAndDedupe reproduces the accept/reject logic from
// entity_extractor.py:177-206. Keeps the order of the first
// occurrence of each (text, type) key within the chunk.
func filterAndDedupe(raw []gpuworker.Entity) []extractedEntity {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]extractedEntity, 0, len(raw))
	for _, e := range raw {
		text := strings.TrimSpace(e.Text)
		entType, ok := glinerTypeMap[e.Label]
		if !ok {
			continue
		}
		if len(text) < 2 || len(text) > 100 {
			continue
		}
		if noiseRe.MatchString(text) {
			continue
		}
		if len(strings.Fields(text)) == 1 {
			if _, gen := genericWords[strings.ToLower(text)]; gen {
				continue
			}
		}
		key := strings.ToLower(text) + "\x00" + entType
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, extractedEntity{
			Text:       text,
			Type:       entType,
			Position:   e.Start,
			Confidence: roundTo(e.Score, 3),
		})
	}
	return out
}

// normalizeCanonical lowercases and strips punctuation. Matches
// entity_extractor.py:_normalize.
func normalizeCanonical(s string) string {
	return strings.TrimSpace(punctRe.ReplaceAllString(strings.ToLower(s), ""))
}

// roundTo rounds f to n decimal places. Python uses round(score, 3);
// we approximate the same result with float arithmetic.
func roundTo(f float64, decimals int) float64 {
	mul := 1.0
	for i := 0; i < decimals; i++ {
		mul *= 10
	}
	if f >= 0 {
		return float64(int64(f*mul+0.5)) / mul
	}
	return float64(int64(f*mul-0.5)) / mul
}
