package repo

// Entity consolidation — originally the wave's standard epilogue (#193,
// owner decision: generation-time merging, no migration, no read-layer
// workaround); since #197 a STANDING mechanism (REST endpoint + post-sync
// hook) around the SAME proven merge. Per-document extraction never merges
// cross-document, so the same concept accumulates N entities (live W9
// finding: deutschland ×194). Same-canonical-form entities across ACTIVE
// snapshots merge into ONE survivor:
//
//   - EXACT form match only (coalesce(canonical_form, text) — the same
//     expression the KG corroboration ranking joins on); fuzzy aliasing is
//     explicitly out.
//   - Survivor: most distinct chunks (mentions), tie -> smallest id —
//     deterministic, re-run stable.
//   - Mentions MOVE to the survivor; a verbatim duplicate (same chunk +
//     span) already on the survivor is SKIPPED — same-snapshot same-form
//     duplicates with identical spans exist live (GLiNER multi-label: same
//     text, two types), and the redundant copy dies with the loser.
//   - Relations re-point on BOTH endpoints; evidence chunk ids unchanged.
//     A relation whose two endpoints merge into one survivor becomes a
//     self-relation — kept (harmless, honest provenance).
//   - Loser rows are deleted AFTER their mentions/relations moved (the
//     entities CASCADE would otherwise destroy them).
//   - Idempotent: a re-run finds no same-form pairs among actives and
//     returns 0.

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// ConsolidationReport is the before/after accounting of one consolidation
// run (#197): entities merged, and the standing duplicate mass — distinct
// canonical forms carried by MORE than one entity across active snapshots
// — counted immediately before and immediately after the merge. A healthy
// run ends at duplicate_forms_after=0; merged=0 with 0/0 is the no-op
// re-run (idempotency pin).
type ConsolidationReport struct {
	Merged               int `json:"merged"`
	DuplicateFormsBefore int `json:"duplicate_forms_before"`
	DuplicateFormsAfter  int `json:"duplicate_forms_after"`
}

// EntityConsolidationDryRun reports the guarded merge blast radius without
// mutating rows. DuplicateForms counts only groups eligible under the same
// guards the apply path uses.
func (r *Repo) EntityConsolidationDryRun(ctx context.Context) (ConsolidationReport, error) {
	groups, losers, err := r.entityConsolidationPlan(ctx, r.pool)
	if err != nil {
		return ConsolidationReport{}, err
	}
	return ConsolidationReport{Merged: losers, DuplicateFormsBefore: groups, DuplicateFormsAfter: groups}, nil
}

// ConsolidateEntitiesReport runs the consolidation and returns the full
// accounting — the surface behind POST /api/kg/consolidate and the
// post-sync standing hook (#197).
func (r *Repo) ConsolidateEntitiesReport(ctx context.Context) (ConsolidationReport, error) {
	before, err := r.duplicateActiveForms(ctx)
	if err != nil {
		return ConsolidationReport{}, fmt.Errorf("consolidate count before: %w", err)
	}
	merged, err := r.consolidateActiveEntities(ctx)
	if err != nil {
		return ConsolidationReport{}, err
	}
	after, err := r.duplicateActiveForms(ctx)
	if err != nil {
		return ConsolidationReport{}, fmt.Errorf("consolidate count after: %w", err)
	}
	return ConsolidationReport{Merged: merged, DuplicateFormsBefore: before, DuplicateFormsAfter: after}, nil
}

// duplicateActiveForms counts guarded merge-eligible duplicate forms across
// ACTIVE snapshots (same form expression and guards as the merge itself).
func (r *Repo) duplicateActiveForms(ctx context.Context) (int, error) {
	groups, _, err := r.entityConsolidationPlan(ctx, r.pool)
	return groups, err
}

// ConsolidateEntities merges same-canonical-form entities across active
// snapshots and returns the number of merged (deleted) entity rows.
// Thin wrapper over ConsolidateEntitiesReport for the CLI epilogue
// (-consolidate-entities) and existing callers.
func (r *Repo) ConsolidateEntities(ctx context.Context) (int, error) {
	rep, err := r.ConsolidateEntitiesReport(ctx)
	return rep.Merged, err
}

// entityConsolidationPlan returns guarded eligible group/loser counts.
func (r *Repo) entityConsolidationPlan(ctx context.Context, db kgSQLRunner) (int, int, error) {
	groups, losers, _, _, err := r.entityConsolidationPairs(ctx, db)
	return groups, losers, err
}

// entityConsolidationPairs builds the exact mutation plan used by both dry-run
// and apply. It ports the W3 binding guards into the destructive merge layer:
// mixed concrete types do not merge; PERSON naked surnames do not merge;
// multi-part identical PERSON names and non-PERSON exact forms still merge.
func (r *Repo) entityConsolidationPairs(ctx context.Context, db kgSQLRunner) (int, int, []string, []string, error) {
	rows, err := db.Query(ctx, `
		SELECT lower(coalesce(e.canonical_form, e.text)) AS form,
		       e.id::text, coalesce(e.canonical_form, e.text),
		       count(DISTINCT m.chunk_id) AS chunks, coalesce(e.type, ''),
		       e.snapshot_id::text
		FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		LEFT JOIN processing_entity_mentions m ON m.entity_id = e.id
		GROUP BY 1, 2, 3, 5, 6
		ORDER BY 1, 2`)
	if err != nil {
		return 0, 0, nil, nil, fmt.Errorf("consolidate load: %w", err)
	}
	groups := map[string][]aliasEnt{}
	for rows.Next() {
		var k string
		var e aliasEnt
		if err := rows.Scan(&k, &e.id, &e.form, &e.chunks, &e.eType, &e.snapID); err != nil {
			rows.Close()
			return 0, 0, nil, nil, fmt.Errorf("consolidate scan: %w", err)
		}
		groups[k] = append(groups[k], e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, nil, nil, err
	}

	eligibleGroups := 0
	var survivors, losers []string
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		members := groups[k]
		if len(members) < 2 {
			continue
		}
		if !familyTypesCompatible(members) || !familyPersonsBindable(members) {
			continue
		}
		sorted := append([]aliasEnt(nil), members...)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].chunks != sorted[j].chunks {
				return sorted[i].chunks > sorted[j].chunks
			}
			return sorted[i].id < sorted[j].id
		})
		surv := sorted[0].id
		eligibleGroups++
		for _, m := range sorted[1:] {
			survivors = append(survivors, surv)
			losers = append(losers, m.id)
		}
	}
	return eligibleGroups, len(losers), survivors, losers, nil
}

// consolidateActiveEntities is the proven merge (#193, c1e0e82 — per-form
// atomic batches, set-based re-points, deterministic survivor).
func (r *Repo) consolidateActiveEntities(ctx context.Context) (int, error) {
	merged := 0
	err := r.withKGMaintenanceTx(ctx, "kg_consolidate_entities", func(tx pgx.Tx) error {
		_, _, survivors, losers, err := r.entityConsolidationPairs(ctx, tx)
		if err != nil {
			return err
		}
		if len(losers) == 0 {
			return r.refreshKGReadModelTx(ctx, tx)
		}
		if err := archiveSupersededEntitiesTx(ctx, tx, survivors, losers); err != nil {
			return fmt.Errorf("consolidate archive losers: %w", err)
		}
		kgHook("kg_consolidate_entities:after_archive")

		// Mentions move per-pair (the unique key's leading column indexes the
		// lookup): a verbatim duplicate (same chunk+span) already on the
		// survivor — from a PRIOR pair of this run or from same-snapshot
		// same-form duplicates (3.5k live collisions) — is skipped; the
		// redundant loser copy dies with the loser.
		for i := range losers {
			if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_mentions m SET entity_id = $1::uuid
			WHERE m.entity_id = $2::uuid
			  AND EXISTS (
			    SELECT 1 FROM processing_entities e
			    JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
			    WHERE e.id = m.entity_id)
			  AND NOT EXISTS (
			    SELECT 1 FROM processing_entity_mentions ex
			    WHERE ex.entity_id = $1::uuid
			      AND ex.chunk_id = m.chunk_id
			      AND ex.start_char = m.start_char
			      AND ex.end_char = m.end_char)`,
				survivors[i], losers[i]); err != nil {
				return fmt.Errorf("consolidate move mentions %s: %w", losers[i], err)
			}
		}
		// Relations re-point on both endpoints (set-based; the table has no
		// endpoint indexes, per-pair loops would seq-scan per loser).
		kgHook("kg_consolidate_entities:after_mentions")
		if _, err := tx.Exec(ctx, `
		UPDATE processing_entity_relationships r SET source_entity_id = v.surv
		FROM (SELECT unnest($1::uuid[]) AS surv, unnest($2::uuid[]) AS lose) v
		WHERE r.source_entity_id = v.lose
		  AND EXISTS (SELECT 1 FROM processing_snapshots s WHERE s.id = r.snapshot_id AND s.active)`,
			survivors, losers); err != nil {
			return fmt.Errorf("consolidate repoint source: %w", err)
		}
		if _, err := tx.Exec(ctx, `
		UPDATE processing_entity_relationships r SET target_entity_id = v.surv
		FROM (SELECT unnest($1::uuid[]) AS surv, unnest($2::uuid[]) AS lose) v
		WHERE r.target_entity_id = v.lose
		  AND EXISTS (SELECT 1 FROM processing_snapshots s WHERE s.id = r.snapshot_id AND s.active)`,
			survivors, losers); err != nil {
			return fmt.Errorf("consolidate repoint target: %w", err)
		}
		// Losers die AFTER their rows moved (the entities CASCADE would
		// otherwise destroy still-referenced mentions/relations).
		kgHook("kg_consolidate_entities:before_delete")
		if _, err := tx.Exec(ctx, `
		DELETE FROM processing_entities e
		USING processing_snapshots s
		WHERE e.snapshot_id = s.id AND s.active AND e.id = ANY($1::uuid[])`, losers); err != nil {
			return fmt.Errorf("consolidate delete losers: %w", err)
		}
		merged = len(losers)
		return r.refreshKGReadModelTx(ctx, tx)
	})
	if err != nil {
		return 0, err
	}
	return merged, nil
}
