package main

// --- v2 proposal mode (#155) -----------------------------------------------
//
// For every confirmed v1 query: scope = dudu's confirmed gold books
// (resolved to document ids), then scoped retrieval with the production
// config (hybrid+rerank) proposes gold-passage candidates from the actual
// chunks. Output: a proposals JSON (machine) + a compact yes/no list
// (human, printed to stdout) — dudu nods through, no book reading.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

type proposal struct {
	QueryID    string    `json:"query_id"`
	Q          string    `json:"q"`
	ScopeBooks []string  `json:"scope_books"`
	ScopeIDs   []string  `json:"scope_document_ids"`
	Best       propHit   `json:"best"`
	Alt        []propHit `json:"alternatives"`
	// Decision = dudus Durchnick-Eintrag: "" (pending), "yes",
	// "alt:N" (N-te Alternative, 1-basiert), "corr:<Hinweis>" (eigene
	// Stelle, z.B. Kapitel/Seite). The v2 materializer consumes it;
	// additive — the committed proposals JSON (all pending) stays valid.
	Decision string `json:"decision,omitempty"`
}

type propHit struct {
	ChunkID string `json:"chunk_id"`
	Book    string `json:"book"`
	Locator string `json:"locator"`
	Excerpt string `json:"excerpt"`
}

func runPropose(ctx context.Context, suite goldSuite, suitePath string) {
	svc, database := openStack(ctx, log.New(os.Stderr, "propose: ", 0))
	defer database.Close()

	titleToID, err := documentIDByTitle(ctx, database)
	if err != nil {
		fatal("title map: %v", err)
	}

	// Warm models once (hybrid+rerank is the proposal config).
	if _, err := svc.Search(ctx, search.Request{Query: "Nachhaltigkeit", TopN: 3}); err != nil {
		log.Printf("warmup: %v", err)
	}

	var props []proposal
	for _, gq := range suite.Queries {
		if !gq.Confirmed {
			continue
		}
		ids := make([]string, 0, len(gq.Gold))
		for _, t := range gq.Gold {
			id, ok := titleToID[norm(t)]
			if !ok {
				fatal("query %s: no document id for gold title %q", gq.ID, t)
			}
			ids = append(ids, id)
		}
		res, err := svc.Search(ctx, search.Request{
			Query: gq.Q, TopN: 5,
			Filters: &search.Filters{DocumentIDs: ids},
		})
		if err != nil {
			fatal("query %s: %v", gq.ID, err)
		}
		pr := proposal{QueryID: gq.ID, Q: gq.Q, ScopeBooks: gq.Gold, ScopeIDs: ids}
		for i, h := range res.Hits {
			ph := propHit{
				ChunkID: h.ChunkID, Book: h.Source.Title,
				Locator: h.Locator.Label,
				Excerpt: excerptOf(h.Text),
			}
			if i == 0 {
				pr.Best = ph
			} else {
				pr.Alt = append(pr.Alt, ph)
			}
		}
		if pr.Best.ChunkID == "" {
			fatal("query %s: no scoped hits", gq.ID)
		}
		props = append(props, pr)
	}

	propPath := filepath.Join(filepath.Dir(suitePath), "gold_suite_v2_proposals.json")
	out, _ := json.MarshalIndent(map[string]any{"proposals": props}, "", "  ")
	if err := os.WriteFile(propPath, out, 0o644); err != nil {
		fatal("write proposals: %v", err)
	}

	// The compact yes/no list for dudu (max 5 minutes, no book reading).
	fmt.Println("=== V2 GOLD-PASSAGE VORSCHLÄGE (Ja/Nein/Korrektur pro Nummer) ===")
	for i, pr := range props {
		fmt.Printf("%2d. [%s] %q\n    Scope: %s\n    → %s | %s | %s\n       „%s“\n",
			i+1, pr.QueryID, pr.Q, strings.Join(pr.ScopeBooks, " + "),
			shortID(pr.Best.ChunkID), pr.Best.Book, pr.Best.Locator, pr.Best.Excerpt)
		if len(pr.Alt) > 0 {
			fmt.Printf("       Alt: %s („%s“)\n", shortID(pr.Alt[0].ChunkID), pr.Alt[0].Excerpt)
		}
	}
	fmt.Printf("\n%d proposals -> %s\nNach dudus Ja/Nein wird gold_suite_v2.json daraus materialisiert.\n", len(props), propPath)
}

func documentIDByTitle(ctx context.Context, database *db.DB) (map[string]string, error) {
	rows, err := database.Pool().Query(ctx, `
		SELECT DISTINCT z.title, z.id::text
		FROM zotero_documents z
		JOIN processing_snapshots s ON s.document_id = z.id AND s.active`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	first := map[string]string{} // norm(title) -> raw title, for collision reports
	for rows.Next() {
		var t, id string
		if err := rows.Scan(&t, &id); err != nil {
			return nil, err
		}
		n := norm(t)
		if prev, dup := first[n]; dup {
			return nil, fmt.Errorf("title collision after norm: %q vs %q", prev, t)
		}
		first[n] = t
		out[n] = id
	}
	return out, rows.Err()
}

// excerptOf truncates on rune boundaries (no split multi-byte chars).
func excerptOf(text string) string {
	r := []rune(strings.TrimSpace(text))
	if len(r) > 140 {
		r = append(r[:140:140], '…')
	}
	s := string(r)
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "  ", " ")
}

func shortID(id string) string { return id[:min(8, len(id))] }

// --- v2 materialization (#155 execution order, 2026-08-16) -----------------
//
// Applies dudu's Durchnick (issue comment, 21x yes, 3x alt:0) to the
// committed proposals and adds the seven verified entries z1-z7 whose gold
// chunks resolve via search-anchor SQL against the books (dudu's
// ESG_Quellen_und_Zitatnotizen — human-verified passages, the hardest bar).

var v2Decisions = map[string]string{
	"qa1": "yes", "qa2": "yes", "qa3": "yes", "qa4": "yes", "qa5": "yes",
	"c6": "yes", "c7": "yes", "c8": "yes", "c9": "yes",
	"c10": "alt:0", // Konsument*innenverantwortung-Kritik statt Georgien-Anekdote
	"c11": "yes", "c12": "yes", "c13": "yes", "c14": "yes", "c15": "yes",
	"f16": "yes", "f17": "yes",
	"n18": "alt:0", // GRI-Überblick (4. Basis) statt IDW-Abschnitt
	"n19": "yes",
	"n20": "alt:0", // ALT nennt IDW PS 821 explizit
	"n21": "yes", "a22": "yes", "a23": "yes", "a24": "yes", "f25": "yes",
}

type verifiedSpec struct {
	ID, Q, Book, Anchor string
}

// z1-z7: dudu-curated passages from ESG_Quellen_und_Zitatnotizen.md.
var verifiedSpecs = []verifiedSpec{
	{"z1", "Was umfasst ESG über die Triple Bottom Line hinaus?", "CSR und Reporting", "Neben diesen drei Dimensionen sind heute"},
	{"z2", "Wozu dient Nachhaltigkeitsreporting intern?", "CSR und Reporting", "bildet auch intern eine Grundlage für bessere Entscheidungen"},
	{"z3", "Welchen Zweck haben Stakeholder-Beziehungen für Unternehmen?", "Stakeholder-Management und Nachhaltigkeits-Reporting", "Stakeholder-Beziehungen sollen dem Unternehmen dienen"},
	{"z4", "Wie ist strategisches Nachhaltigkeitsmanagement aufgebaut?", "Nachhaltiges Management: Nachhaltigkeit als exzellenten Managementansatz entwickeln", "Es mündet in einer Roadmap mit Zielen"},
	{"z5", "Führt gesellschaftliche Verantwortung zu Kosten oder zu Marktchancen?", "CSR und Innovationsmanagement", "gesteigerter Produktivität und einer Ausweitung der Märkte"},
	{"z6", "Warum muss Nachhaltigkeit über die eigenen vier Wände hinaus in der Wertschöpfungskette liegen?", "CSR und Value Chain Management", "nicht nur in den 'eigenen 4 Wänden'"},
	{"z7", "Wie hat Henkel Nachhaltigkeit messbar gemacht?", "CSR und Value Chain Management", "innerhalb von 20 Jahren das Verhältnis zwischen den geschaffenen Werten"},
}

// materializeV2 writes gold_suite_v2.json: the 25 decided proposals plus the
// z1-z7 verified entries. Pure bookkeeping on the proposals plus anchor SQL.
func materializeV2(ctx context.Context, database *db.DB, suiteDir string) error {
	raw, err := os.ReadFile(filepath.Join(suiteDir, "gold_suite_v2_proposals.json"))
	if err != nil {
		return err
	}
	var props struct {
		Proposals []proposal `json:"proposals"`
	}
	if err := json.Unmarshal(raw, &props); err != nil {
		return err
	}
	titleToID, err := documentIDByTitle(ctx, database)
	if err != nil {
		return err
	}
	out := goldSuite{Note: "v2 (#155): 25 dudu-decided proposals (21 yes / 3 alt:0) + 7 verified entries from ESG_Quellen_und_Zitatnotizen (human-verified gold passages, anchor-resolved). Passage-level scoring: P@1/hit@5/MRR on gold chunk ids, scoped per query."}
	for _, pr := range props.Proposals {
		dec, ok := v2Decisions[pr.QueryID]
		if !ok {
			return fmt.Errorf("no decision for %s", pr.QueryID)
		}
		gold := pr.Best.ChunkID
		if dec != "yes" { // alt:N — zero-based alternative index
			idx := 0
			if len(dec) > 5 {
				idx, _ = strconv.Atoi(dec[4:])
			}
			if idx >= len(pr.Alt) {
				return fmt.Errorf("%s: alt index %d out of range (%d alts)", pr.QueryID, idx, len(pr.Alt))
			}
			gold = pr.Alt[idx].ChunkID
		}
		out.Queries = append(out.Queries, goldQuery{
			ID: pr.QueryID, Type: "proposal", Q: pr.Q,
			Scope: pr.ScopeIDs, GoldChunks: []string{gold},
			Confirmed: true, Origin: "dudu-durchnick:" + dec,
		})
	}
	for _, z := range verifiedSpecs {
		docID, ok := titleToID[norm(z.Book)]
		if !ok {
			return fmt.Errorf("z %s: book %q not found", z.ID, z.Book)
		}
		var chunkID string
		if err := database.Pool().QueryRow(ctx, `
			SELECT c.id::text
			FROM processing_chunks c
			JOIN processing_snapshots s ON s.id = c.snapshot_id AND s.active
			WHERE s.document_id = $1::uuid AND c.text ILIKE '%' || $2 || '%'
			ORDER BY c.chunk_index LIMIT 1`, docID, z.Anchor).Scan(&chunkID); err != nil {
			return fmt.Errorf("z %s anchor %q: %w", z.ID, z.Anchor, err)
		}
		out.Queries = append(out.Queries, goldQuery{
			ID: z.ID, Type: "verified", Q: z.Q,
			Scope: []string{docID}, GoldChunks: []string{chunkID},
			Confirmed: true, Origin: "dudu-zitatnotizen",
		})
		fmt.Printf("  %s: anchor -> chunk %s (%s)\n", z.ID, chunkID[:8], z.Book)
	}
	buf, _ := json.MarshalIndent(out, "", "  ")
	return os.WriteFile(filepath.Join(suiteDir, "gold_suite_v2.json"), buf, 0o644)
}

// --- v2.1 (#155): VWL + ORG_HA verified entries from dudu's traces --------
//
// VWL anchors: quellen_freihandel.txt (old-axiom OpenSearch snippets, quality-
// gated by topic-keyword scoring — only on-topic quotes kept; section 17
// dropped as topically off). ORG_HA anchors: quellennachweise_originalstellen
// _iteration3.md — literal verified blockquotes. Anchors resolve GLOBALLY;
// the scope becomes the resolved chunk's document (citation family checked).

var v21Specs = []verifiedSpec{
	// VWL trace
	{"w1", "Was ist das Ziel des Freihandels?", "Heine/Herr", "Im Zwei-Länder-Fall ist ein Wohlfahrtsgewinn in der Form der Arbeitszeitersparnis"},
	{"w3", "Warum entsteht internationaler Handel durch Arbeitsteilung?", "Eisenhut/Sturm", "Arbeitsteilung, Tausch und Geld"},
	{"w4", "Was sind absolute Kostenvorteile nach Adam Smith?", "Engelkamp/Sell", "Während Adam Smith die Bedeutung der absoluten Kostenvorteil"},
	{"w5", "Worin besteht der komparative Kostenvorteil nach Ricardo?", "Bofinger", "absolute Kostenvorteile"},
	{"w7", "Wie entstehen Produktionsmöglichkeiten durch Spezialisierung?", "Premer", "Die folgende Tabelle 1.2 zeigt das Produktions- und Speziali"},
	{"w8", "Wie entstehen Wohlfahrtsgewinne durch Freihandel?", "Eisenhut/Sturm", "die weitere Wohlfahrtsgewinne ermöglichen. Das GATT"},
	{"w9", "Wie wirken Konsumenten- und Produzentenrente im Außenhandel?", "Premer", "Oder diese Mengeneinheit würde zu einem Preis von"},
	{"w11", "Was besagt das Heckscher-Ohlin-Theorem?", "Mankiw/Taylor", "Die Verfügbarkeit von Produktionsfaktoren: Das Heckscher"},
	{"w14", "Was besagt die Prebisch-Singer-These zu den Terms of Trade?", "Mankiw/Taylor", "Prebisch-Singer-These"},
	{"w15", "Was sind Wechselkurse im Außenhandel?", "Mankiw/Taylor", "kann man mit einer Einheit einer Währung, z. B. eines Euro"},
	{"w18", "Wie wirken Zölle und Importabgaben?", "Engelkamp/Sell", "Importabgaben (an die EU abzuführend"},
	{"w24", "Warum ist die WTO in Handelsverhandlungen blockiert?", "Eisenhut/Sturm", "sind die Verhandlungen oftmals so gut wie blockiert"},
	// ORG_HA trace (literal verified quotes)
	{"o1", "Wie sind die Integrationsdimensionen industrieller Software und KI strukturiert?", "Kett", "The model comprises five hierarchical levels"},
	{"o2", "Welche Folgekosten hat die Reduktion von Abhängigkeiten?", "Schreyögg", "Jeder Entkopplung der Subsysteme drohen kostspielige Reibung"},
	{"o3", "Welchen Nutzen hat Predictive Maintenance?", "VDMA", "Der Lebenszyklus der Anlagen kann verlängert"},
	{"o4", "Wie wirkt NIS2 vertraglich auf Lieferketten?", "NIS2", "die Cybersicherheitsverfahren ih"},
	{"o5", "Welche ökonomischen Wechselkosten entstehen?", "Hungenberg", "Diese können ökonomischer Natur sein"},
	{"o6", "Wie wird KI-gestützte Softwareentwicklung governiert?", "DORA", "Are downstream systems"},
	{"o7", "Wie sind Prozesse mit Input und Output definiert?", "Prozess", "Processes can be defined as a sequence of activities"},
	{"o8", "Was ist ein soziotechnisches System?", "Soziotechnik", "Management is an action-oriented science"},
	{"o9", "Was bedeutet die Überlappung von Umweltsphären?", "Umweltsphären", "Auch die Umweltsphäre Technologie ist"},
}

// materializeV21 extends gold_suite_v2.json with the v2.1 verified entries
// (anchor-resolved globally; scope = resolved document) -> gold_suite_v21.json.
func materializeV21(ctx context.Context, database *db.DB, suiteDir string) error {
	raw, err := os.ReadFile(filepath.Join(suiteDir, "gold_suite_v2.json"))
	if err != nil {
		return err
	}
	var suite goldSuite
	if err := json.Unmarshal(raw, &suite); err != nil {
		return err
	}
	added := 0
	for _, z := range v21Specs {
		var chunkID, title string
		if err := database.Pool().QueryRow(ctx, `
			SELECT c.id::text, zd.title
			FROM processing_chunks c
			JOIN processing_snapshots s ON s.id = c.snapshot_id AND s.active
			JOIN zotero_documents zd ON zd.id = s.document_id
			WHERE c.text ILIKE '%' || $1 || '%'
			ORDER BY c.chunk_index LIMIT 1`, z.Anchor).Scan(&chunkID, &title); err != nil {
			fmt.Printf("  %s: anchor NOT found (%s %q) — skipped\n", z.ID, z.Book, truncate(z.Anchor, 40))
			continue
		}
		var docID string
		if err := database.Pool().QueryRow(ctx, `
			SELECT s.document_id::text FROM processing_chunks c
			JOIN processing_snapshots s ON s.id = c.snapshot_id AND s.active
			WHERE c.id = $1::uuid`, chunkID).Scan(&docID); err != nil {
			return fmt.Errorf("v21 %s doc: %w", z.ID, err)
		}
		suite.Queries = append(suite.Queries, goldQuery{
			ID: z.ID, Type: "verified", Q: z.Q,
			Scope: []string{docID}, GoldChunks: []string{chunkID},
			Confirmed: true, Origin: "v21-trace:" + z.Book,
		})
		fmt.Printf("  %s: %s -> chunk %s | %q\n", z.ID, z.Book, chunkID[:8], truncate(title, 44))
		added++
	}
	suite.Note = "v2.1 (#155): v2 (32 dudu-confirmed scoped entries) + VWL/ORG_HA verified entries from dudu's trace files (quality-gated anchors, globally resolved, scope = resolved document)."
	buf, _ := json.MarshalIndent(suite, "", "  ")
	if err := os.WriteFile(filepath.Join(suiteDir, "gold_suite_v21.json"), buf, 0o644); err != nil {
		return err
	}
	fmt.Printf("gold_suite_v21.json: %d v2 + %d neue = %d Eintraege\n", len(suite.Queries)-added, added, len(suite.Queries))
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
