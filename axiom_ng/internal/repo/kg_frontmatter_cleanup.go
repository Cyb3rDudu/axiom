package repo

// #198 item 1 — the frontmatter cleanup pass (SQL + the calibrated Go
// classifier, no GPU): KG relations/entities whose evidence sits in gated
// frontmatter section classes leave the ACTIVE-generation graph. The
// deletion rules mirror the persist-time gate (kg_frontmatter_gate.go)
// exactly, so a cleaned corpus converges to what gated extraction would
// have produced:
//
//   - a relation dies when ALL its evidence chunks are gated OR an endpoint
//     entity dies;
//   - an entity dies when ALL its mentions sit in gated chunks;
//   - a surviving relation keeps only ungated evidence refs (stripped);
//   - gated CHUNKS stay (retrieval keeps them; only KG evidence is gated);
//   - scope: ACTIVE snapshots only — inactive generations keep their
//     evidence until retention removes the snapshots.
//
// Dry-run (apply=false) computes the identical report without mutating.
// Idempotent: a re-run finds no gated evidence left and reports zeros.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/frontmatter"
	"github.com/jackc/pgx/v5"
)

// FrontmatterClassCounts is the blast-radius accounting for one class.
type FrontmatterClassCounts struct {
	Chunks               int `json:"chunks"`
	Relations            int `json:"relations"`
	Entities             int `json:"entities"`
	Mentions             int `json:"mentions"`
	EvidenceRefsStripped int `json:"evidence_refs_stripped"`
}

// FrontmatterCleanupReport carries per-class counts plus totals.
type FrontmatterCleanupReport struct {
	Applied bool                              `json:"applied"`
	Classes map[string]FrontmatterClassCounts `json:"classes"`
	Totals  FrontmatterClassCounts            `json:"totals"`
}

func (c FrontmatterClassCounts) add(o FrontmatterClassCounts) FrontmatterClassCounts {
	return FrontmatterClassCounts{
		Chunks: c.Chunks + o.Chunks, Relations: c.Relations + o.Relations,
		Entities: c.Entities + o.Entities, Mentions: c.Mentions + o.Mentions,
		EvidenceRefsStripped: c.EvidenceRefsStripped + o.EvidenceRefsStripped,
	}
}

// fmMention is one gated mention row (cleanup accounting).
type fmMention struct{ entity, id, chunk string }

// CleanupFrontmatterKG computes (and with apply=true executes) the
// frontmatter cleanup over the active generation.
func (r *Repo) CleanupFrontmatterKG(ctx context.Context, apply bool) (FrontmatterCleanupReport, error) {
	rep := FrontmatterCleanupReport{Applied: apply, Classes: map[string]FrontmatterClassCounts{}}
	clsMap := map[string]*FrontmatterClassCounts{}
	cls := func(class string) *FrontmatterClassCounts {
		c, ok := clsMap[class]
		if !ok {
			c = &FrontmatterClassCounts{}
			clsMap[class] = c
		}
		return c
	}
	defer func() {
		for k, v := range clsMap {
			rep.Classes[k] = *v
		}
	}()

	var tx pgx.Tx
	if apply {
		var err error
		tx, err = r.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return rep, fmt.Errorf("cleanup begin: %w", err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, kgMaintenanceLockKey); err != nil {
			return rep, fmt.Errorf("cleanup lock: %w", err)
		}
		kgHook("kg_cleanup_frontmatter:after_lock")
	}
	q := func(sql string, args ...any) (pgx.Rows, error) {
		if tx != nil {
			return tx.Query(ctx, sql, args...)
		}
		return r.pool.Query(ctx, sql, args...)
	}

	// 1. Classify every chunk that is KG evidence of the ACTIVE generation.
	rows, err := q(`
		WITH ev AS (
		  SELECT DISTINCT m.chunk_id FROM processing_entity_mentions m
		  JOIN processing_entities e ON e.id = m.entity_id
		  JOIN processing_snapshots se ON se.id = e.snapshot_id AND se.active
		  UNION
		  SELECT DISTINCT (jsonb_array_elements_text(r.evidence_chunk_ids))::uuid
		  FROM processing_entity_relationships r
		  JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
		)
		SELECT ch.id::text, ch.text FROM processing_chunks ch JOIN ev ON ev.chunk_id = ch.id`)
	if err != nil {
		return rep, fmt.Errorf("cleanup evidence chunks: %w", err)
	}
	classOf := map[string]frontmatter.Class{}
	gated := make([]string, 0, 64)
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return rep, err
		}
		if class := frontmatter.Classify(text); class != frontmatter.ClassNone {
			classOf[id] = class
			gated = append(gated, id)
			cls(string(class)).Chunks++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	if len(gated) == 0 {
		if tx != nil {
			if err := tx.Commit(ctx); err != nil {
				return rep, err
			}
		}
		return rep, nil
	}

	// 2. Gated mentions of active entities -> entity deletion set.
	rows, err = q(`
		SELECT m.entity_id::text, m.id::text, m.chunk_id::text
		FROM processing_entity_mentions m
		JOIN processing_entities e ON e.id = m.entity_id
		JOIN processing_snapshots se ON se.id = e.snapshot_id AND se.active
		WHERE m.chunk_id = ANY($1::uuid[])`, gated)
	if err != nil {
		return rep, fmt.Errorf("cleanup gated mentions: %w", err)
	}
	gatedMentions := []fmMention{}
	for rows.Next() {
		var m fmMention
		if err := rows.Scan(&m.entity, &m.id, &m.chunk); err != nil {
			rows.Close()
			return rep, err
		}
		gatedMentions = append(gatedMentions, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	gatedByEntity := map[string][]fmMention{}
	for _, m := range gatedMentions {
		gatedByEntity[m.entity] = append(gatedByEntity[m.entity], m)
		cls(string(classOf[m.chunk])).Mentions++
	}
	// total mention counts for the touched entities only
	mentioned := make([]string, 0, len(gatedByEntity))
	for e := range gatedByEntity {
		mentioned = append(mentioned, e)
	}
	rows, err = q(`
		SELECT m.entity_id::text, count(*) FROM processing_entity_mentions m
		WHERE m.entity_id = ANY($1::uuid[]) GROUP BY 1`, mentioned)
	if err != nil {
		return rep, fmt.Errorf("cleanup mention totals: %w", err)
	}
	entDelete := make([]string, 0, len(gatedByEntity))
	entClass := map[string]frontmatter.Class{}
	for rows.Next() {
		var e string
		var total int
		if err := rows.Scan(&e, &total); err != nil {
			rows.Close()
			return rep, err
		}
		if gm := gatedByEntity[e]; len(gm) > 0 && len(gm) == total {
			entDelete = append(entDelete, e)
			entClass[e] = classOf[gm[0].chunk]
			cls(string(classOf[gm[0].chunk])).Entities++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	// 3. Relations: candidates are (a) evidence touching gated chunks,
	// (b) relations with a dying endpoint.
	relCandidates := map[string][4]string{} // id -> {evidenceJSON, src, tgt, class}
	fetchRels := func(where string, args ...any) error {
		rows, err := q(`
			SELECT r.id::text, r.evidence_chunk_ids::text,
			       r.source_entity_id::text, r.target_entity_id::text
			FROM processing_entity_relationships r
			JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
			`+where, args...)
		if err != nil {
			return fmt.Errorf("cleanup relations: %w", err)
		}
		for rows.Next() {
			var id, ev, src, tgt string
			if err := rows.Scan(&id, &ev, &src, &tgt); err != nil {
				rows.Close()
				return err
			}
			if _, dup := relCandidates[id]; !dup {
				relCandidates[id] = [4]string{id, ev, src, tgt}
			}
		}
		rows.Close()
		return rows.Err()
	}
	if err := fetchRels(`WHERE r.evidence_chunk_ids ?| $1::text[]`, textSet(gated)); err != nil {
		return rep, err
	}
	if len(entDelete) > 0 {
		if err := fetchRels(`WHERE r.source_entity_id = ANY($1::uuid[]) OR r.target_entity_id = ANY($1::uuid[])`, entDelete); err != nil {
			return rep, err
		}
	}

	relDelete := make([]string, 0, len(relCandidates))
	relStrip := make([]string, 0, len(relCandidates))
	stripCounts := map[string]int{} // relID -> gated refs removed
	for _, rc := range relCandidates {
		id, evRaw, src, tgt := rc[0], rc[1], rc[2], rc[3]
		var ev []string
		_ = json.Unmarshal([]byte(evRaw), &ev)
		allGated := len(ev) > 0
		gatedRefs := 0
		var firstGatedClass frontmatter.Class
		for _, chunkID := range ev {
			if class, g := classOf[chunkID]; g {
				gatedRefs++
				if firstGatedClass == "" {
					firstGatedClass = class
				}
			} else {
				allGated = false
			}
		}
		srcDead, tgtDead := containsID(entDelete, src), containsID(entDelete, tgt)
		if (allGated && gatedRefs > 0) || srcDead || tgtDead {
			relDelete = append(relDelete, id)
			class := firstGatedClass
			if class == "" {
				// endpoint-driven: attribute to the dying endpoint's class
				for _, e := range []string{src, tgt} {
					if c, ok := entClass[e]; ok && class == "" {
						class = c
					}
				}
			}
			c := cls(string(class))
			c.Relations++
			continue
		}
		if gatedRefs > 0 {
			relStrip = append(relStrip, id)
			stripCounts[id] = gatedRefs
			cls(string(firstGatedClass)).EvidenceRefsStripped += gatedRefs
		}
	}

	// Totals.
	for _, c := range clsMap {
		rep.Totals = rep.Totals.add(*c)
	}

	if !apply {
		return rep, nil
	}

	// 4. Apply: relations -> survivor evidence strip -> mentions -> entities.
	if len(relDelete) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM processing_entity_relationships r USING processing_snapshots s WHERE r.snapshot_id = s.id AND s.active AND r.id = ANY($1::uuid[])`, relDelete); err != nil {
			return rep, fmt.Errorf("cleanup delete relations: %w", err)
		}
	}
	kgHook("kg_cleanup_frontmatter:after_delete_relations")
	if len(relStrip) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE processing_entity_relationships r
			SET evidence_chunk_ids = COALESCE((
			  SELECT jsonb_agg(v) FROM jsonb_array_elements_text(r.evidence_chunk_ids) v
			  WHERE NOT (v = ANY($1::text[]))
			), '[]'::jsonb)
			FROM processing_snapshots s
			WHERE r.snapshot_id = s.id AND s.active AND r.id = ANY($2::uuid[])`, textSet(gated), relStrip); err != nil {
			return rep, fmt.Errorf("cleanup strip evidence: %w", err)
		}
	}
	// Mentions of SURVIVING entities (deleted entities' mentions cascade).
	if survivorMentions := gatedMentionsOfSurvivors(gatedMentions, entDelete); len(survivorMentions) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM processing_entity_mentions m USING processing_entities e, processing_snapshots s WHERE m.entity_id = e.id AND e.snapshot_id = s.id AND s.active AND m.id = ANY($1::uuid[])`, survivorMentions); err != nil {
			return rep, fmt.Errorf("cleanup delete mentions: %w", err)
		}
	}
	if len(entDelete) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM processing_entities e USING processing_snapshots s WHERE e.snapshot_id = s.id AND s.active AND e.id = ANY($1::uuid[])`, entDelete); err != nil {
			return rep, fmt.Errorf("cleanup delete entities: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, fmt.Errorf("cleanup commit: %w", err)
	}
	return rep, nil
}

// textSet keeps only distinct strings (gated chunk ids may repeat across
// mention rows).
func textSet(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func containsID(set []string, id string) bool {
	for _, s := range set {
		if s == id {
			return true
		}
	}
	return false
}

func gatedMentionsOfSurvivors(gatedMentions []fmMention, entDelete []string) []string {
	out := make([]string, 0, len(gatedMentions))
	for _, m := range gatedMentions {
		if !containsID(entDelete, m.entity) {
			out = append(out, m.id)
		}
	}
	return out
}
