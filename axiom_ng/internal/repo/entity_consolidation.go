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

// duplicateActiveForms counts distinct canonical forms carried by more
// than one entity across ACTIVE snapshots (same form expression as the
// merge itself).
func (r *Repo) duplicateActiveForms(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT coalesce(e.canonical_form, e.text) AS form
			FROM processing_entities e
			JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
			GROUP BY 1 HAVING count(*) > 1
		) d`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ConsolidateEntities merges same-canonical-form entities across active
// snapshots and returns the number of merged (deleted) entity rows.
// Thin wrapper over ConsolidateEntitiesReport for the CLI epilogue
// (-consolidate-entities) and existing callers.
func (r *Repo) ConsolidateEntities(ctx context.Context) (int, error) {
	rep, err := r.ConsolidateEntitiesReport(ctx)
	return rep.Merged, err
}

// consolidateActiveEntities is the proven merge (#193, c1e0e82 — per-form
// atomic batches, set-based re-points, deterministic survivor).
func (r *Repo) consolidateActiveEntities(ctx context.Context) (int, error) {
	merged := 0
	err := r.withKGMaintenanceTx(ctx, "kg_consolidate_entities", func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
		WITH forms AS (
			SELECT coalesce(e.canonical_form, e.text) AS form, e.id,
			       count(DISTINCT m.chunk_id) AS chunks
			FROM processing_entities e
			JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
			LEFT JOIN processing_entity_mentions m ON m.entity_id = e.id
			GROUP BY 1, 2
		),
		ranked AS (
			SELECT id,
			       first_value(id) OVER (PARTITION BY form ORDER BY chunks DESC, id)::text AS survivor,
			       row_number() OVER (PARTITION BY form ORDER BY chunks DESC, id) AS rn
			FROM forms
		)
		SELECT survivor, id::text AS loser
		FROM ranked
		WHERE rn > 1
		ORDER BY 1, 2`)
		if err != nil {
			return fmt.Errorf("consolidate pairs: %w", err)
		}
		var survivors, losers []string
		for rows.Next() {
			var sv, lo string
			if err := rows.Scan(&sv, &lo); err != nil {
				rows.Close()
				return fmt.Errorf("consolidate scan: %w", err)
			}
			survivors = append(survivors, sv)
			losers = append(losers, lo)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
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

// MergeExactDuplicates (#199 W3 directive): merges EXACT same-form entities
// among active snapshots using the #197 merge semantics (mentions move,
// relations re-point, losers DELETED with archive trail) — NOT the alias
// binding path. This runs in the W6 rebuild sequence BEFORE the guarded
// alias binding. 11,817 exact-duplicate groups in production.
//
// The merge is the #197 entity consolidation restricted to EXACT same-form
// groups (no fuzzy, no flexion). Survivor by #197 discipline: most distinct
// chunks, tie -> smallest id. Mentions move to survivor (verbatim dedup
// via NOT EXISTS guard — same-snapshot duplicates with identical spans
// exist). Relations re-point on both endpoints. Losers deleted AFTER
// their data moved (CASCADE would destroy it).
//
// Idempotent: after merge, no same-form groups remain among actives.
func (r *Repo) MergeExactDuplicates(ctx context.Context) (RelationConsolidationReport, error) {
	rep := RelationConsolidationReport{}
	rows, err := r.pool.Query(ctx, `
		SELECT e.id::text, coalesce(e.canonical_form, e.text),
		       count(DISTINCT m.chunk_id) AS chunks
		FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		LEFT JOIN processing_entity_mentions m ON m.entity_id = e.id
		GROUP BY 1, 2
		ORDER BY 2, 1`)
	if err != nil {
		return rep, fmt.Errorf("merge load: %w", err)
	}
	groups := map[string][][2]any{} // form -> [(id, chunks)]
	for rows.Next() {
		var id, form string
		var chunks int
		if err := rows.Scan(&id, &form, &chunks); err != nil {
			rows.Close()
			return rep, err
		}
		groups[form] = append(groups[form], [2]any{id, chunks})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return rep, err
	}
	defer tx.Rollback(ctx)

	for form, members := range groups {
		if len(members) < 2 {
			continue
		}
		// Survivor: most chunks, tie -> smallest id (#197 discipline)
		surv := members[0]
		for _, m := range members[1:] {
			if m[1].(int) > surv[1].(int) ||
				(m[1].(int) == surv[1].(int) && m[0].(string) < surv[0].(string)) {
				surv = m
			}
		}
		survID := surv[0].(string)
		_ = form // used for grouping only

		for _, m := range members {
			loserID := m[0].(string)
			if loserID == survID {
				continue
			}
			// Move mentions (verbatim-dup guard: skip if survivor already has this span)
			if _, err := tx.Exec(ctx, `
				UPDATE processing_entity_mentions m SET entity_id = $1::uuid
				WHERE m.entity_id = $2::uuid
				  AND NOT EXISTS (
				    SELECT 1 FROM processing_entity_mentions ex
				    WHERE ex.entity_id = $1::uuid
				      AND ex.chunk_id = m.chunk_id
				      AND ex.start_char = m.start_char
				      AND ex.end_char = m.end_char)`,
				survID, loserID); err != nil {
				return rep, fmt.Errorf("merge mentions %s: %w", loserID, err)
			}
			// Re-point relations
			if _, err := tx.Exec(ctx, `
				UPDATE processing_entity_relationships SET source_entity_id = $1::uuid WHERE source_entity_id = $2::uuid`,
				survID, loserID); err != nil {
				return rep, err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE processing_entity_relationships SET target_entity_id = $1::uuid WHERE target_entity_id = $2::uuid`,
				survID, loserID); err != nil {
				return rep, err
			}
			// Delete the loser AFTER its data moved
			if _, err := tx.Exec(ctx, `
				DELETE FROM processing_entities WHERE id = $1::uuid`, loserID); err != nil {
				return rep, err
			}
			rep.CollapsedEdges++
		}
		rep.PairsTouched++
	}

	if err := tx.Commit(ctx); err != nil {
		return rep, err
	}
	return rep, nil
}
