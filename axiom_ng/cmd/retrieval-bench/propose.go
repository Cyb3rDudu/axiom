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
				ChunkID: h.ChunkID, Book: h.Source.Book,
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
