package repo

// Entity typing normalization (#198-3): pure SQL rules over EXISTING
// entities of ACTIVE snapshots — no re-extraction, no fuzzy matching.
// The two rule classes from the external regression:
//
//   bare_form    — exact normalized form (lower-cased, whitespace
//                  collapsed) is a generic role noun/plural or a generic
//                  management term: stakeholders, primäre stakeholder,
//                  management, top-management, ... These describe ROLE
//                  GROUPS, never a specific PERSON or ORGANIZATION.
//                  Any type other than CONCEPT (incl. NULL) is corrected.
//
//   plural_head  — 1–3 word form ENDING in a role-plural head noun
//                  (stakeholders/shareholders/...) currently mistyped as
//                  PERSON or ORGANIZATION: 'Externe Stakeholder',
//                  'Industry Stakeholders'. Proper-noun tails are safe:
//                  'Academy of Management' ends in a singular head and is
//                  not in the bare list.
//
// Idempotent by construction: matched rows become CONCEPT and no longer
// match (both rules require a non-CONCEPT type). Counts are reportable
// WITHOUT applying (blast-radius-first).

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// typingBareForms: exact normalized forms (lower-case, single spaces).
var typingBareForms = []string{
	// generic role nouns and plurals (DE + EN)
	"stakeholder", "stakeholders", "stakeholdern",
	"primäre stakeholder", "sekundäre stakeholder",
	"primären stakeholdern", "sekundären stakeholdern",
	"key stakeholders", "interne stakeholder", "externe stakeholder",
	"internen stakeholdern", "externen stakeholdern",
	"shareholder", "shareholders", "shareholdern",
	"anteilseigner", "anteilseignern", "anspruchsgruppe", "anspruchsgruppen",
	// generic management terms (role groups, never PERSON/ORGANIZATION)
	"management", "das management", "managements",
	"top-management", "top management", "topmanagement",
	"top managements", "top-managements", "topmanagements",
	"mittleres management", "middle management", "mittleres managements",
	"oberes management", "upper management", "lower management",
	"managementebene", "managementebenen",
	// audited generic role/group nouns (#199 Chattys final finding):
	// mitarbeiter 27 entities, 2 with edges (791/171 mentions) — the last
	// real name collision in the graph; the homonym guard keeps single-word
	// PERSONs separate, so these need CONCEPT typing to merge.
	"mitarbeiter", "mitarbeitern", "mitarbeiterinnen",
	"kunden", "kunde",
	"vorstand",
	"führungskräfte", "führungskräften", "führungskraft",
	"unternehmer", "unternehmern",
	"geschäftsführer", "geschäftsführerin",
	"beschäftigte", "arbeitnehmer", "arbeitnehmerin",
	"arbeitgeber", "arbeitgebern",
	// Genitive/casus forms of the audited stems (korpus-vermessen):
	// mitarbeiters 9×PERSON, arbeitgebers 7×, vorstands 7×, arbeitnehmers 5×,
	// unternehmers 5×, geschäftsführers 2× — the Genitiv gap blocked the
	// mitarbeiter family (PERSON-majority via 8 genitive stragglers).
	"mitarbeiters", "arbeitgebers", "vorstands", "arbeitnehmers",
	"unternehmers", "geschäftsführers",
	// deliberately NOT here: manager (tester accepted PERSON class),
	// sie/pronouns (extraction noise, future filter — not a type fix)
	// NOT here (korpus-abwesend): kundes, beschäftigters
}

// typingPluralHeads: head nouns that make a short form a generic role
// group. Kept closed and plural-only — singular heads (management,
// stakeholder) are handled by the bare list where the whole form must
// match exactly.
var typingPluralHeads = []string{
	// plural + dative plural (-n) + genitive plural (-s)
	"stakeholders", "stakeholdern",
	"shareholders", "shareholdern",
	"anteilseignern", "anteilseigners",
	"anspruchsgruppen",
	"managements", // plural of management (genitive/dative)
	"mitarbeitern", "mitarbeiterinnen",
	"kunden",
	"führungskräften",
	"arbeitgebern", "arbeitnehmerinnen",
}

// EntityTypingReport is the accounting of one normalization run (the
// dry-run variant carries the same numbers without mutating).
type EntityTypingReport struct {
	MatchedRows int            `json:"matched_rows"` // dry alias of UpdatedRows
	UpdatedRows int            `json:"updated_rows"`
	ByRule      map[string]int `json:"by_rule"`
}

const typingMatchSQL = `
	WITH act AS (
		SELECT e.id, e.type,
		       lower(regexp_replace(btrim(coalesce(e.canonical_form, e.text)), '\s+', ' ', 'g')) AS form
		FROM processing_entities e
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
	),
	m AS (
		SELECT id, coalesce(type, '(null)') AS type,
		       CASE WHEN form = ANY($1::text[]) AND type IS DISTINCT FROM 'CONCEPT' THEN 'bare_form'
		            WHEN form ~ $2 AND type IN ('PERSON', 'ORGANIZATION') THEN 'plural_head'
		       END AS rule
		FROM act
	)
	SELECT rule, type, count(*) FROM m WHERE rule IS NOT NULL GROUP BY 1, 2`

func typingPluralHeadPattern() string {
	// ^[a-zäöüß][a-zäöüß-]*( [a-zäöüß][a-zäöüß-]*){0,2} (head1|head2|...)$
	return fmt.Sprintf(`^[a-zäöüß][a-zäöüß-]*( [a-zäöüß][a-zäöüß-]*){0,2} (%s)$`,
		strings.Join(typingPluralHeads, "|"))
}

// EntityTypingCounts reports the blast radius without mutating anything.
func (r *Repo) EntityTypingCounts(ctx context.Context) (EntityTypingReport, error) {
	return r.entityTyping(ctx, false)
}

// NormalizeEntityTypes applies the rules and returns the accounting.
func (r *Repo) NormalizeEntityTypes(ctx context.Context) (EntityTypingReport, error) {
	return r.entityTyping(ctx, true)
}

func (r *Repo) entityTyping(ctx context.Context, apply bool) (EntityTypingReport, error) {
	rep := EntityTypingReport{ByRule: map[string]int{}}
	rows, err := r.pool.Query(ctx, typingMatchSQL, typingBareForms, typingPluralHeadPattern())
	if err != nil {
		return rep, fmt.Errorf("typing match: %w", err)
	}
	defer rows.Close()
	byRuleType := map[[2]string]int{}
	total := 0
	for rows.Next() {
		var rule, typ string
		var n int
		if err := rows.Scan(&rule, &typ, &n); err != nil {
			return rep, fmt.Errorf("typing scan: %w", err)
		}
		byRuleType[[2]string{rule, typ}] = n
		total += n
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}
	for k, n := range byRuleType {
		rep.ByRule[k[0]] += n
		rep.ByRule[k[0]+"/"+k[1]] = n
	}
	if !apply {
		rep.MatchedRows = total
		return rep, nil
	}
	err = r.withKGMaintenanceTx(ctx, "kg_normalize_entity_types", func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			WITH act AS (
				SELECT e.id, e.type,
				       lower(regexp_replace(btrim(coalesce(e.canonical_form, e.text)), '\s+', ' ', 'g')) AS form
				FROM processing_entities e
				JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
			)
			UPDATE processing_entities e
			SET type = 'CONCEPT'
			FROM act
			WHERE e.id = act.id
			  AND act.type IS DISTINCT FROM 'CONCEPT'
			  AND (act.form = ANY($1::text[])
			       OR (act.form ~ $2 AND act.type IN ('PERSON', 'ORGANIZATION')))`,
			typingBareForms, typingPluralHeadPattern())
		if err != nil {
			return fmt.Errorf("typing apply: %w", err)
		}
		rep.UpdatedRows = int(ct.RowsAffected())
		kgHook("kg_normalize_entity_types:after_update")
		return r.refreshKGReadModelTx(ctx, tx)
	})
	if err != nil {
		return rep, err
	}
	rep.MatchedRows = rep.UpdatedRows
	return rep, nil
}
