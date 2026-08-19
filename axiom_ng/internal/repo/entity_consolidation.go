package repo

// Entity consolidation — the wave's standard epilogue (#193, owner
// decision: generation-time merging, no migration, no read-layer
// workaround). Per-document extraction never merges cross-document, so the
// same concept accumulates N entities (live W9 finding: deutschland ×194).
// After a wave drains, same-canonical-form entities across ACTIVE snapshots
// merge into ONE survivor:
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

// ConsolidateEntities merges same-canonical-form entities across active
// snapshots and returns the number of merged (deleted) entity rows.
func (r *Repo) ConsolidateEntities(ctx context.Context) (int, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("consolidate begin: %w", err)
	}
	defer tx.Rollback(ctx)

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
		return 0, fmt.Errorf("consolidate pairs: %w", err)
	}
	var survivors, losers []string
	for rows.Next() {
		var sv, lo string
		if err := rows.Scan(&sv, &lo); err != nil {
			rows.Close()
			return 0, fmt.Errorf("consolidate scan: %w", err)
		}
		survivors = append(survivors, sv)
		losers = append(losers, lo)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(losers) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// Mentions move per-pair (the unique key's leading column indexes the
	// lookup): a verbatim duplicate (same chunk+span) already on the
	// survivor — from a PRIOR pair of this run or from same-snapshot
	// same-form duplicates (3.5k live collisions) — is skipped; the
	// redundant loser copy dies with the loser.
	for i := range losers {
		if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_mentions m SET entity_id = $1::uuid
			WHERE m.entity_id = $2::uuid
			  AND NOT EXISTS (
			    SELECT 1 FROM processing_entity_mentions ex
			    WHERE ex.entity_id = $1::uuid
			      AND ex.chunk_id = m.chunk_id
			      AND ex.start_char = m.start_char
			      AND ex.end_char = m.end_char)`,
			survivors[i], losers[i]); err != nil {
			return 0, fmt.Errorf("consolidate move mentions %s: %w", losers[i], err)
		}
	}
	// Relations re-point on both endpoints (set-based; the table has no
	// endpoint indexes, per-pair loops would seq-scan per loser).
	if _, err := tx.Exec(ctx, `
		UPDATE processing_entity_relationships r SET source_entity_id = v.surv
		FROM (SELECT unnest($1::uuid[]) AS surv, unnest($2::uuid[]) AS lose) v
		WHERE r.source_entity_id = v.lose`,
		survivors, losers); err != nil {
		return 0, fmt.Errorf("consolidate repoint source: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE processing_entity_relationships r SET target_entity_id = v.surv
		FROM (SELECT unnest($1::uuid[]) AS surv, unnest($2::uuid[]) AS lose) v
		WHERE r.target_entity_id = v.lose`,
		survivors, losers); err != nil {
		return 0, fmt.Errorf("consolidate repoint target: %w", err)
	}
	// Losers die AFTER their rows moved (the entities CASCADE would
	// otherwise destroy still-referenced mentions/relations).
	if _, err := tx.Exec(ctx, `
		DELETE FROM processing_entities e
		WHERE e.id = ANY($1::uuid[])`, losers); err != nil {
		return 0, fmt.Errorf("consolidate delete losers: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("consolidate commit: %w", err)
	}
	return len(losers), nil
}
