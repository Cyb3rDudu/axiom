package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/frontmatter"
	"github.com/jackc/pgx/v5"
)

type kgRootBuild struct {
	id          string
	primaryForm string
	primaryText string
	primaryType string
	forms       map[string]bool
	typeVotes   map[string]int
	mentions    int
	members     int
}

type kgRelBuild struct {
	src, tgt                         string
	typ                              string
	rows                             int
	evidence                         map[string]bool
	strength                         *float32
	forwardRows, reverseRows         int
	forwardEvidence, reverseEvidence map[string]bool
}

type kgRawRel struct {
	srcRoot, tgtRoot string
	typ              string
	strength         *float32
	evidence         []string
}

// RefreshKGReadModel rebuilds the materialized KG API read model from active
// raw graph rows under the W1 KG maintenance lock. The raw tables remain the
// source of truth; these rows are a deterministic projection.
func (r *Repo) RefreshKGReadModel(ctx context.Context) error {
	return r.withKGMaintenanceTx(ctx, "kg_refresh_read_model", func(tx pgx.Tx) error {
		return r.refreshKGReadModelTx(ctx, tx)
	})
}

func (r *Repo) refreshKGReadModelTx(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `DELETE FROM kg_relation_evidence_docs`); err != nil {
		return fmt.Errorf("refresh kg evidence docs clear: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM kg_relation_triples`); err != nil {
		return fmt.Errorf("refresh kg triples clear: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM kg_entity_roots`); err != nil {
		return fmt.Errorf("refresh kg roots clear: %w", err)
	}

	roots, err := loadKGRoots(ctx, tx)
	if err != nil {
		return err
	}
	for _, root := range roots {
		forms := sortedKeys(root.forms)
		formsJSON, _ := json.Marshal(forms)
		votesJSON, _ := json.Marshal(root.typeVotes)
		primaryType := any(nil)
		if root.primaryType != "" {
			primaryType = root.primaryType
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO kg_entity_roots
			  (root_entity_id, primary_form, primary_text, primary_type, forms,
			   type_votes, mention_count, member_count, normalized_form,
			   normalized_form_nofam)
			VALUES ($1::uuid,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10)`,
			root.id, root.primaryForm, root.primaryText, primaryType, string(formsJSON), string(votesJSON),
			root.mentions, root.members, normalizeKGTerm(root.primaryForm), normalizeKGTermNoFam(root.primaryForm)); err != nil {
			return fmt.Errorf("refresh kg root %s: %w", root.id, err)
		}
	}

	rels, err := loadKGRawRelations(ctx, tx)
	if err != nil {
		return err
	}
	chunkDocs, chunkBody, err := loadKGEvidenceFacts(ctx, tx, rels)
	if err != nil {
		return err
	}
	groups := groupKGRawRelations(rels)
	for _, g := range groups {
		if g.src == g.tgt { // intra-family edge safety boundary
			continue
		}
		rootSrc, rootTgt := roots[g.src], roots[g.tgt]
		if rootSrc == nil || rootTgt == nil {
			continue
		}
		evidence := sortedKeys(g.evidence)
		docCounts := map[string]int{}
		body := 0
		for _, c := range evidence {
			if docs := chunkDocs[c]; len(docs) > 0 {
				for d := range docs {
					docCounts[d]++
				}
			}
			if chunkBody[c] {
				body++
			}
		}
		sec := 1.0
		if len(evidence) > 0 {
			sec = float64(body) / float64(len(evidence))
		}
		docs := len(docCounts)
		if docs < 1 {
			docs = 1
		}
		evJSON, _ := json.Marshal(evidence)
		var tripleID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO kg_relation_triples
			  (source_root_id, target_root_id, type, source_form, target_form,
			   source_type, target_type, source_mentions, target_mentions, strength,
			   evidence_chunk_ids, evidence_count, triple_row_count,
			   corroborating_documents, section_quality, confidence)
			VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13,$14,$15,$16)
			RETURNING id::text`,
			g.src, g.tgt, g.typ, rootSrc.primaryForm, rootTgt.primaryForm,
			nullString(rootSrc.primaryType), nullString(rootTgt.primaryType), rootSrc.mentions, rootTgt.mentions, g.strength,
			string(evJSON), len(evidence), g.rows, docs, sec, kgConfidence(docs, 1, len(evidence), sec)).Scan(&tripleID); err != nil {
			return fmt.Errorf("refresh kg triple %s-%s-%s: %w", g.src, g.tgt, g.typ, err)
		}
		for doc, n := range docCounts {
			if _, err := tx.Exec(ctx, `
				INSERT INTO kg_relation_evidence_docs (triple_id, document_id, evidence_count)
				VALUES ($1::uuid,$2::uuid,$3)`, tripleID, doc, n); err != nil {
				return fmt.Errorf("refresh kg triple doc %s/%s: %w", tripleID, doc, err)
			}
		}
	}
	return nil
}

func loadKGRoots(ctx context.Context, tx pgx.Tx) (map[string]*kgRootBuild, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.id::text, coalesce(e.alias_of, e.id)::text,
		       coalesce(e.canonical_form, e.text), e.text, coalesce(e.type,''),
		       count(DISTINCT m.chunk_id) AS mentions
		FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		LEFT JOIN processing_entity_mentions m ON m.entity_id = e.id
		GROUP BY e.id, 2, 3, 4, 5
		ORDER BY 2, mentions DESC, e.id`)
	if err != nil {
		return nil, fmt.Errorf("refresh kg roots load: %w", err)
	}
	defer rows.Close()
	roots := map[string]*kgRootBuild{}
	for rows.Next() {
		var id, rootID, form, text, typ string
		var mentions int
		if err := rows.Scan(&id, &rootID, &form, &text, &typ, &mentions); err != nil {
			return nil, err
		}
		root := roots[rootID]
		if root == nil {
			root = &kgRootBuild{id: rootID, primaryForm: form, primaryText: text, primaryType: typ, forms: map[string]bool{}, typeVotes: map[string]int{}}
			roots[rootID] = root
		}
		if id == rootID {
			root.primaryForm, root.primaryText, root.primaryType = form, text, typ
		}
		root.forms[form] = true
		root.mentions += mentions
		root.members++
		if typ != "" {
			root.typeVotes[typ] += mentions
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, root := range roots {
		if root.primaryType == "" && len(root.typeVotes) > 0 {
			root.primaryType = winningType(root.typeVotes)
		}
	}
	return roots, nil
}

func loadKGRawRelations(ctx context.Context, tx pgx.Tx) ([]kgRawRel, error) {
	rows, err := tx.Query(ctx, `
		SELECT coalesce(se.alias_of, se.id)::text,
		       coalesce(te.alias_of, te.id)::text,
		       r.type, r.strength, r.evidence_chunk_ids::text
		FROM processing_entity_relationships r
		JOIN processing_snapshots s ON s.id = r.snapshot_id AND s.active
		JOIN processing_entities se ON se.id = r.source_entity_id
		JOIN processing_entities te ON te.id = r.target_entity_id`)
	if err != nil {
		return nil, fmt.Errorf("refresh kg relations load: %w", err)
	}
	defer rows.Close()
	out := []kgRawRel{}
	for rows.Next() {
		var x kgRawRel
		var ev string
		if err := rows.Scan(&x.srcRoot, &x.tgtRoot, &x.typ, &x.strength, &ev); err != nil {
			return nil, err
		}
		x.evidence = parseUUIDArray(ev)
		out = append(out, x)
	}
	return out, rows.Err()
}

func loadKGEvidenceFacts(ctx context.Context, tx pgx.Tx, rels []kgRawRel) (map[string]map[string]bool, map[string]bool, error) {
	idsSet := map[string]bool{}
	for _, r := range rels {
		for _, c := range r.evidence {
			idsSet[c] = true
		}
	}
	ids := sortedKeys(idsSet)
	docs := map[string]map[string]bool{}
	body := map[string]bool{}
	if len(ids) == 0 {
		return docs, body, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT c.id::text, s.document_id::text, c.text
		FROM processing_chunks c
		JOIN processing_snapshots s ON s.id = c.snapshot_id AND s.active
		WHERE c.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("refresh kg evidence facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, doc, text string
		if err := rows.Scan(&id, &doc, &text); err != nil {
			return nil, nil, err
		}
		if docs[id] == nil {
			docs[id] = map[string]bool{}
		}
		docs[id][doc] = true
		body[id] = frontmatter.Classify(text) == frontmatter.ClassNone
	}
	return docs, body, rows.Err()
}

func groupKGRawRelations(rels []kgRawRel) []*kgRelBuild {
	groups := map[string]*kgRelBuild{}
	for _, r := range rels {
		lo, hi := r.srcRoot, r.tgtRoot
		if hi < lo {
			lo, hi = hi, lo
		}
		key := lo + "\x00" + hi + "\x00" + r.typ
		g := groups[key]
		if g == nil {
			g = &kgRelBuild{src: lo, tgt: hi, typ: r.typ, evidence: map[string]bool{}, forwardEvidence: map[string]bool{}, reverseEvidence: map[string]bool{}}
			groups[key] = g
		}
		g.rows++
		if r.strength != nil && (g.strength == nil || *r.strength > *g.strength) {
			s := *r.strength
			g.strength = &s
		}
		forward := r.srcRoot == lo && r.tgtRoot == hi
		if forward {
			g.forwardRows++
		} else {
			g.reverseRows++
		}
		for _, c := range r.evidence {
			g.evidence[c] = true
			if forward {
				g.forwardEvidence[c] = true
			} else {
				g.reverseEvidence[c] = true
			}
		}
	}
	out := make([]*kgRelBuild, 0, len(groups))
	for _, g := range groups {
		if g.reverseRows > g.forwardRows || (g.reverseRows == g.forwardRows && len(g.reverseEvidence) > len(g.forwardEvidence)) {
			g.src, g.tgt = g.tgt, g.src
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].src != out[j].src {
			return out[i].src < out[j].src
		}
		if out[i].tgt != out[j].tgt {
			return out[i].tgt < out[j].tgt
		}
		return out[i].typ < out[j].typ
	})
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func winningType(votes map[string]int) string {
	types := make([]string, 0, len(votes))
	for t := range votes {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		if votes[types[i]] != votes[types[j]] {
			return votes[types[i]] > votes[types[j]]
		}
		return types[i] < types[j]
	})
	return types[0]
}

func (r *Repo) resolveKGRootID(ctx context.Context, entityID string) (string, error) {
	var root string
	err := r.pool.QueryRow(ctx, `
		SELECT root_entity_id::text FROM kg_entity_roots WHERE root_entity_id=$1::uuid
		UNION ALL
		SELECT coalesce(e.alias_of, e.id)::text
		FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		WHERE e.id=$1::uuid
		LIMIT 1`, entityID).Scan(&root)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return root, err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
