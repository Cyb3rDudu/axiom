package repo

// Flexion alias binding (#198-3): variants of a lemma family
// (Nachhaltigkeitsbericht/-berichte/-berichts, Stakeholder/stakeholders/
// Stakeholdern) stay separate rows — exact-form entity consolidation
// (#197) deliberately does not merge different forms — but they get an
// alias link (alias_of = survivor id) so the graph leads the family as
// ONE node with a forms list.
//
//   - Lemma folding is deterministic suffix stripping (DE + EN): one of
//     [es, en, s, n, e] (longest first), only when the stem is >= 5 chars.
//     Family = forms sharing the same stem. "management" ends in none of
//     the suffixes after the >=5 guard on the STEM (strip 'e'? 'management'
//     ends 't' — no match), so it folds nowhere.
//   - Survivor: most distinct chunks (mentions), tie -> smallest id —
//     the #197 discipline. Variants get alias_of = survivor.
//   - Idempotent: families whose members already point at the same
//     survivor are untouched; re-runs are no-ops.
//   - No #197 interference: alias variants keep their own canonical_form
//     (search still finds every form) and their own mentions.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// EntityAliasReport is the accounting of one alias-binding run.
type EntityAliasReport struct {
	Families       int `json:"families"`
	VariantsLinked int `json:"variants_linked"`
	FormsCollected int `json:"forms_collected"` // survivor forms + variant forms
	AlreadyBound   int `json:"already_bound"`
}

var aliasSuffixes = []string{"es", "en", "s", "n", "e"}

func aliasStem(form string) string {
	f := strings.ToLower(strings.Join(strings.Fields(form), " "))
	for _, suf := range aliasSuffixes {
		if strings.HasSuffix(f, suf) {
			stem := strings.TrimSuffix(f, suf)
			if len(stem) >= 5 {
				return stem
			}
		}
	}
	return f
}

// EntityAliasCounts reports the blast radius without mutating.
func (r *Repo) EntityAliasCounts(ctx context.Context) (EntityAliasReport, error) {
	return r.entityAlias(ctx, false)
}

// BindFlexionAliases applies the alias links.
func (r *Repo) BindFlexionAliases(ctx context.Context) (EntityAliasReport, error) {
	return r.entityAlias(ctx, true)
}

func (r *Repo) entityAlias(ctx context.Context, apply bool) (EntityAliasReport, error) {
	rep := EntityAliasReport{}
	rows, err := r.pool.Query(ctx, `
		SELECT e.id::text, coalesce(e.canonical_form, e.text),
		       count(DISTINCT m.chunk_id) AS chunks, e.alias_of::text
		FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		LEFT JOIN processing_entity_mentions m ON m.entity_id = e.id
		WHERE e.alias_of IS NULL OR true -- report includes already-bound rows
		GROUP BY 1, 2, 4
		ORDER BY 2`)
	if err != nil {
		return rep, fmt.Errorf("alias load: %w", err)
	}
	type ent struct {
		id, form, aliasOf string
		chunks            int
	}
	families := map[string][]ent{}
	for rows.Next() {
		var e ent
		var ao *string
		if err := rows.Scan(&e.id, &e.form, &e.chunks, &ao); err != nil {
			rows.Close()
			return rep, fmt.Errorf("alias scan: %w", err)
		}
		if ao != nil {
			e.aliasOf = *ao
		}
		families[aliasStem(e.form)] = append(families[aliasStem(e.form)], e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	type binding struct{ variant, survivor string }
	var todo []binding
	var survivorIDs []string
	for _, members := range families {
		if len(members) < 2 {
			continue
		}
		// Distinct surviving roots already present: skip fully-bound families.
		roots := map[string]bool{}
		for _, m := range members {
			if m.aliasOf != "" {
				roots[m.aliasOf] = true
			}
		}
		sorted := append([]ent(nil), members...)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].chunks != sorted[j].chunks {
				return sorted[i].chunks > sorted[j].chunks
			}
			return sorted[i].id < sorted[j].id
		})
		surv := sorted[0]
		if len(roots) == 1 && roots[surv.id] {
			// every bound member points at the current survivor already
			allBound := true
			for _, m := range members {
				if m.id != surv.id && m.aliasOf != surv.id {
					allBound = false
				}
			}
			if allBound {
				rep.AlreadyBound++
				continue
			}
		}
		rep.Families++
		survivorIDs = append(survivorIDs, surv.id)
		for _, m := range members {
			if m.id == surv.id || m.aliasOf == surv.id {
				continue
			}
			todo = append(todo, binding{m.id, surv.id})
		}
	}
	rep.VariantsLinked = len(todo)
	rep.FormsCollected = rep.VariantsLinked + rep.Families*2 // survivor + >=1 variant each

	if !apply || len(todo) == 0 {
		return rep, nil
	}
	for _, b := range todo {
		if _, err := r.pool.Exec(ctx, `
			UPDATE processing_entities SET alias_of = $2::uuid WHERE id = $1::uuid`,
			b.variant, b.survivor); err != nil {
			return rep, fmt.Errorf("alias bind %s: %w", b.variant, err)
		}
	}
	// W3: a re-elected survivor must not keep its own stale alias_of from
	// a previous run — the family lead node cannot be an alias of anything.
	if _, err := r.pool.Exec(ctx, `
		UPDATE processing_entities SET alias_of = NULL
		WHERE id = ANY($1::uuid[]) AND alias_of IS NOT NULL`,
		survivorIDs); err != nil {
		return rep, fmt.Errorf("alias clear survivor: %w", err)
	}
	return rep, nil
}

// KGEntityFamilyForms lists the family forms of a node: the survivor's
// own form plus every variant bound via alias_of (one node, forms list).
func (r *Repo) KGEntityFamilyForms(ctx context.Context, entityID string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT coalesce(e.canonical_form, e.text)
		FROM processing_entities e
		WHERE e.id = $1::uuid
		   OR e.alias_of = $1::uuid
		ORDER BY 1`, entityID)
	if err != nil {
		return nil, fmt.Errorf("family forms: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RepointAliasEdges (#198-3 NACHZUG): edges whose source or target is an
// alias VARIANT re-point to the family survivor. The resulting pair
// duplicates are then resolved by -consolidate-relations (idempotent —
// this must run BEFORE the consolidation apply).
func (r *Repo) RepointAliasEdges(ctx context.Context) error {
	if _, err := r.pool.Exec(ctx, `
		UPDATE processing_entity_relationships r
		SET source_entity_id = s.id
		FROM processing_entities v
		JOIN processing_entities s ON s.id = v.alias_of
		WHERE r.source_entity_id = v.id`); err != nil {
		return fmt.Errorf("repoint source: %w", err)
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE processing_entity_relationships r
		SET target_entity_id = s.id
		FROM processing_entities v
		JOIN processing_entities s ON s.id = v.alias_of
		WHERE r.target_entity_id = v.id`); err != nil {
		return fmt.Errorf("repoint target: %w", err)
	}
	return nil
}
