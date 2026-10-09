package main

// --- gold-suite re-anchoring (#351) ---------------------------------------
//
// The v2 suite is retired (its 25 proposal queries duplicated the book-level
// gold_suite.json and its 7 verified entries became gold_suite_z.json — zero
// unique query content). What survives from v2.1 is the trace-verified VWL/
// ORG_HA material: 20 entries whose anchors re-resolve against the CURRENT
// active chunks after every rechunk. materializeTrace re-anchors them and
// rewrites gold_suite_v21.json as a standalone 20-entry passage suite
// (the same re-anchoring pattern the z-suite got in #200).
//
// VWL anchors: quellen_freihandel.txt (old-axiom OpenSearch snippets, quality-
// gated by topic-keyword scoring — only on-topic quotes kept; section 17
// dropped as topically off). ORG_HA anchors: quellennachweise_originalstellen
// _iteration3.md — literal verified blockquotes.
//
// Scope-pinning: every anchor resolves ONLY inside its expected document
// (scopeDoc = the pre-rechunk suite's scope, #200 convention: the scopes stay
// unchanged across re-anchoring). This is what the #351 review demanded after
// a free-global ILIKE first-resolve pinned w5 to an unrelated lecture
// transcript that merely quotes "absolute Kostenvorteile" — anchor text alone
// is NOT a document guarantee. Any anchor miss outside the expected skip set
// is fatal, so the suite can never silently shrink.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cyb3rDudu/axiom/axiom/internal/db"
)

type verifiedSpec struct {
	ID, Q, Book, Anchor string
	// ScopeDoc pins the anchor resolution to the document the entry has
	// pointed at since the original #155 run (uuid from the pre-rechunk
	// gold_suite_v21.json).
	ScopeDoc string
	// SkipDoc names the one honest miss: o9's anchor is not in the corpus
	// at all (was already skipped in the original materialization run).
	SkipDoc string
}

var v21Specs = []verifiedSpec{
	// VWL trace
	{"w1", "Was ist das Ziel des Freihandels?", "Heine/Herr", "Im Zwei-Länder-Fall ist ein Wohlfahrtsgewinn in der Form der Arbeitszeitersparnis", "d5171e42-3596-425f-8e07-6a2b9424f3e5", ""},
	{"w3", "Warum entsteht internationaler Handel durch Arbeitsteilung?", "Eisenhut/Sturm", "Arbeitsteilung, Tausch und Geld", "5d5e482b-8b49-4ce8-ba4f-dae6f3648dd6", ""},
	{"w4", "Was sind absolute Kostenvorteile nach Adam Smith?", "Engelkamp/Sell", "Während Adam Smith die Bedeutung der absoluten Kostenvorteil", "a64defc4-e1bc-4f8b-bf97-be4a716cb039", ""},
	{"w5", "Worin besteht der komparative Kostenvorteil nach Ricardo?", "Bofinger", "absolute Kostenvorteile", "c4dc0951-262d-4db5-98c7-e7061c94a6dc", ""},
	{"w7", "Wie entstehen Produktionsmöglichkeiten durch Spezialisierung?", "Premer", "Die folgende Tabelle 1.2 zeigt das Produktions- und Speziali", "0a182cd3-2c61-410e-9725-25f98ed6fea2", ""},
	{"w8", "Wie entstehen Wohlfahrtsgewinne durch Freihandel?", "Eisenhut/Sturm", "die weitere Wohlfahrtsgewinne ermöglichen. Das GATT", "5d5e482b-8b49-4ce8-ba4f-dae6f3648dd6", ""},
	{"w9", "Wie wirken Konsumenten- und Produzentenrente im Außenhandel?", "Premer", "Oder diese Mengeneinheit würde zu einem Preis von", "0a182cd3-2c61-410e-9725-25f98ed6fea2", ""},
	{"w11", "Was besagt das Heckscher-Ohlin-Theorem?", "Mankiw/Taylor", "Die Verfügbarkeit von Produktionsfaktoren: Das Heckscher", "1af168c9-bac8-4aa9-b201-d0b7bbc84fbd", ""},
	{"w14", "Was besagt die Prebisch-Singer-These zu den Terms of Trade?", "Mankiw/Taylor", "Prebisch-Singer-These", "a3a025f3-93a4-40d2-8828-b905f1ba22b5", ""},
	{"w15", "Was sind Wechselkurse im Außenhandel?", "Mankiw/Taylor", "kann man mit einer Einheit einer Währung, z. B. eines Euro", "1af168c9-bac8-4aa9-b201-d0b7bbc84fbd", ""},
	{"w18", "Wie wirken Zölle und Importabgaben?", "Engelkamp/Sell", "Importabgaben (an die EU abzuführend", "a64defc4-e1bc-4f8b-bf97-be4a716cb039", ""},
	{"w24", "Warum ist die WTO in Handelsverhandlungen blockiert?", "Eisenhut/Sturm", "sind die Verhandlungen oftmals so gut wie blockiert", "5d5e482b-8b49-4ce8-ba4f-dae6f3648dd6", ""},
	// ORG_HA trace (literal verified quotes)
	{"o1", "Wie sind die Integrationsdimensionen industrieller Software und KI strukturiert?", "Kett", "The model comprises five hierarchical levels", "34754197-4fb0-4332-ac7f-b2651774d1eb", ""},
	{"o2", "Welche Folgekosten hat die Reduktion von Abhängigkeiten?", "Schreyögg", "Jeder Entkopplung der Subsysteme drohen kostspielige Reibung", "5fcf9d8c-99fe-434e-9412-d22c98ea8b70", ""},
	{"o3", "Welchen Nutzen hat Predictive Maintenance?", "VDMA", "Der Lebenszyklus der Anlagen kann verlängert", "9129d22a-4349-40d1-a87c-c32d121ec010", ""},
	{"o4", "Wie wirkt NIS2 vertraglich auf Lieferketten?", "NIS2", "die Cybersicherheitsverfahren ih", "8e3825d5-8003-4f40-b6ea-ef48c2119e07", ""},
	{"o5", "Welche ökonomischen Wechselkosten entstehen?", "Hungenberg", "Diese können ökonomischer Natur sein", "692d9cd0-a7c9-46cf-858b-a8d3c765622a", ""},
	{"o6", "Wie wird KI-gestützte Softwareentwicklung governiert?", "DORA", "Are downstream systems", "02e560ac-07a2-43fd-aa13-27259c0d4328", ""},
	{"o7", "Wie sind Prozesse mit Input und Output definiert?", "Prozess", "Processes can be defined as a sequence of activities", "4bba62c6-cbc6-4f78-bc3d-c6585bcba80c", ""},
	{"o8", "Was ist ein soziotechnisches System?", "Soziotechnik", "Management is an action-oriented science", "4bba62c6-cbc6-4f78-bc3d-c6585bcba80c", ""},
	{"o9", "Was bedeutet die Überlappung von Umweltsphären?", "Umweltsphären", "Auch die Umweltsphäre Technologie ist", "", "kein Anker im Korpus — seit dem Original-Lauf ehrlich übersprungen"},
}

// materializeTrace re-anchors the trace-verified entries against the CURRENT
// active chunks, each pinned to its expected scope document, and rewrites
// gold_suite_v21.json as a standalone suite (no v2 base anymore — v2 is
// retired, #351).
func materializeTrace(ctx context.Context, database *db.DB, suiteDir string) error {
	out := goldSuite{Note: "#351 re-anchored trace suite: the 20 VWL/ORG_HA verified entries formerly carried inside v2.1 (v2 base retired — its 25 proposal queries duplicated the book-level gold_suite.json, its 7 z-entries became gold_suite_z.json). Anchors re-resolved against the current active chunks, each pinned to its pre-rechunk scope document (#200 convention: scopes unchanged); an unexpected anchor miss is fatal, o9 stays the one honest skip. Passage-level scoring like the z-suite. Known quirk kept for scope stability: w14 resolves to the Kompakt-Lexikon Internationale Wirtschaft (the original run's global first-hit), not the Mankiw/Taylor volume named in the trace — documented in RETRIEVAL_BENCHMARK.md."}
	skipped := 0
	for _, z := range v21Specs {
		if z.ScopeDoc == "" { // the one honest miss, expected since the original run
			fmt.Printf("  %s: expected skip (%s): %s\n", z.ID, z.Book, z.SkipDoc)
			skipped++
			continue
		}
		var chunkID, title, creators string
		err := database.Pool().QueryRow(ctx, `
			SELECT c.id::text, sd.title, sd.creators
			FROM processing_chunks c
			JOIN processing_snapshots s ON s.id = c.snapshot_id AND s.active
			JOIN store_documents sd ON sd.document_id = s.document_id AND sd.preferred
			WHERE s.document_id = $1::uuid AND c.text ILIKE '%' || $2 || '%'
			ORDER BY c.chunk_index LIMIT 1`, z.ScopeDoc, z.Anchor).Scan(&chunkID, &title, &creators)
		if err != nil {
			return fmt.Errorf("trace %s: anchor %q not found in pinned scope doc %s: %w", z.ID, z.Anchor, z.ScopeDoc, err)
		}
		out.Queries = append(out.Queries, goldQuery{
			ID: z.ID, Type: "verified", Q: z.Q,
			Scope: []string{z.ScopeDoc}, GoldChunks: []string{chunkID},
			Confirmed: true, Origin: "v21-trace:" + z.Book,
		})
		fmt.Printf("  %s: %s -> chunk %s | %q | %q\n", z.ID, z.Book, chunkID[:8], truncate(title, 44), truncate(creators, 44))
	}
	buf, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(filepath.Join(suiteDir, "gold_suite_v21.json"), buf, 0o644); err != nil {
		return err
	}
	fmt.Printf("gold_suite_v21.json: %d trace-verified Eintraege (re-anchored, scope-pinned), %d erwartete Skips\n", len(out.Queries), skipped)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
