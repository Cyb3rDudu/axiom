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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// Pool-based dry-run (no lock needed — no mutation).
	return r.bindByGrouperPool(ctx, aliasStem, false)
}

// BindFlexionAliases applies the alias links.
func (r *Repo) BindFlexionAliases(ctx context.Context) (EntityAliasReport, error) {
	rep := EntityAliasReport{}
	err := r.withKGMaintenanceTx(ctx, "kg_bind_flexion_aliases", func(tx pgx.Tx) error {
		var e error
		rep, e = r.bindByGrouperOn(ctx, tx, aliasStem, true)
		if e != nil {
			return e
		}
		return r.refreshKGReadModelTx(ctx, tx)
	})
	return rep, err
}

type kgSQLRunner interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (r *Repo) entityAlias(ctx context.Context, apply bool) (EntityAliasReport, error) {
	if apply {
		var out EntityAliasReport
		err := r.withKGMaintenanceTx(ctx, "kg_bind_flexion_aliases", func(tx pgx.Tx) error {
			var err error
			out, err = entityAliasOn(ctx, tx, true)
			if err != nil {
				return err
			}
			return r.refreshKGReadModelTx(ctx, tx)
		})
		return out, err
	}
	return entityAliasOn(ctx, r.pool, false)
}

func entityAliasOn(ctx context.Context, db kgSQLRunner, apply bool) (EntityAliasReport, error) {
	rep := EntityAliasReport{}
	rows, err := db.Query(ctx, `
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
	hb := newKGHeartbeat("alias bindings", len(todo))
	for i, b := range todo {
		hb.beat(i+1, b.variant)
		if _, err := db.Exec(ctx, `
			UPDATE processing_entities e SET alias_of = $2::uuid
			FROM processing_snapshots s
			WHERE e.snapshot_id = s.id AND s.active AND e.id = $1::uuid`,
			b.variant, b.survivor); err != nil {
			return rep, fmt.Errorf("alias bind %s: %w", b.variant, err)
		}
		if i == 0 {
			kgHook("kg_bind_flexion_aliases:after_first_binding")
		}
	}
	// W3: a re-elected survivor must not keep its own stale alias_of from
	// a previous run — the family lead node cannot be an alias of anything.
	if _, err := db.Exec(ctx, `
		UPDATE processing_entities e SET alias_of = NULL
		FROM processing_snapshots s
		WHERE e.snapshot_id = s.id AND s.active
		  AND e.id = ANY($1::uuid[]) AND e.alias_of IS NOT NULL`,
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
	return r.withKGMaintenanceTx(ctx, "kg_repoint_alias_edges", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_relationships r
			SET source_entity_id = s.id
			FROM processing_entities v
			JOIN processing_entities s ON s.id = v.alias_of
			WHERE r.source_entity_id = v.id
			  AND EXISTS (SELECT 1 FROM processing_snapshots sn WHERE sn.id = r.snapshot_id AND sn.active)`); err != nil {
			return fmt.Errorf("repoint source: %w", err)
		}
		kgHook("kg_repoint_alias_edges:after_source")
		if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_relationships r
			SET target_entity_id = s.id
			FROM processing_entities v
			JOIN processing_entities s ON s.id = v.alias_of
			WHERE r.target_entity_id = v.id
			  AND EXISTS (SELECT 1 FROM processing_snapshots sn WHERE sn.id = r.snapshot_id AND sn.active)`); err != nil {
			return fmt.Errorf("repoint target: %w", err)
		}
		kgHook("kg_repoint_alias_edges:after_target")
		// W1 (review): intra-family edges (variant→survivor) become self-loops
		// (survivor→survivor) after the re-points. No schema constraint forbids
		// them and they serve as API noise (source_form = target_form). Delete.
		if _, err := tx.Exec(ctx, `
			DELETE FROM processing_entity_relationships r
			USING processing_snapshots s
			WHERE r.snapshot_id = s.id AND s.active
			  AND r.source_entity_id = r.target_entity_id`); err != nil {
			return fmt.Errorf("repoint self-loop cleanup: %w", err)
		}
		return r.refreshKGReadModelTx(ctx, tx)
	})
}

// BindExactFormAliases (#198-3/3b goal 1): groups of IDENTICAL
// canonical_form among active snapshots bind like flexion families —
// survivor by #197 discipline (most chunks, tie smallest id), variants
// get alias_of. This covers the gap the stem-folding cannot: after the
// #197 entity consolidation, same-form entities should already be one —
// but new syncs re-create them, and between the auto-trigger and the
// alias binding, identical forms sit unbound. Idempotent.
func (r *Repo) BindExactFormAliases(ctx context.Context) (EntityAliasReport, error) {
	rep := EntityAliasReport{}
	err := r.withKGMaintenanceTx(ctx, "kg_bind_exact_form_aliases", func(tx pgx.Tx) error {
		var e error
		rep, e = r.bindByGrouperOn(ctx, tx, func(form string) string {
			return form
		}, true)
		if e != nil {
			return e
		}
		return r.refreshKGReadModelTx(ctx, tx)
	})
	return rep, err
}

// BindExactFormAliasesDryRun counts without mutating.
func (r *Repo) BindExactFormAliasesDryRun(ctx context.Context) (EntityAliasReport, error) {
	// Dry-run: pool reads (no lock needed — no mutation).
	return r.bindByGrouperPool(ctx, func(form string) string {
		return form
	}, false)
}

// bindByGrouperPool is the pool-based wrapper for dry-run paths.
func (r *Repo) bindByGrouperPool(ctx context.Context, key func(string) string, apply bool) (EntityAliasReport, error) {
	return r.bindByGrouperOn(ctx, r.pool, key, apply)
}

// bindByGrouper is the shared binding engine: group entities by a key
// function, pick the survivor, bind variants. Used by both the flexion
// pass (stem key) and the exact-form pass (identity key).
//
// #198-3/3b GUARDS (owner directive after satellite deep review):
//   - PERSON homonym guard: entities typed PERSON bind ONLY within the
//     same document OR when all family members share the identical full
//     canonical form AND type PERSON. Cross-document surname-only
//     PERSONs (schmidt/müller/martin) NEVER bind — they are different
//     people who happen to share a name.
//   - Type compatibility: families with incompatible types (CONCEPT +
//     ORGANIZATION, PERSON + CONCEPT, etc.) stay unbound — the typing
//     normalization pass resolves types FIRST, then binding revisits.
//     The only compatible pairs are same-type or type-to-CONCEPT (the
//     typing pass promotes generics to CONCEPT, so a CONCEPT survivor
//     can absorb an already-CONCEPT variant, and a null-type variant
//     can join any family — the extractor leaves type empty often).
func (r *Repo) bindByGrouperOn(ctx context.Context, db kgSQLRunner, key func(string) string, apply bool) (EntityAliasReport, error) {
	rep := EntityAliasReport{}
	rows, err := db.Query(ctx, `
		SELECT e.id::text, coalesce(e.canonical_form, e.text),
		       count(DISTINCT m.chunk_id) AS chunks, e.alias_of::text,
		       coalesce(e.type, ''), e.snapshot_id::text
		FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		LEFT JOIN processing_entity_mentions m ON m.entity_id = e.id
		GROUP BY 1, 2, 4, 5, 6
		ORDER BY 2`)
	if err != nil {
		return rep, fmt.Errorf("alias load: %w", err)
	}
	type ent = aliasEnt
	groups := map[string][]ent{}
	for rows.Next() {
		var e ent
		var ao *string
		if err := rows.Scan(&e.id, &e.form, &e.chunks, &ao, &e.eType, &e.snapID); err != nil {
			rows.Close()
			return rep, fmt.Errorf("alias scan: %w", err)
		}
		if ao != nil {
			e.aliasOf = *ao
		}
		k := key(e.form)
		groups[k] = append(groups[k], e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	type binding struct{ variant, survivor string }
	var todo []binding
	var survivorIDs []string
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		// TYPE COMPATIBILITY GUARD: skip families with incompatible types.
		if !familyTypesCompatible(members) {
			continue
		}
		// PERSON HOMONYM GUARD: cross-document PERSONs never bind unless
		// they share the identical full canonical form (same person, not
		// just same surname). We check this AFTER type compatibility.
		if !familyPersonsBindable(members) {
			continue
		}
		sorted := append([]ent(nil), members...)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].chunks != sorted[j].chunks {
				return sorted[i].chunks > sorted[j].chunks
			}
			return sorted[i].id < sorted[j].id
		})
		surv := sorted[0]
		// Skip if every non-survivor member already points at surv.
		allBound := true
		for _, m := range members {
			if m.id != surv.id && m.aliasOf != surv.id {
				allBound = false
				break
			}
		}
		if allBound && surv.aliasOf == "" {
			rep.AlreadyBound++
			continue
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

	if !apply {
		return rep, nil
	}
	hb := newKGHeartbeat("alias bindings", len(todo))
	for i, b := range todo {
		hb.beat(i+1, b.variant)
		if _, err := db.Exec(ctx, `
			UPDATE processing_entities e SET alias_of = $2::uuid
			FROM processing_snapshots s
			WHERE e.snapshot_id = s.id AND s.active AND e.id = $1::uuid`,
			b.variant, b.survivor); err != nil {
			return rep, fmt.Errorf("alias bind %s: %w", b.variant, err)
		}
		if i == 0 {
			kgHook("kg_bind_flexion_aliases:after_first_binding")
		}
	}
	// W3: re-elected survivor clears its own stale alias_of.
	if len(survivorIDs) > 0 {
		if _, err := db.Exec(ctx, `
			UPDATE processing_entities SET alias_of = NULL
			WHERE id = ANY($1::uuid[]) AND alias_of IS NOT NULL`,
			survivorIDs); err != nil {
			return rep, fmt.Errorf("alias clear survivor: %w", err)
		}
	}
	return rep, nil
}

// BindAllAliases runs exact-form binding THEN flexion binding (the
// flexion pass may connect families the exact pass created). Runbook:
// -bind-all-aliases → -normalize-entity-types → -repoint-alias-edges →
// -consolidate-relations --apply.
func (r *Repo) BindAllAliases(ctx context.Context) (EntityAliasReport, error) {
	rep1, err := r.BindExactFormAliases(ctx)
	if err != nil {
		return rep1, err
	}
	rep2, err := r.BindFlexionAliases(ctx)
	if err != nil {
		return rep2, err
	}
	return EntityAliasReport{
		Families:       rep1.Families + rep2.Families,
		VariantsLinked: rep1.VariantsLinked + rep2.VariantsLinked,
		AlreadyBound:   rep1.AlreadyBound + rep2.AlreadyBound,
	}, nil
}

// aliasEnt is the shared member shape for the guard functions.
type aliasEnt struct {
	id, form, aliasOf, eType, snapID string
	chunks                           int
}

// familyTypesCompatible with majority arbitration (#199 Chattys final):
// mixed groups (organisation 15×ORG+8×CONCEPT, unternehmen 35×ORG+1×WORK)
// merge by mention-weighted majority — the outlier no longer blocks the
// whole group. PERSON-majority groups keep the old strict guard (no
// PERSON-fusion through the back door). Only non-PERSON minorities
// may be absorbed.
func familyTypesCompatible(members []aliasEnt) bool {
	// Weighted type census by mention count.
	weights := map[string]int{}
	for _, m := range members {
		if m.eType != "" && m.eType != "CONCEPT" {
			weights[m.eType] += m.chunks
		}
	}
	if len(weights) <= 1 {
		return true // uniform or empty → compatible
	}
	// Find the majority type (highest mention-weighted count).
	majority, maxW := "", 0
	for t, w := range weights {
		if w > maxW {
			majority, maxW = t, w
		}
	}
	// PERSON-majority: strict guard — no PERSON fusion via arbitration.
	if majority == "PERSON" {
		return false
	}
	// Non-PERSON majority: minority non-PERSON types may be absorbed.
	// But PERSON minorities are NEVER absorbed into non-PERSON.
	for _, m := range members {
		if m.eType == "PERSON" && majority != "PERSON" {
			// A PERSON entity in a non-PERSON-majority group: this is a
			// genuine type conflict (a person named "organisation") —
			// block the whole group to avoid mis-typing a real person.
			return false
		}
	}
	return true
}

// familyPersonsBindable: PERSON-typed entities bind ONLY when the shared
// form is a MULTI-PART name (contains whitespace). Naked surnames
// ("schmidt" == "schmidt") NEVER bind — 12,297 active naked-surname PERSONs
// are overwhelmingly different people who share a name; the satellite
// production probe showed the identical-form guard alone let Schmidt go
// from 9 to 10 bindings (same surname, same type = same "person" to the
// old rule, but a different human).
func familyPersonsBindable(members []aliasEnt) bool {
	hasPerson := false
	for _, m := range members {
		if m.eType == "PERSON" {
			hasPerson = true
			break
		}
	}
	if !hasPerson {
		return true // no PERSONs → no naked-name guard (single-word CONCEPTs bind normally)
	}
	// ALL members must share the same form AND that form must be
	// multi-part (given name + surname = a specific identifiable person).
	firstForm := members[0].form
	if !strings.Contains(strings.TrimSpace(firstForm), " ") {
		return false // naked surname → never bind PERSONs
	}
	for _, m := range members {
		if m.form != firstForm {
			return false // different forms in a PERSON family → homonym risk
		}
	}
	return true
}
