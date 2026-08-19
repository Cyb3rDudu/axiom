// Package repo — knowledge-graph read layer (R6, #136). The graph is
// read-only, derived from active snapshots (L6): entities, mentions,
// evidenzbelegte relations. The mention-stability filter (>=2 distinct
// chunks per entity, L8-Analyse §6) is a mandatory default everywhere —
// 71% of entities are one-hit noise.
package repo

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
)

// KGEntity is a graph entity with its mention footprint.
type KGEntity struct {
	ID            string `json:"id"`
	CanonicalForm string `json:"canonical_form"`
	Text          string `json:"text"`
	Type          string `json:"type,omitempty"`
	Mentions      int    `json:"mentions"` // distinct chunks mentioning it
}

// activeEntityCounts CTE: mention counts per entity, only active snapshots.
const activeEntityCounts = `
	WITH em AS (
		SELECT e.id AS entity_id, count(DISTINCT m.chunk_id) AS chunks
		FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		JOIN processing_entity_mentions m ON m.entity_id = e.id
		GROUP BY e.id
	)`

// SearchKGEntities prefix/substring-matches canonical forms (falling back
// to raw text) over ACTIVE snapshots, mention-stability filter applied,
// ordered by mentions desc. Empty q returns the top hubs.
func (r *Repo) SearchKGEntities(ctx context.Context, q string, minMentions, limit int) ([]KGEntity, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// #193 P3: a caller-supplied % or _ would otherwise act as a wildcard
	// inside the ILIKE pattern (and \\ un-escapes). Escape with the
	// Postgres default escape char so the term matches literally.
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	q = esc.Replace(q)
	rows, err := r.pool.Query(ctx, activeEntityCounts+`
		SELECT e.id::text, coalesce(e.canonical_form, e.text), e.text,
		       coalesce(e.type, ''), em.chunks
		FROM em
		JOIN processing_entities e ON e.id = em.entity_id
		WHERE em.chunks >= $1
		  AND ($2 = '' OR coalesce(e.canonical_form, e.text) ILIKE '%' || $2 || '%' OR e.text ILIKE '%' || $2 || '%')
		ORDER BY em.chunks DESC, e.id
		LIMIT $3`, minMentions, q, limit)
	if err != nil {
		return nil, err
	}
	return scanKGEntities(rows) // takes ownership of closing
}

// KGNeighbor is one 1-hop edge with both endpoints' stability.
type KGNeighbor struct {
	OtherID        string   `json:"other_id"`
	OtherForm      string   `json:"other_form"`
	OtherType      string   `json:"other_type,omitempty"`
	Direction      string   `json:"direction"` // "out" | "in"
	Type           string   `json:"type"`
	Strength       *float32 `json:"strength,omitempty"`
	EvidenceChunks []string `json:"evidence_chunks,omitempty"`
	OtherMentions  int      `json:"other_mentions"`
}

// KGNeighbors returns 1-hop edges of an entity where BOTH endpoints have
// >= minMentions distinct chunks (the stability filter is the point —
// 71% of entities are one-hit noise, L8: 71.6%).
func (r *Repo) KGNeighbors(ctx context.Context, entityID string, minMentions, limit int) ([]KGNeighbor, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, activeEntityCounts+`
		SELECT o.id::text, coalesce(o.canonical_form, o.text), coalesce(o.type, ''),
		       CASE WHEN r.source_entity_id = $1::uuid THEN 'out' ELSE 'in' END,
		       r.type, r.strength, r.evidence_chunk_ids::text, om.chunks
		FROM processing_entity_relationships r
		JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
		JOIN em src ON src.entity_id = r.source_entity_id
		JOIN em tgt ON tgt.entity_id = r.target_entity_id
		JOIN processing_entities o
		  ON o.id = CASE WHEN r.source_entity_id = $1::uuid THEN r.target_entity_id ELSE r.source_entity_id END
		JOIN em om ON om.entity_id = o.id
		WHERE (r.source_entity_id = $1::uuid OR r.target_entity_id = $1::uuid)
		  AND src.chunks >= $2 AND tgt.chunks >= $2
		ORDER BY om.chunks DESC, o.id
		LIMIT $3`, entityID, minMentions, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KGNeighbor{} // #193 P2: empty slice marshals as [], never null
	for rows.Next() {
		var n KGNeighbor
		var ev string
		var strength *float32
		if err := rows.Scan(&n.OtherID, &n.OtherForm, &n.OtherType, &n.Direction,
			&n.Type, &strength, &ev, &n.OtherMentions); err != nil {
			return nil, err
		}
		n.Strength = strength
		n.EvidenceChunks = parseUUIDArray(ev)
		out = append(out, n)
	}
	return out, rows.Err()
}

// KGRelationView is a relation with resolved endpoint forms.
type KGRelationView struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	SourceID       string   `json:"source_id"`
	SourceForm     string   `json:"source_form"`
	TargetID       string   `json:"target_id"`
	TargetForm     string   `json:"target_form"`
	Strength       *float32 `json:"strength,omitempty"`
	EvidenceChunks []string `json:"evidence_chunks,omitempty"`
	// Documents is the number of distinct library documents corroborating
	// this (source_form, type, target_form) triple across ACTIVE snapshots —
	// the #185 thematic signal: a triple found in several books is thematically
	// real, a single-document triple is one extractor pass (often junk-typed:
	// "nachhaltigkeit owned_by weleda"). 1 = single-document evidence.
	Documents int `json:"documents"`
}

// KGRelations browses relations (optional type, entity and document scope),
// active snapshots, stability filter on both endpoints. Ranking (#185):
// cross-document corroboration of the (source_form, type, target_form)
// triple FIRST — endpoint popularity (the old ranking) surfaced hub-junk
// like 6 weleda relations ahead of every thematic candidate —, popularity
// only as tiebreak. document_id scopes the result to relations with at
// least one evidence chunk in that document's active snapshot; the
// corroboration count stays global (it weights the triple, not the scope).
func (r *Repo) KGRelations(ctx context.Context, relType, entityID, documentID string, minMentions, limit int) ([]KGRelationView, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// ponytail: cor is a group-by over all active relations (~75k rows at
	// current corpus; the CTE itself is cheap, but the full ranked browse
	// measured ~1.5s on the live graph with parallelism off — plan for the
	// WHOLE query, not just the CTE). Materialize as a table if the graph
	// grows or the endpoint becomes latency-sensitive.
	rows, err := r.pool.Query(ctx, activeEntityCounts+`,
	cor AS (
		SELECT r.type, coalesce(se.canonical_form, se.text) AS sf,
		       coalesce(te.canonical_form, te.text) AS tf,
		       count(DISTINCT sn.document_id) AS docs
		FROM processing_entity_relationships r
		JOIN processing_snapshots sn ON sn.id = r.snapshot_id AND sn.active
		JOIN processing_entities se ON se.id = r.source_entity_id
		JOIN processing_entities te ON te.id = r.target_entity_id
		GROUP BY 1, 2, 3
	)
	SELECT r.id::text, r.type,
	       r.source_entity_id::text, coalesce(se.canonical_form, se.text),
	       r.target_entity_id::text, coalesce(te.canonical_form, te.text),
	       r.strength, r.evidence_chunk_ids::text, coalesce(cor.docs, 1)
	FROM processing_entity_relationships r
	JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
	JOIN em src ON src.entity_id = r.source_entity_id
	JOIN em tgt ON tgt.entity_id = r.target_entity_id
	JOIN processing_entities se ON se.id = r.source_entity_id
	JOIN processing_entities te ON te.id = r.target_entity_id
	LEFT JOIN cor ON cor.type = r.type
	             AND cor.sf = coalesce(se.canonical_form, se.text)
	             AND cor.tf = coalesce(te.canonical_form, te.text)
	WHERE ($1 = '' OR r.type = $1)
	  AND ($2 = '' OR r.source_entity_id = $2::uuid OR r.target_entity_id = $2::uuid)
	  AND ($5 = '' OR EXISTS (
		      SELECT 1 FROM processing_chunks ec
		      JOIN processing_snapshots esn ON esn.id = ec.snapshot_id AND esn.active
		      WHERE ec.id IN (SELECT jsonb_array_elements_text(r.evidence_chunk_ids)::uuid)
		        AND esn.document_id = $5::uuid))
	  AND src.chunks >= $3 AND tgt.chunks >= $3
	ORDER BY cor.docs DESC NULLS LAST, src.chunks + tgt.chunks DESC, r.id
	LIMIT $4`, relType, entityID, minMentions, limit, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KGRelationView{} // #193 P2: empty slice marshals as [], never null
	for rows.Next() {
		var v KGRelationView
		var ev string
		var strength *float32
		if err := rows.Scan(&v.ID, &v.Type, &v.SourceID, &v.SourceForm,
			&v.TargetID, &v.TargetForm, &strength, &ev, &v.Documents); err != nil {
			return nil, err
		}
		v.Strength = strength
		v.EvidenceChunks = parseUUIDArray(ev)
		out = append(out, v)
	}
	return out, rows.Err()
}

// KGChunkCandidate is a graph-arm candidate with the fields the search hit
// needs (text/locator/section/document for hydration + rendering).
type KGChunkCandidate struct {
	ChunkID       string
	DocumentID    string
	Text          string
	Locator       map[string]any
	SectionTitles []string
	EntityLinks   int // distinct stable entities shared with the seed chunks
}

// GraphCandidates expands seed chunk ids through their stable entities into
// NEIGHBOR chunks (other chunks mentioning the same stable entities),
// ranked by distinct entity links. Excludes the seeds themselves.
func (r *Repo) GraphCandidates(ctx context.Context, seedChunkIDs []string, minMentions, limit int) ([]KGChunkCandidate, error) {
	if len(seedChunkIDs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	rows, err := r.pool.Query(ctx, activeEntityCounts+`
		SELECT c.id::text, sn.document_id::text, c.text, c.locator,
		       c.section_titles, count(DISTINCT e.id) AS links
		FROM processing_entity_mentions sm
		JOIN em ON em.entity_id = sm.entity_id AND em.chunks >= $2
		JOIN processing_entities e ON e.id = sm.entity_id
		JOIN processing_entity_mentions m ON m.entity_id = e.id
		JOIN processing_chunks c ON c.id = m.chunk_id
		JOIN processing_snapshots sn ON sn.id = c.snapshot_id AND sn.active
		WHERE sm.chunk_id = ANY($1::uuid[])
		  AND NOT (m.chunk_id = ANY($1::uuid[]))
		GROUP BY c.id, sn.document_id, c.text, c.locator, c.section_titles
		ORDER BY links DESC, c.id
		LIMIT $3`, seedChunkIDs, minMentions, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KGChunkCandidate
	for rows.Next() {
		var c KGChunkCandidate
		if err := rows.Scan(&c.ChunkID, &c.DocumentID, &c.Text, &c.Locator, &c.SectionTitles, &c.EntityLinks); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanKGEntities(rows pgx.Rows) ([]KGEntity, error) {
	defer rows.Close()
	out := []KGEntity{} // #193 P2: empty slice marshals as [], never null
	for rows.Next() {
		var e KGEntity
		if err := rows.Scan(&e.ID, &e.CanonicalForm, &e.Text, &e.Type, &e.Mentions); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// parseUUIDArray decodes a JSONB uuid-array text ("[\"a\",\"b\"]");
// "null" and "[]" decode to nil (json.Unmarshal into a slice accepts both).
func parseUUIDArray(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || len(out) == 0 {
		return nil
	}
	return out
}
