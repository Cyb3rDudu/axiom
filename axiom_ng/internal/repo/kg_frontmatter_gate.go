package repo

// #198 item 1 — the extraction-side KG frontmatter gate. Runs at persist
// time on every natural (re)processing: entities/relationships whose
// evidence sits in gated frontmatter section classes (TOC, author lists,
// preface, bibliography, index, chapter byline/title lines) never enter
// the KG. No rebuild, no runner change — the gate rides the next persist.
//
// Semantics (mirrored by the corpus cleanup, kg_frontmatter_cleanup.go):
//   - a mention in a gated chunk is dropped;
//   - an entity with NO remaining mention is dropped (all-frontmatter
//     sourcing);
//   - a relationship is dropped when ALL its evidence chunks are gated OR
//     an endpoint entity was dropped;
//   - a surviving relationship keeps only its ungated evidence refs
//     (gated chunks are not KG evidence).
//
// Chunk relationships (sequential surface) are untouched — #198 scopes the
// ENTITY graph.

import (
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/frontmatter"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

// FrontmatterGateStats reports what one persist-time gate pass dropped.
type FrontmatterGateStats struct {
	EntitiesDropped       int
	EntityMentionsDropped int
	RelationshipsDropped  int
}

// gateKgFrontmatter filters res IN PLACE and returns the drop accounting.
// Idempotent: a second pass over already-filtered res drops nothing.
func gateKgFrontmatter(res *processor.Result) FrontmatterGateStats {
	var st FrontmatterGateStats
	if len(res.Entities) == 0 && len(res.EntityRelationships) == 0 {
		return st
	}
	gated := make(map[string]bool, len(res.Chunks))
	for _, c := range res.Chunks {
		if frontmatter.Classify(c.Text) != frontmatter.ClassNone {
			gated[c.Ref] = true
		}
	}
	if len(gated) == 0 {
		return st
	}

	// Entities: drop gated mentions, then entities without remaining mentions.
	droppedEntity := make(map[string]bool, len(res.Entities))
	keptEntities := make([]processor.Entity, 0, len(res.Entities))
	for _, e := range res.Entities {
		mentions := make([]processor.EntityMention, 0, len(e.Mentions))
		for _, m := range e.Mentions {
			if gated[m.ChunkRef] {
				st.EntityMentionsDropped++
				continue
			}
			mentions = append(mentions, m)
		}
		if len(mentions) == 0 && len(e.Mentions) > 0 {
			droppedEntity[e.Ref] = true
			st.EntitiesDropped++
			continue
		}
		e.Mentions = mentions
		keptEntities = append(keptEntities, e)
	}

	// Relationships: all-gated evidence or a dropped endpoint kills the
	// edge; survivors keep only ungated evidence refs.
	keptRels := make([]processor.EntityRelationship, 0, len(res.EntityRelationships))
	for _, rel := range res.EntityRelationships {
		if droppedEntity[rel.SourceEntityRef] || droppedEntity[rel.TargetEntityRef] {
			st.RelationshipsDropped++
			continue
		}
		ev := make([]string, 0, len(rel.EvidenceChunkRefs))
		for _, ref := range rel.EvidenceChunkRefs {
			if gated[ref] {
				continue
			}
			ev = append(ev, ref)
		}
		if len(ev) == 0 && len(rel.EvidenceChunkRefs) > 0 {
			// every evidence chunk gated — the edge has no support left
			st.RelationshipsDropped++
			continue
		}
		rel.EvidenceChunkRefs = ev
		keptRels = append(keptRels, rel)
	}

	res.Entities = keptEntities
	res.EntityRelationships = keptRels
	return st
}
