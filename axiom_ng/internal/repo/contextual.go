// #255 contextual source class — the rule resolution and sync projection.
//
// Two voices in the corpus: literature (citable, trust-graded) and context
// (lecture slides/transcripts: fully searchable at equal rank, never a
// citation target, KG-excluded). Zotero stays the source of truth for
// membership; the rule INPUTS are environment-configured (the established
// Nix-managed config pattern — no rules table, no edit surface):
//
//	AXIOM_CONTEXTUAL_COLLECTIONS  comma-separated collection paths, any depth
//	AXIOM_CONTEXTUAL_TAGS         comma-separated literal tag names
//
// Both are resolved at boot against the already-synced canonical state.
// An unknown path or tag is a LOUD start error, never a silent ignore. The
// resolved collection rule is stabilized on zotero_key so a collection
// rename cannot silently drop the rule mid-lifetime. The persisted outcome
// is the zotero_documents.citation_class projection, recomputed on every
// canonical sync: contextual = member of a ruled collection OR carrying a
// ruled tag (a tag only ever forces contextual — nothing forces citable).
package repo

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ContextualRules is the resolved, boot-validated rule set one sync applies.
// The zero value (no keys, no tags) legitimately means "everything citable".
type ContextualRules struct {
	// CollectionKeys are zotero_keys of the ruled collections (paths
	// resolved once at boot; membership matching rides the stable key).
	CollectionKeys []string
	// Tags are literal Zotero tag names that force contextual.
	Tags []string
}

// Empty reports whether no rule can mark anything contextual.
func (r ContextualRules) Empty() bool {
	return len(r.CollectionKeys) == 0 && len(r.Tags) == 0
}

// ResolveContextualRules validates the configured rule inputs against the
// synced canonical state and returns the resolved rule set. It is the boot
// gate: every configured collection path must resolve to an existing
// (non-deleted) collection — segments walk the parent hierarchy — and every
// configured tag must exist on at least one active document. An unknown
// path/tag returns an error naming it (the caller fatals — loud, never
// silent). A matching-but-deleted collection resolves (the rule rides the
// stable key; a deleted collection simply has no members and the projection
// recomputes citable — reversible by design).
func (r *Repo) ResolveContextualRules(ctx context.Context, paths, tags []string) (ContextualRules, error) {
	var out ContextualRules

	if len(paths) > 0 {
		type coll struct {
			key, name, parent string
			deleted           bool
		}
		byKey := map[string]coll{}
		rows, err := r.pool.Query(ctx, `
			SELECT zotero_key, name, COALESCE(parent_key,''), deleted
			FROM zotero_collections`)
		if err != nil {
			return out, fmt.Errorf("contextual rules: load collections: %w", err)
		}
		for rows.Next() {
			var c coll
			if err := rows.Scan(&c.key, &c.name, &c.parent, &c.deleted); err != nil {
				rows.Close()
				return out, err
			}
			byKey[c.key] = c
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
		// children of key K by name: a same-named child in two sources both
		// match (union) — ambiguity is membership, not error.
		children := map[string]map[string][]string{} // parentKey -> name -> keys
		for _, c := range byKey {
			m := children[c.parent]
			if m == nil {
				m = map[string][]string{}
				children[c.parent] = m
			}
			m[c.name] = append(m[c.name], c.key)
		}

		for _, path := range paths {
			segs := strings.Split(strings.TrimSpace(path), "/")
			// Level roots: top-level collections matching the first segment.
			var level []string
			roots := children[""]
			for _, seg := range segs {
				seg = strings.TrimSpace(seg)
				if seg == "" {
					return out, fmt.Errorf("contextual collection path %q: empty segment (AXIOM_CONTEXTUAL_COLLECTIONS uses paths like VWL/Lectures)", path)
				}
				if level == nil {
					level = roots[seg]
				} else {
					var next []string
					for _, k := range level {
						next = append(next, children[k][seg]...)
					}
					level = next
				}
				if len(level) == 0 {
					return out, fmt.Errorf("contextual collection path %q: no collection %q at this level (AXIOM_CONTEXTUAL_COLLECTIONS must name collections that exist after a sync — sync once or fix the path)",
						path, seg)
				}
			}
			// Deleted matches resolve to their key (see doc comment) but are
			// reported so the operator sees the state they configured.
			var live, dead int
			for _, k := range level {
				if byKey[k].deleted {
					dead++
				} else {
					live++
				}
			}
			if live == 0 && dead > 0 {
				return out, fmt.Errorf("contextual collection path %q: matches only deleted collections (restore the collection in Zotero or remove the path)", path)
			}
			out.CollectionKeys = append(out.CollectionKeys, level...)
		}
	}

	for _, tag := range tags {
		var exists bool
		if err := r.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM zotero_documents
				WHERE NOT deleted
				  AND EXISTS (SELECT 1 FROM jsonb_array_elements(
					CASE WHEN jsonb_typeof(tags) = 'array' THEN tags ELSE '[]'::jsonb END
				) t WHERE t->>'tag'=$1)
			)`, tag).Scan(&exists); err != nil {
			return out, fmt.Errorf("contextual rules: validate tag %q: %w", tag, err)
		}
		if !exists {
			return out, fmt.Errorf("contextual tag %q: no active document carries this tag (AXIOM_CONTEXTUAL_TAGS must name tags that exist after a sync — fix the env or tag one document first)", tag)
		}
		out.Tags = append(out.Tags, tag)
	}
	return out, nil
}

// recomputeCitationClassTx re-derives zotero_documents.citation_class for
// every active document of the source from the CURRENT memberships + tags
// (both were written earlier in the same apply transaction). Runs on every
// sync — the projection is never hand-edited and stays reversible: moving a
// document out of a ruled collection (or removing the tag) recomputes it
// citable on the next sync. An empty rule set legitimately computes all
// citable (no rule can mark anything contextual).
func recomputeCitationClassTx(ctx context.Context, tx pgx.Tx, sourceID string, rules ContextualRules) error {
	var keys, tags []string
	if len(rules.CollectionKeys) == 0 {
		keys = []string{}
	} else {
		keys = rules.CollectionKeys
	}
	if len(rules.Tags) == 0 {
		tags = []string{}
	} else {
		tags = rules.Tags
	}
	_, err := tx.Exec(ctx, `
		UPDATE zotero_documents d
		SET citation_class = CASE WHEN
			    -- ruled tag on the document (only ever forces contextual).
			    -- tags is jsonb: NULL-scalar (no tags) degrades to the empty set.
			    EXISTS (SELECT 1 FROM jsonb_array_elements(
			            CASE WHEN jsonb_typeof(d.tags) = 'array' THEN d.tags ELSE '[]'::jsonb END
			        ) t WHERE t->>'tag' = ANY($2::text[]))
			    -- direct membership in a ruled collection (key-stable;
			    -- Zotero models sub-collection membership explicitly,
			    -- same convention as the #166 selection expansion)
			 OR EXISTS (SELECT 1
			        FROM zotero_item_collections ic
			        JOIN zotero_collections c ON c.id = ic.collection_id
			        WHERE ic.item_id = d.canonical_item_id
			          AND c.zotero_key = ANY($3::text[])
			          AND NOT c.deleted)
			THEN 'contextual' ELSE 'citable' END,
			updated_at = now()
		WHERE d.source_id = $1 AND NOT d.deleted`,
		sourceID, tags, keys)
	if err != nil {
		return fmt.Errorf("recompute citation_class: %w", err)
	}
	return nil
}
