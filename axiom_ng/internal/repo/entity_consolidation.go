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
//   - Mentions MOVE to the survivor (their chunk ids are globally unique,
//     so the (entity, chunk, span) unique key can never collide).
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
			SELECT form, id,
			       row_number() OVER (PARTITION BY form ORDER BY chunks DESC, id) AS rn,
			       count(*) OVER (PARTITION BY form) AS n
			FROM forms
		)
		SELECT (SELECT l2.id FROM ranked l2
		        WHERE l2.form = loser.form AND l2.rn = 1)::text AS survivor,
		       loser.id::text AS loser
		FROM ranked loser
		WHERE loser.rn > 1 AND loser.n >= 2
		ORDER BY 1, 2`)
	if err != nil {
		return 0, fmt.Errorf("consolidate pairs: %w", err)
	}
	type pair struct{ survivor, loser string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.survivor, &p.loser); err != nil {
			rows.Close()
			return 0, fmt.Errorf("consolidate scan: %w", err)
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, p := range pairs {
		if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_mentions SET entity_id=$1::uuid WHERE entity_id=$2::uuid`,
			p.survivor, p.loser); err != nil {
			return 0, fmt.Errorf("consolidate move mentions %s: %w", p.loser, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_relationships SET source_entity_id=$1::uuid WHERE source_entity_id=$2::uuid`,
			p.survivor, p.loser); err != nil {
			return 0, fmt.Errorf("consolidate repoint source %s: %w", p.loser, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_relationships SET target_entity_id=$1::uuid WHERE target_entity_id=$2::uuid`,
			p.survivor, p.loser); err != nil {
			return 0, fmt.Errorf("consolidate repoint target %s: %w", p.loser, err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM processing_entities WHERE id=$1::uuid`, p.loser); err != nil {
			return 0, fmt.Errorf("consolidate delete loser %s: %w", p.loser, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("consolidate commit: %w", err)
	}
	return len(pairs), nil
}
