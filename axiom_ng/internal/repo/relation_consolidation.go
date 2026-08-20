package repo

// Relations consolidation (#198-2): per (source,target) pair ONE aggregated
// edge among ACTIVE snapshots. Same discipline as the entity consolidation
// (#193/#197): deterministic arbitration, archived losers (never silent
// deletes), idempotent re-runs.
//
//   - Pair scope: UNORDERED {least(a,b), greatest(a,b)} across active
//     snapshots — after entity consolidation the same concept pair shares
//     entity ids across documents, so edges of many snapshots belong to
//     one pair.
//   - Direction by convention: the orientation with higher corroboration
//     (edge count, then evidence-chunk union size) wins; a tie keeps the
//     canonical orientation least->greatest (smaller entity id source).
//   - Winning type by corroboration: edge count, then evidence union
//     size, then type ascending (full determinism).
//   - Survivor row: smallest id among the winning direction + type —
//     keeps its snapshot (provenance); strength = max of the winning
//     group; evidence_chunk_ids = UNION of the winning group (corpus-wide
//     triple support; the documents read-layer derives from exactly this
//     evidence).
//   - Losers: every other type of the winning direction AND every type
//     of the losing direction is archived in metadata.superseded_types
//     as {type, direction: as_is|reversed, evidence_chunk_ids, edges} —
//     the evidence trail stays queryable. Existing archive entries are
//     MERGED by (type, direction) so re-consolidation after new syncs
//     accumulates instead of overwriting.
//   - Single-edge pairs are untouched (no metadata stamping) — which is
//     also what makes re-runs no-ops.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// RelationConsolidationReport is the before/after accounting of one run.
type RelationConsolidationReport struct {
	EdgesBefore           int            `json:"edges_before"`
	EdgesAfter            int            `json:"edges_after"`
	MultiEdgePairs        int            `json:"multi_edge_pairs"`
	CollapsedEdges        int            `json:"collapsed_edges"`
	DirectionFlips        int            `json:"direction_flips"`
	SupersededTypeEntries int            `json:"superseded_type_entries"`
	PairsTouched          int            `json:"pairs_touched"`
	BySupersededType      map[string]int `json:"by_superseded_type,omitempty"`
}

type relEdgeRow struct {
	lo, hi           string // unordered pair key
	src, tgt         string // stored direction
	typ              string
	id               string
	evidence         []string
	strength         *float64
	existingMetadata map[string]any
}

// ConsolidateRelationsReport runs the consolidation and returns the full
// accounting (the surface behind the CLI epilogue and the REST route).
func (r *Repo) ConsolidateRelationsReport(ctx context.Context) (RelationConsolidationReport, error) {
	before, pairsBefore, err := r.activeEdgeStats(ctx)
	if err != nil {
		return RelationConsolidationReport{}, fmt.Errorf("relations count before: %w", err)
	}
	rep, err := r.consolidateActiveRelations(ctx)
	if err != nil {
		return RelationConsolidationReport{}, err
	}
	after, _, err := r.activeEdgeStats(ctx)
	if err != nil {
		return RelationConsolidationReport{}, fmt.Errorf("relations count after: %w", err)
	}
	rep.EdgesBefore, rep.EdgesAfter = before, after
	rep.MultiEdgePairs, rep.CollapsedEdges = pairsBefore, before-after
	return rep, nil
}

// activeEdgeStats: total active edges + pairs carrying more than one edge.
func (r *Repo) activeEdgeStats(ctx context.Context) (edges, multiPairs int, err error) {
	if err = r.pool.QueryRow(ctx, `
		SELECT coalesce(sum(cnt), 0), count(*) FILTER (WHERE cnt > 1)
		FROM (
			SELECT count(*) AS cnt
			FROM processing_entity_relationships r
			JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
			GROUP BY least(r.source_entity_id, r.target_entity_id),
			         greatest(r.source_entity_id, r.target_entity_id)
		) p`).Scan(&edges, &multiPairs); err != nil {
		return 0, 0, err
	}
	return edges, multiPairs, nil
}

func (r *Repo) consolidateActiveRelations(ctx context.Context) (RelationConsolidationReport, error) {
	rep := RelationConsolidationReport{BySupersededType: map[string]int{}}
	rows, err := r.pool.Query(ctx, `
		SELECT least(r.source_entity_id, r.target_entity_id)::text,
		       greatest(r.source_entity_id, r.target_entity_id)::text,
		       r.source_entity_id::text, r.target_entity_id::text,
		       r.type, r.id::text, r.evidence_chunk_ids::text, r.strength::float8,
		       r.metadata::text
		FROM processing_entity_relationships r
		JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
		ORDER BY 1, 2, 6`)
	if err != nil {
		return rep, fmt.Errorf("relations load: %w", err)
	}
	groups := map[[2]string][]relEdgeRow{}
	for rows.Next() {
		var e relEdgeRow
		var ev, meta string
		if err := rows.Scan(&e.lo, &e.hi, &e.src, &e.tgt, &e.typ, &e.id, &ev, &e.strength, &meta); err != nil {
			rows.Close()
			return rep, fmt.Errorf("relations scan: %w", err)
		}
		_ = json.Unmarshal([]byte(ev), &e.evidence)
		if meta != "" && meta != "{}" {
			_ = json.Unmarshal([]byte(meta), &e.existingMetadata)
		}
		if e.existingMetadata == nil {
			e.existingMetadata = map[string]any{}
		}
		k := [2]string{e.lo, e.hi}
		groups[k] = append(groups[k], e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return rep, fmt.Errorf("relations consolidate begin: %w", err)
	}
	defer tx.Rollback(ctx)

	type archEntry struct {
		Type      string   `json:"type"`
		Direction string   `json:"direction"`
		Evidence  []string `json:"evidence_chunk_ids"`
		Edges     int      `json:"edges"`
	}
	for k, edges := range groups {
		if len(edges) < 2 {
			continue // single-edge pair: untouched (idempotency)
		}
		rep.PairsTouched++

		forward, reversed := []relEdgeRow{}, []relEdgeRow{}
		for _, e := range edges {
			if e.src == k[0] && e.tgt == k[1] {
				forward = append(forward, e)
			} else {
				reversed = append(reversed, e)
			}
		}
		// Direction arbitration: corroboration = (edge count, evidence
		// union size); tie keeps the canonical least->greatest orientation.
		// After a swap, `forward` holds the majority-direction edges and
		// the survivor keeps that stored direction.
		if len(reversed) > len(forward) ||
			(len(reversed) == len(forward) && unionSize(reversed) > unionSize(forward)) {
			forward, reversed = reversed, forward
		}
		// Winning type within the winning direction.
		byType := map[string][]relEdgeRow{}
		for _, e := range forward {
			byType[e.typ] = append(byType[e.typ], e)
		}
		types := make([]string, 0, len(byType))
		for t := range byType {
			types = append(types, t)
		}
		sort.Slice(types, func(i, j int) bool { // corroboration, then name
			a, b := byType[types[i]], byType[types[j]]
			if len(a) != len(b) {
				return len(a) > len(b)
			}
			ua, ub := unionSize(a), unionSize(b)
			if ua != ub {
				return ua > ub
			}
			return types[i] < types[j]
		})
		winner := byType[types[0]]

		// Survivor: smallest id of the winning group (rows are id-sorted,
		// but the flip above may have reordered — compare explicitly).
		surv := winner[0]
		for _, e := range winner[1:] {
			if e.id < surv.id {
				surv = e
			}
		}
		// One aggregated edge per pair: same-type same-direction duplicates
		// fold INTO the survivor (evidence unioned); only losing types and
		// losing directions get archive entries — their evidence trail
		// stays in metadata, so nothing is silently deleted.
		losers := make([]string, 0, len(edges))
		for _, e := range edges {
			if e.id != surv.id {
				losers = append(losers, e.id)
			}
		}

		// Aggregated values.
		evSet := map[string]bool{}
		var maxStrength *float64
		for _, e := range winner {
			for _, c := range e.evidence {
				evSet[c] = true
			}
			if e.strength != nil && (maxStrength == nil || *e.strength > *maxStrength) {
				s := *e.strength
				maxStrength = &s
			}
		}
		union := make([]string, 0, len(evSet))
		for c := range evSet {
			union = append(union, c)
		}
		sort.Strings(union)

		// Archive: merge existing superseded_types with the new losers —
		// from ALL group members (W1 fix): a re-elected survivor must
		// inherit the previous survivor's archive, or the evidence trail
		// dies with the deleted old survivor on re-run after sync.
		arch := map[[2]string]*archEntry{}
		for _, member := range edges {
			raw, ok := member.existingMetadata["superseded_types"]
			if !ok {
				continue
			}
			arr, ok := raw.([]any)
			if !ok {
				continue
			}
			for _, it := range arr {
				m, _ := it.(map[string]any)
				if m == nil {
					continue
				}
				t, _ := m["type"].(string)
				d, _ := m["direction"].(string)
				if t == "" {
					continue
				}
				key := [2]string{t, d}
				e := arch[key]
				if e == nil {
					e = &archEntry{Type: t, Direction: d, Evidence: []string{}}
					arch[key] = e
				}
				seen := map[string]bool{}
				for _, c := range e.Evidence {
					seen[c] = true
				}
				if evs, ok := m["evidence_chunk_ids"].([]any); ok {
					for _, c := range evs {
						if s, ok := c.(string); ok && !seen[s] {
							e.Evidence = append(e.Evidence, s)
							seen[s] = true
						}
					}
				}
				if n, ok := m["edges"].(float64); ok {
					e.Edges += int(n)
				}
			}
		}
		addArch := func(group []relEdgeRow, direction string) {
			byT := map[string][]relEdgeRow{}
			for _, e := range group {
				byT[e.typ] = append(byT[e.typ], e)
			}
			ts := make([]string, 0, len(byT))
			for t := range byT {
				ts = append(ts, t)
			}
			sort.Strings(ts)
			for _, t := range ts {
				g := byT[t]
				key := [2]string{t, direction}
				e := arch[key]
				if e == nil {
					e = &archEntry{Type: t, Direction: direction, Evidence: []string{}}
					arch[key] = e
				}
				seen := map[string]bool{}
				for _, c := range e.Evidence {
					seen[c] = true
				}
				for _, ed := range g {
					for _, c := range ed.evidence {
						if !seen[c] {
							seen[c] = true
							e.Evidence = append(e.Evidence, c)
						}
					}
				}
				e.Edges += len(g)
				sort.Strings(e.Evidence)
			}
		}
		for _, t := range types[1:] {
			addArch(byType[t], "as_is")
		}
		addArch(reversed, "reversed")
		if len(reversed) > 0 {
			rep.DirectionFlips++
		}

		archList := make([]archEntry, 0, len(arch))
		for _, e := range arch {
			archList = append(archList, *e)
		}
		sort.Slice(archList, func(i, j int) bool {
			if archList[i].Type != archList[j].Type {
				return archList[i].Type < archList[j].Type
			}
			return archList[i].Direction < archList[j].Direction
		})
		for _, e := range archList {
			rep.SupersededTypeEntries++
			rep.BySupersededType[e.Type+"/"+e.Direction]++
		}
		archJSON, err := json.Marshal(archList)
		if err != nil {
			return rep, err
		}
		survMeta := map[string]any{}
		for k, v := range surv.existingMetadata {
			survMeta[k] = v
		}
		var archAny any
		_ = json.Unmarshal(archJSON, &archAny)
		survMeta["superseded_types"] = archAny
		metaJSON, err := json.Marshal(survMeta)
		if err != nil {
			return rep, err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_relationships
			SET evidence_chunk_ids = $2::jsonb, strength = $3, metadata = $4::jsonb
			WHERE id = $1::uuid`,
			surv.id, "["+joinQuoted(union)+"]", maxStrength, string(metaJSON)); err != nil {
			return rep, fmt.Errorf("relations survivor %s: %w", surv.id, err)
		}

		if _, err := tx.Exec(ctx, `
			DELETE FROM processing_entity_relationships WHERE id = ANY($1::uuid[])`,
			losers); err != nil {
			return rep, fmt.Errorf("relations losers: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return rep, fmt.Errorf("relations consolidate commit: %w", err)
	}
	return rep, nil
}

func unionSize(edges []relEdgeRow) int {
	seen := map[string]bool{}
	for _, e := range edges {
		for _, c := range e.evidence {
			seen[c] = true
		}
	}
	return len(seen)
}

func joinQuoted(ids []string) string {
	out := ""
	for i, c := range ids {
		if i > 0 {
			out += ","
		}
		out += `"` + c + `"`
	}
	return out
}

// RelationsConsolidationDryRun counts the multi-edge pairs without
// mutating — blast-radius-first.
func (r *Repo) RelationsConsolidationDryRun(ctx context.Context) (report RelationConsolidationReport, multiPairs int, err error) {
	_, pairs, err := r.activeEdgeStats(ctx)
	if err != nil {
		return RelationConsolidationReport{}, 0, err
	}
	return RelationConsolidationReport{MultiEdgePairs: pairs}, pairs, nil
}
