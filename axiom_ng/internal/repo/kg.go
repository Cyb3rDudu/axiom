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
	ID            string   `json:"id"`
	CanonicalForm string   `json:"canonical_form"`
	Text          string   `json:"text"`
	Type          string   `json:"type,omitempty"`
	Mentions      int      `json:"mentions"`        // distinct chunks mentioning it
	Forms         []string `json:"forms,omitempty"` // family forms (W3: survivor + variants)
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
	nq := normalizeKGTerm(q)
	nqNoFam := normalizeKGTermNoFam(q)
	qRaw := strings.ToLower(strings.TrimSpace(q))
	rows, err := r.pool.Query(ctx, `
		SELECT root_entity_id::text, primary_form, primary_text,
		       coalesce(primary_type, ''), mention_count, forms::text
		FROM kg_entity_roots
		WHERE mention_count >= $1
		  AND ($2 = '' OR normalized_form LIKE '%' || $2 || '%'
		       OR ($2 LIKE '%' || normalized_form || '%' AND length(normalized_form) >= 4))
		ORDER BY CASE
		           WHEN $2 = '' THEN 4
		           WHEN $4 = lower(primary_form) THEN 1
		           WHEN normalized_form_nofam = $3 THEN 2
		           WHEN normalized_form = $2 THEN 3
		           ELSE 4
		         END,
		         mention_count DESC, root_entity_id
		LIMIT $5`, minMentions, nq, nqNoFam, qRaw, limit)
	if err != nil {
		return nil, err
	}
	return scanKGEntities(rows)
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
	rootID, err := r.resolveKGRootID(ctx, entityID)
	if err != nil || rootID == "" {
		return []KGNeighbor{}, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT CASE WHEN source_root_id = $1::uuid THEN target_root_id::text ELSE source_root_id::text END AS other_id,
		       CASE WHEN source_root_id = $1::uuid THEN target_form ELSE source_form END AS other_form,
		       coalesce(CASE WHEN source_root_id = $1::uuid THEN target_type ELSE source_type END, '') AS other_type,
		       CASE WHEN source_root_id = $1::uuid THEN 'out' ELSE 'in' END AS direction,
		       type, strength, evidence_chunk_ids::text,
		       CASE WHEN source_root_id = $1::uuid THEN target_mentions ELSE source_mentions END AS other_mentions,
		       confidence
		FROM kg_relation_triples
		WHERE (source_root_id = $1::uuid OR target_root_id = $1::uuid)
		  AND source_mentions >= $2 AND target_mentions >= $2
		ORDER BY other_mentions DESC, other_id
		LIMIT $3`, rootID, minMentions, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KGNeighbor{}
	for rows.Next() {
		var e KGNeighbor
		var ev string
		if err := rows.Scan(&e.OtherID, &e.OtherForm, &e.OtherType, &e.Direction,
			&e.Type, &e.Strength, &ev, &e.OtherMentions, &e.Confidence); err != nil {
			return nil, err
		}
		e.EvidenceChunks = parseUUIDArray(ev)
		out = append(out, e)
	}
	return out, rows.Err()
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
	rootID := ""
	var err error
	if entityID != "" {
		rootID, err = r.resolveKGRootID(ctx, entityID)
		if err != nil || rootID == "" {
			return []KGRelationView{}, err
		}
	}
	rows, err := r.pool.Query(ctx, `
		SELECT t.id::text, t.type,
		       t.source_root_id::text, t.source_form,
		       t.target_root_id::text, t.target_form,
		       t.strength, t.evidence_chunk_ids::text, t.corroborating_documents,
		       t.confidence
		FROM kg_relation_triples t
		WHERE ($1 = '' OR t.type = $1)
		  AND ($2 = '' OR t.source_root_id = $2::uuid OR t.target_root_id = $2::uuid)
		  AND ($5 = '' OR EXISTS (
		      SELECT 1 FROM kg_relation_evidence_docs d
		      WHERE d.triple_id = t.id AND d.document_id = $5::uuid))
		  AND t.source_mentions >= $3 AND t.target_mentions >= $3
		ORDER BY t.corroborating_documents DESC, t.source_mentions + t.target_mentions DESC, t.confidence DESC, t.id
		LIMIT $4`, relType, rootID, minMentions, limit, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KGRelationView{}
	for rows.Next() {
		var x KGRelationView
		var ev string
		if err := rows.Scan(&x.ID, &x.Type, &x.SourceID, &x.SourceForm,
			&x.TargetID, &x.TargetForm, &x.Strength, &ev, &x.Documents, &x.Confidence); err != nil {
			return nil, err
		}
		x.EvidenceChunks = parseUUIDArray(ev)
		x.CorroboratingDocuments = x.Documents
		out = append(out, x)
	}
	return out, rows.Err()
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
		var formsJSON string
		if err := rows.Scan(&e.ID, &e.CanonicalForm, &e.Text, &e.Type, &e.Mentions, &formsJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(formsJSON), &e.Forms)
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
