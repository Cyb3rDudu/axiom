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
// _iteration3.md — literal verified blockquotes. Anchors resolve GLOBALLY;
// the scope becomes the resolved chunk's document (citation family checked).

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
}

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

// materializeTrace re-anchors the trace-verified entries against the CURRENT
// active chunks and rewrites gold_suite_v21.json as a standalone suite (no v2
// base anymore — v2 is retired, #351).
func materializeTrace(ctx context.Context, database *db.DB, suiteDir string) error {
	out := goldSuite{Note: "#351 re-anchored trace suite: the 20 VWL/ORG_HA verified entries formerly carried inside v2.1 (v2 base retired — its 25 proposal queries duplicated the book-level gold_suite.json, its 7 z-entries became gold_suite_z.json). Anchors re-resolved globally against the current active chunks; scope = resolved document. Passage-level scoring like the z-suite."}
	for _, z := range v21Specs {
		var chunkID, docID string
		if err := database.Pool().QueryRow(ctx, `
			SELECT c.id::text, s.document_id::text
			FROM processing_chunks c
			JOIN processing_snapshots s ON s.id = c.snapshot_id AND s.active
			WHERE c.text ILIKE '%' || $1 || '%'
			ORDER BY c.chunk_index LIMIT 1`, z.Anchor).Scan(&chunkID, &docID); err != nil {
			fmt.Printf("  %s: anchor NOT found (%s): %v\n", z.ID, z.Book, err)
			continue
		}
		out.Queries = append(out.Queries, goldQuery{
			ID: z.ID, Type: "verified", Q: z.Q,
			Scope: []string{docID}, GoldChunks: []string{chunkID},
			Confirmed: true, Origin: "v21-trace:" + z.Book,
		})
		fmt.Printf("  %s: %s -> chunk %s\n", z.ID, z.Book, chunkID[:8])
	}
	buf, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(filepath.Join(suiteDir, "gold_suite_v21.json"), buf, 0o644); err != nil {
		return err
	}
	fmt.Printf("gold_suite_v21.json: %d trace-verified Eintraege (re-anchored)\n", len(out.Queries))
	return nil
}
