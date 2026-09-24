// ladder.go — the metadata verify ladder (F06, #300). Binding order:
//
//	(1) Dokumentinhalt (title page/imprint — the DocumentInspector rung)
//	(2) exakter Identifier-Lookup (DOI vor unscharfer Suche)
//	(3) Crossref-Kandidatensuche (Titel/Autor/Jahr/Typ bewertet)
//	(4) Open Library/ISBN
//	(5) Web-Recherche — NOT wired in F06 (R21); absence is the honest state.
//
// Invariants (the fachliche Kernanforderung of the acquisition contract):
//   - NIEMALS erfundene Felder — empty beats wrong; a rung contributes a
//     field only when it actually knows it.
//   - Verified DOCUMENT fields are LOCKED: weaker rungs never overwrite
//     them. Every rejected contribution lands as an applied=false
//     provenance row (the audit sonde).
//   - Ambiguous candidates (near-equal confidence, multiple hits) or a
//     record-type conflict surface as a DECISION — never a silent
//     first-match takeover.
package library

import (
	"context"
	"fmt"
	"strings"
)

// ladderFieldName maps ResolvedFields onto provenance field names.
var ladderFields = []string{"title", "authors", "year", "publisher", "language", "record_type", "doi", "isbn"}

// LadderOutcome is the resolved state after the ladder ran (or paused).
type LadderOutcome struct {
	// Merged carries the effective field set (strongest applied source
	// per field).
	Merged ResolvedFields
	// Ambiguous: non-empty when the ladder paused for a decision.
	Ambiguous *Decision
	// Provenance: every rung contribution, applied or rejected.
	Provenance []ProvenanceRow
}

// Decision is an open question for the caller (awaiting_confirmation).
type Decision struct {
	DecisionID string
	Subject    string
	Candidates []DecisionCandidate
}

// DecisionCandidate is one offered choice.
type DecisionCandidate struct {
	CandidateID string
	Origin      string
	Summary     string
	// Fields: the candidate's own proposal (merged over on confirm).
	Fields ResolvedFields
}

// runLadder executes rungs 1–4. docFields come from the inspecting step
// (rung 1). hints steer the queries but never override anything.
func runLadder(ctx context.Context, ports Ports, recordType string, docFields ResolvedFields,
	hints struct{ DOI, ISBN, Title string }, enrich struct{ crossref, openLibrary bool }) (LadderOutcome, error) {

	out := LadderOutcome{Merged: docFields}

	// Rung 1 bookkeeping: document fields are applied + locked.
	for _, f := range ladderFields {
		if v, ok := docFieldValue(docFields, f); ok {
			out.Provenance = append(out.Provenance, ProvenanceRow{
				Field: f, Source: "document", Confidence: 1.0, Applied: true, Value: v,
			})
		}
	}

	// Query assembly: identifiers from hints or the document itself.
	qIdent := ResolveQuery{
		DOI:  firstNonEmpty(hints.DOI, docFields.DOI),
		ISBN: firstNonEmpty(hints.ISBN, docFields.ISBN),
	}
	qFuzzy := ResolveQuery{
		Title:      firstNonEmpty(hints.Title, docFields.Title),
		RecordType: recordType,
	}

	for _, res := range ports.Resolvers {
		switch res.Name() {
		case "crossref":
			if !enrich.crossref {
				continue
			}
		case "open_library":
			if !enrich.openLibrary {
				continue
			}
		}

		// Rung (2): exact identifier lookup — DOI vor unscharfer Suche.
		if qIdent.DOI != "" || qIdent.ISBN != "" {
			cands, err := res.Resolve(ctx, qIdent)
			if err != nil {
				return out, err
			}
			if len(cands) == 1 && (qIdent.DOI != "" || qIdent.ISBN != "") {
				// Unambiguous identifier hit: direct resolution — the
				// fuzzy pass for THIS resolver is skipped (a direct
				// identifier hit beats fuzzy search).
				out.merge(cands[0].Fields, "identifier", res, &out.Provenance)
				continue
			}
			if len(cands) > 1 {
				out.Ambiguous = ambiguityDecision("bibliography", cands, "identifier", res)
				return out, nil
			}
		}

		// Rungs (3)/(4): candidate search, scored.
		if qFuzzy.Title == "" {
			continue
		}
		q := qFuzzy
		q.DOI, q.ISBN = qIdent.DOI, qIdent.ISBN
		if y := out.Merged.Year; y != nil {
			q.Year = y
		}
		cands, err := res.Resolve(ctx, q)
		if err != nil {
			return out, err
		}
		if len(cands) == 0 {
			continue
		}
		// Type conflict: the top candidate disagrees with the requested
		// record type → decision (no silent takeover, no silent retyping).
		if t := topCandidate(cands); t.Fields.RecordType != "" && t.Fields.RecordType != recordType {
			out.Ambiguous = ambiguityDecision("bibliography", cands, res.Name(), res)
			return out, nil
		}
		if ambiguous(cands) {
			out.Ambiguous = ambiguityDecision("bibliography", cands, res.Name(), res)
			return out, nil
		}
		out.merge(cands[0].Fields, res.Name(), res, &out.Provenance)
	}
	return out, nil
}

// merge folds an automatic rung contribution into the effective field
// set. Fields already present (document rung or a stronger rung) are
// LOCKED: differing values are logged as applied=false rows (the audit
// sonde), agreeing values add nothing. Empty fields are filled (applied).
func (o *LadderOutcome) merge(fields ResolvedFields, source string, res BibliographicResolver, prov *[]ProvenanceRow) {
	version := ""
	if res != nil {
		version = res.Version()
	}
	for _, f := range ladderFields {
		v, ok := fieldOf(fields, f)
		if !ok {
			continue
		}
		cur, held := anyFieldValue(o.Merged, f)
		if held && differing(cur, v, f) {
			// Locked field: the attempt is documented, the value unchanged.
			*prov = append(*prov, ProvenanceRow{
				Field: f, Source: source, ResolverVersion: version, Confidence: 0.5,
				Applied: false, Value: v,
			})
			continue
		}
		if !held {
			setField(&o.Merged, f, v)
			*prov = append(*prov, ProvenanceRow{
				Field: f, Source: source, ResolverVersion: version, Confidence: 0.9,
				Applied: true, Value: v,
			})
		}
	}
}

// ambiguous reports near-equal multiple hits (no runaway winner).
func ambiguous(cands []Candidate) bool {
	if len(cands) < 2 {
		return false
	}
	return cands[0].Confidence-cands[1].Confidence < 0.05
}

func topCandidate(cands []Candidate) Candidate {
	best := cands[0]
	for _, c := range cands[1:] {
		if c.Confidence > best.Confidence {
			best = c
		}
	}
	return best
}

// ambiguityDecision builds the awaiting_confirmation payload.
func ambiguityDecision(subject string, cands []Candidate, origin string, res BibliographicResolver) *Decision {
	d := &Decision{DecisionID: "dec-bibliography", Subject: subject}
	version := ""
	if res != nil {
		version = res.Version()
	}
	for _, c := range cands {
		d.Candidates = append(d.Candidates, DecisionCandidate{
			CandidateID: c.CandidateID,
			Origin:      origin,
			Summary: fmt.Sprintf("%s (%s%s)", firstNonEmpty(c.Fields.Title, "untitled"),
				strings.Join(c.Fields.Authors, ", "),
				suffixNonEmpty(version, " via "+origin)),
			Fields: c.Fields,
		})
	}
	return d
}

// docFieldValue reports the DOCUMENT-run-verified value of a field (the
// locked set is the inspector's own output — tracked by provenance
// source "document" applied rows).
func docFieldValue(f ResolvedFields, name string) (string, bool) {
	if name != "title" && name != "language" {
		// The document rung vouches for content-derived fields only.
		return "", false
	}
	return fieldOf(f, name)
}

// anyFieldValue reports any present value.
func anyFieldValue(f ResolvedFields, name string) (string, bool) {
	return fieldOf(f, name)
}

func fieldOf(f ResolvedFields, name string) (string, bool) {
	switch name {
	case "title":
		return f.Title, f.Title != ""
	case "authors":
		return strings.Join(f.Authors, "; "), len(f.Authors) > 0
	case "year":
		if f.Year == nil {
			return "", false
		}
		return fmt.Sprintf("%d", *f.Year), true
	case "publisher":
		return f.Publisher, f.Publisher != ""
	case "language":
		return f.Language, f.Language != ""
	case "record_type":
		return f.RecordType, f.RecordType != ""
	case "doi":
		return NormalizeDOI(f.DOI), f.DOI != ""
	case "isbn":
		return NormalizeISBN(f.ISBN), f.ISBN != ""
	}
	return "", false
}

func setField(f *ResolvedFields, name, v string) {
	switch name {
	case "title":
		f.Title = v
	case "authors":
		f.Authors = strings.Split(v, "; ")
		for i := range f.Authors {
			f.Authors[i] = strings.TrimSpace(f.Authors[i])
		}
	case "year":
		var y int
		if _, err := fmt.Sscanf(v, "%d", &y); err == nil {
			yy := y
			f.Year = &yy
		}
	case "publisher":
		f.Publisher = v
	case "language":
		f.Language = v
	case "record_type":
		f.RecordType = v
	case "doi":
		f.DOI = NormalizeDOI(v)
	case "isbn":
		f.ISBN = NormalizeISBN(v)
	}
}

// differing compares two field values (normalized per field kind).
func differing(a, b, field string) bool {
	switch field {
	case "doi":
		return NormalizeDOI(a) != NormalizeDOI(b)
	case "isbn":
		return NormalizeISBN(a) != NormalizeISBN(b)
	case "authors":
		return strings.Join(splitAuthors(a), "|") != strings.Join(splitAuthors(b), "|")
	}
	return a != b
}

func splitAuthors(s string) []string {
	parts := strings.Split(s, "; ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func suffixNonEmpty(s, prefixIfSet string) string {
	if s == "" {
		return ""
	}
	return prefixIfSet + s
}
