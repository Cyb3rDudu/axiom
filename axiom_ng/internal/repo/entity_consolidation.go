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
	"time"

	"github.com/jackc/pgx/v5"
)

// batchTimeout bounds each per-form transaction (owner decision A):
// a pathological batch fails loudly and rolls back ONLY itself.
const batchTimeout = 60 * time.Second

// ConsolidateEntitiesDryRun counts the merge pairs without mutating.
func (r *Repo) ConsolidateEntitiesDryRun(ctx context.Context) (forms, losers int, err error) {
	rows, err := r.pool.Query(ctx, pairsQuery())
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var sv, lo string
		if err := rows.Scan(&sv, &lo); err != nil {
			return 0, 0, err
		}
		seen[sv] = true
		losers++
	}
	return len(seen), losers, rows.Err()
}

func pairsQuery() string {
	return `
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
		ORDER BY 1, 2`
}

// consolidateForm merges all losers of ONE survivor in its own transaction
// (per-form atomicity — no form is ever half-merged across commits).
func (r *Repo) consolidateForm(ctx context.Context, survivor string, losers []string) error {
	bctx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	tx, err := r.pool.BeginTx(bctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("consolidate begin: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, lo := range losers {
		if _, err := tx.Exec(bctx, `
			UPDATE processing_entity_mentions m SET entity_id = $1::uuid
			WHERE m.entity_id = $2::uuid
			  AND NOT EXISTS (
			    SELECT 1 FROM processing_entity_mentions ex
			    WHERE ex.entity_id = $1::uuid
			      AND ex.chunk_id = m.chunk_id
			      AND ex.start_char = m.start_char
			      AND ex.end_char = m.end_char)`,
			survivor, lo); err != nil {
			return fmt.Errorf("consolidate move mentions %s: %w", lo, err)
		}
	}
	if _, err := tx.Exec(bctx, `
		UPDATE processing_entity_relationships r SET source_entity_id = v.surv
		FROM (SELECT unnest($1::uuid[]) AS surv, unnest($2::uuid[]) AS lose) v
		WHERE r.source_entity_id = v.lose`,
		[]string{survivor}, losers); err != nil {
		return fmt.Errorf("consolidate repoint source: %w", err)
	}
	if _, err := tx.Exec(bctx, `
		UPDATE processing_entity_relationships r SET target_entity_id = v.surv
		FROM (SELECT unnest($1::uuid[]) AS surv, unnest($2::uuid[]) AS lose) v
		WHERE r.target_entity_id = v.lose`,
		[]string{survivor}, losers); err != nil {
		return fmt.Errorf("consolidate repoint target: %w", err)
	}
	if _, err := tx.Exec(bctx, `
		DELETE FROM processing_entities e
		WHERE e.id = ANY($1::uuid[])`, losers); err != nil {
		return fmt.Errorf("consolidate delete losers: %w", err)
	}
	return tx.Commit(bctx)
}

// ConsolidateEntities merges same-canonical-form entities across active
// snapshots and returns the number of merged (deleted) entity rows.
// Batched-resume (owner decision A): per-form transactions with progress
// via Progress — a death leaves a consistent partial state; a re-run
// resumes at the un-merged forms. A batch hitting batchTimeout fails
// loudly (progress records it) and the run CONTINUES.
func (r *Repo) ConsolidateEntities(ctx context.Context) (int, error) {
	return r.ConsolidateEntitiesProgress(ctx, func(done, totalForms, merged int, form string, err error) {})
}

// ConsolidateEntitiesProgress is ConsolidateEntities with a progress
// callback per form-batch (done forms, total forms, merged losers so far,
// survivor id, batch error — non-nil means THIS form failed and was
// skipped; the run continues).
func (r *Repo) ConsolidateEntitiesProgress(
	ctx context.Context,
	progress func(done, totalForms, merged int, form string, batchErr error),
) (int, error) {
	rows, err := r.pool.Query(ctx, pairsQuery())
	if err != nil {
		return 0, fmt.Errorf("consolidate pairs: %w", err)
	}
	byForm := map[string][]string{}
	var order []string
	for rows.Next() {
		var sv, lo string
		if err := rows.Scan(&sv, &lo); err != nil {
			rows.Close()
			return 0, fmt.Errorf("consolidate scan: %w", err)
		}
		if _, ok := byForm[sv]; !ok {
			order = append(order, sv)
		}
		byForm[sv] = append(byForm[sv], lo)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := len(order)
	merged := 0
	for i, sv := range order {
		losers := byForm[sv]
		if err := r.consolidateForm(ctx, sv, losers); err != nil {
			// Loud skip: this form stays un-merged and consistent; the
			// run continues (resume re-attempts it).
			progress(i+1, total, merged, sv, err)
			continue
		}
		merged += len(losers)
		progress(i+1, total, merged, sv, nil)
	}
	return merged, nil
}
