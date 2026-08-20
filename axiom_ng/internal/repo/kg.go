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

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/frontmatter"
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

// normalizeKGTerm is the Go half of the #198 item-5 German query
// normalization. The SQL half (kgNormalizedForm, below) applies the SAME
// spec to stored canonical forms — pinned equivalent by the IT. Steps, in
// order: lowercase; ß->ss; strip everything outside [a-z0-9äöü] (hyphens,
// spaces, punctuation — and LIKE wildcards %/_, so the normalized term
// needs no escaping); bilingual families theory->theorie and
// sustainability->nachhaltigkeit; light plural stem (strip ONE trailing
// suffix from en/er/e/s when the form has >= 6 chars).
func normalizeKGSteps(s string, families bool) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "ß", "ss")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == 'ä' || r == 'ö' || r == 'ü' {
			b.WriteRune(r)
		}
	}
	s = b.String()
	if families {
		s = strings.ReplaceAll(s, "theory", "theorie")
		s = strings.ReplaceAll(s, "sustainability", "nachhaltigkeit")
	}
	if len(s) >= 6 {
		for _, suf := range []string{"en", "er", "e", "s"} {
			if strings.HasSuffix(s, suf) {
				s = s[:len(s)-len(suf)]
				break
			}
		}
	}
	return s
}

func normalizeKGTerm(s string) string      { return normalizeKGSteps(s, true) }
func normalizeKGTermNoFam(s string) string { return normalizeKGSteps(s, false) }

// kgNormalizedForm is the SQL half of the normalization spec — a lateral
// chain over a stored canonical form, spliced into the FROM clause AFTER
// the processing_entities alias e. nf keeps the full spec (strip → family
// → stem); nf_nofam is the same chain WITHOUT the bilingual families (the
// tier-2 basis — a form that only matches THROUGH a family is tier 3).
// Must stay byte-equivalent to normalizeKGTerm / normalizeKGTermNoFam.
const kgNormalizedForm = `JOIN processing_entities e ON e.id = em.entity_id
	CROSS JOIN LATERAL (SELECT replace(lower(coalesce(e.canonical_form, e.text)), 'ß', 'ss') AS s0) l0
	CROSS JOIN LATERAL (SELECT regexp_replace(l0.s0, '[^a-z0-9äöü]', '', 'g') AS s1) l1
	CROSS JOIN LATERAL (SELECT CASE WHEN length(l1.s1) >= 6 THEN regexp_replace(l1.s1, '(en|er|e|s)$', '') ELSE l1.s1 END AS nf_nofam) l2a
	CROSS JOIN LATERAL (SELECT replace(replace(l1.s1, 'theory', 'theorie'), 'sustainability', 'nachhaltigkeit') AS s2) l2
	CROSS JOIN LATERAL (SELECT CASE WHEN length(l2.s2) >= 6 THEN regexp_replace(l2.s2, '(en|er|e|s)$', '') ELSE l2.s2 END AS nf) l3`

// SearchKGEntities matches canonical forms (falling back to raw text) over
// ACTIVE snapshots, mention-stability filter applied. Empty q returns the
// top hubs. Since #198 item 5 the match is NORMALIZED German/bilingual:
// lower, hyphen/space-stripped, plural-stemmed, theory<->theorie family —
// plus reverse containment (stored form inside the query term), which
// de-compounds "doppelte Wesentlichkeit" down to "wesentlichkeit".
// Ranking is TIERED (#198 rider): (1) exact form, (2) normalized-
// equivalent (flexion / compound-full), (3) bilingual synonym family,
// (4) substring/decomposition fragments — mentions DESC only within a
// tier, so a 220-mention fragment never precedes a 2-mention exact
// plural match.
func (r *Repo) SearchKGEntities(ctx context.Context, q string, minMentions, limit int) ([]KGEntity, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// #193 P3 escape is unnecessary now: the normalized term keeps only
	// [a-z0-9äöü] — % and _ cannot survive normalization.
	nq := normalizeKGTerm(q)
	nqNoFam := normalizeKGTermNoFam(q)
	qRaw := strings.ToLower(strings.TrimSpace(q))
	rows, err := r.pool.Query(ctx, activeEntityCounts+`
		SELECT e.id::text, coalesce(e.canonical_form, e.text), e.text,
		       coalesce(e.type, ''), em.chunks
		FROM em
		`+kgNormalizedForm+`
		WHERE em.chunks >= $1
		  AND ($2 = '' OR l3.nf LIKE '%' || $2 || '%'
		       OR ($2 LIKE '%' || l3.nf || '%' AND length(l3.nf) >= 4))
		ORDER BY CASE
		           WHEN $2 = '' THEN 4
		           WHEN $4 = lower(coalesce(e.canonical_form, e.text)) THEN 1 -- exact form
		           WHEN l2a.nf_nofam = $3 THEN 2 -- normalized-equivalent (flexion, compound-full)
		           WHEN l3.nf = $2 THEN 3        -- bilingual synonym family
		           ELSE 4                        -- substring / decomposition fragments
		         END,
		         em.chunks DESC, e.id
		LIMIT $5`, minMentions, nq, nqNoFam, qRaw, limit)
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
	Confidence     float32  `json:"confidence"` // #198 item 4 (computed read-side)
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
	rows, err := r.pool.Query(ctx, activeEntityCounts+`,
	cor AS (
		SELECT r.type, coalesce(se.canonical_form, se.text) AS sf,
		       coalesce(te.canonical_form, te.text) AS tf,
		       count(DISTINCT es.document_id) AS docs,
		       count(DISTINCT r.id) AS triprows
		FROM processing_entity_relationships r
		JOIN processing_snapshots sn ON sn.id = r.snapshot_id AND sn.active
		JOIN processing_entities se ON se.id = r.source_entity_id
		JOIN processing_entities te ON te.id = r.target_entity_id
		CROSS JOIN LATERAL jsonb_array_elements_text(r.evidence_chunk_ids) ev
		JOIN processing_chunks c ON c.id = ev.value::uuid
		JOIN processing_snapshots es ON es.id = c.snapshot_id AND es.active
		GROUP BY 1, 2, 3
	)
	SELECT o.id::text, coalesce(o.canonical_form, o.text), coalesce(o.type, ''),
	       CASE WHEN r.source_entity_id = $1::uuid THEN 'out' ELSE 'in' END,
	       r.type, r.strength, r.evidence_chunk_ids::text, om.chunks,
	       coalesce(cor.docs, 1), coalesce(cor.triprows, 1), jsonb_array_length(r.evidence_chunk_ids)
	FROM processing_entity_relationships r
	JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
	JOIN em src ON src.entity_id = r.source_entity_id
	JOIN em tgt ON tgt.entity_id = r.target_entity_id
	JOIN processing_entities o
	  ON o.id = CASE WHEN r.source_entity_id = $1::uuid THEN r.target_entity_id ELSE r.source_entity_id END
	JOIN em om ON om.entity_id = o.id
	JOIN processing_entities se2 ON se2.id = r.source_entity_id
	JOIN processing_entities te2 ON te2.id = r.target_entity_id
	LEFT JOIN cor ON cor.type = r.type
	             AND cor.sf = coalesce(se2.canonical_form, se2.text)
	             AND cor.tf = coalesce(te2.canonical_form, te2.text)
	WHERE (r.source_entity_id = $1::uuid OR r.target_entity_id = $1::uuid)
	  AND src.chunks >= $2 AND tgt.chunks >= $2
	ORDER BY om.chunks DESC, o.id
	LIMIT $3`, entityID, minMentions, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type neighborRaw struct {
		n     KGNeighbor
		docs  int
		rows  int
		evLen int
	}
	var edges []neighborRaw
	for rows.Next() {
		var e neighborRaw
		var ev string
		var strength *float32
		if err := rows.Scan(&e.n.OtherID, &e.n.OtherForm, &e.n.OtherType, &e.n.Direction,
			&e.n.Type, &strength, &ev, &e.n.OtherMentions, &e.docs, &e.rows, &e.evLen); err != nil {
			return nil, err
		}
		e.n.Strength = strength
		e.n.EvidenceChunks = parseUUIDArray(ev)
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// #198 item 4: section quality of the PAGE's evidence (one batched
	// chunk-text fetch; frontmatter classes are corpus-absent since item 1,
	// the term defends against leakage).
	var evs [][]string
	for _, e := range edges {
		evs = append(evs, e.n.EvidenceChunks)
	}
	sec, err := r.kgEvidenceSectionQuality(ctx, evs)
	if err != nil {
		return nil, err
	}
	out := []KGNeighbor{} // #193 P2: empty slice marshals as [], never null
	for _, e := range edges {
		e.n.Confidence = kgConfidence(e.docs, e.rows, e.evLen, sec[evKey(e.n.EvidenceChunks)])
		out = append(out, e.n)
	}
	return out, nil
}

// KGRelationView is a relation with resolved endpoint forms.
type KGRelationView struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	SourceID   string   `json:"source_id"`
	SourceForm string   `json:"source_form"`
	TargetID   string   `json:"target_id"`
	TargetForm string   `json:"target_form"`
	Strength   *float32 `json:"strength,omitempty"` // persisted extractor strength (uniform 0.7 today)
	// Confidence is the #198 item-4 computed read-side quality of the edge:
	// 0.6 * document support + 0.3 * repetition + 0.1 * evidence section
	// quality, each term in [0,1) — see kgConfidence. Persisted strength is
	// untouched; consumers migrate at their pace.
	Confidence     float32  `json:"confidence"`
	EvidenceChunks []string `json:"evidence_chunks,omitempty"`
	// Documents is the number of distinct library documents corroborating
	// this (source_form, type, target_form) triple across ACTIVE snapshots —
	// the #185 thematic signal: a triple found in several books is thematically
	// real, a single-document triple is one extractor pass (often junk-typed:
	// "nachhaltigkeit owned_by weleda"). 1 = single-document evidence.
	// DEPRECATED NAME (#198 item 6): corroborating_documents is the same
	// value under its honest name; kept so consumers migrate safely.
	Documents int `json:"documents"`
	// CorroboratingDocuments mirrors Documents under its honest name
	// (#198 item 6): the count of distinct documents corroborating the
	// triple — NOT the number of evidence chunks and NOT per-edge rows.
	CorroboratingDocuments int `json:"corroborating_documents"`
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
		       count(DISTINCT es.document_id) AS docs,
		       count(DISTINCT r.id) AS triprows
		FROM processing_entity_relationships r
		JOIN processing_snapshots sn ON sn.id = r.snapshot_id AND sn.active
		JOIN processing_entities se ON se.id = r.source_entity_id
		JOIN processing_entities te ON te.id = r.target_entity_id
		CROSS JOIN LATERAL jsonb_array_elements_text(r.evidence_chunk_ids) ev
		JOIN processing_chunks c ON c.id = ev.value::uuid
		JOIN processing_snapshots es ON es.id = c.snapshot_id AND es.active
		GROUP BY 1, 2, 3
	)
	SELECT r.id::text, r.type,
	       r.source_entity_id::text, coalesce(se.canonical_form, se.text),
	       r.target_entity_id::text, coalesce(te.canonical_form, te.text),
	       r.strength, r.evidence_chunk_ids::text, coalesce(cor.docs, 1),
	       coalesce(cor.triprows, 1), jsonb_array_length(r.evidence_chunk_ids)
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
	type relRaw struct {
		v     KGRelationView
		docs  int
		rows  int
		evLen int
	}
	var rels []relRaw
	for rows.Next() {
		var x relRaw
		var ev string
		var strength *float32
		if err := rows.Scan(&x.v.ID, &x.v.Type, &x.v.SourceID, &x.v.SourceForm,
			&x.v.TargetID, &x.v.TargetForm, &strength, &ev, &x.docs, &x.rows, &x.evLen); err != nil {
			return nil, err
		}
		x.v.Strength = strength
		x.v.EvidenceChunks = parseUUIDArray(ev)
		x.v.Documents = x.docs
		x.v.CorroboratingDocuments = x.docs
		rels = append(rels, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// #198 item 4: section quality of the page's evidence (batched).
	var evs [][]string
	for _, x := range rels {
		evs = append(evs, x.v.EvidenceChunks)
	}
	sec, err := r.kgEvidenceSectionQuality(ctx, evs)
	if err != nil {
		return nil, err
	}
	for _, x := range rels {
		x.v.Confidence = kgConfidence(x.docs, x.rows, x.evLen, sec[evKey(x.v.EvidenceChunks)])
		out = append(out, x.v)
	}
	return out, nil
}

// kgConfidence is the #198 item-4 read-side quality formula:
//
//	confidence = 0.6 * (1 - 1/(1+docs))    document support (corroboration)
//	           + 0.3 * (1 - 1/rep)        repetition (evidence chunks + triple rows)
//	           + 0.1 * sec                evidence section quality
//
// Every term is monotone and bounded; a single-doc single-evidence
// all-body edge scores 0.4, a 5-doc edge approaches 1. sec is the fraction
// of evidence chunks that are NOT frontmatter-class (1.0 on the item-1-
// cleaned corpus; the frontmatter classes are gated at persist). The
// typing-consistency term lands when #198 item 3 (implementor-69865)
// ships — the weights already reserve room for it (rebalance then).
func kgConfidence(docs, tripRows, evidenceLen int, sec float64) float32 {
	if docs < 1 {
		docs = 1
	}
	if sec < 0 {
		sec = 0
	} else if sec > 1 {
		sec = 1
	}
	rep := evidenceLen + tripRows - 1
	if rep < 1 {
		rep = 1
	}
	c := 0.6*(1-1/float64(1+docs)) + 0.3*(1-1/float64(rep)) + 0.1*sec
	return float32(c)
}

// evKey canonicalizes an evidence-id slice into a comparable map key.
func evKey(ev []string) string { return strings.Join(ev, ",") }

// kgEvidenceSectionQuality classifies the evidence chunks of the returned
// page (one batched text fetch) and reports, per evidence-id-slice, the
// fraction of chunks that are body (non-frontmatter). Empty evidence
// arrays are neutral (1.0) — sequential-type relations carry none.
func (r *Repo) kgEvidenceSectionQuality(ctx context.Context, evidence [][]string) (map[string]float64, error) {
	out := make(map[string]float64, len(evidence))
	ids := make([]string, 0)
	for _, ev := range evidence {
		if len(ev) == 0 {
			out[evKey(ev)] = 1
			continue
		}
		ids = append(ids, ev...)
	}
	if len(ids) == 0 {
		return out, nil
	}
	text := map[string]string{}
	rows, err := r.pool.Query(ctx,
		`SELECT id::text, text FROM processing_chunks WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			rows.Close()
			return nil, err
		}
		text[id] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, ev := range evidence {
		if len(ev) == 0 {
			continue
		}
		body := 0
		for _, id := range ev {
			if frontmatter.Classify(text[id]) == frontmatter.ClassNone {
				body++
			}
		}
		out[evKey(ev)] = float64(body) / float64(len(ev))
	}
	return out, nil
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
