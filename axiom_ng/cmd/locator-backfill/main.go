// Command locator-backfill enriches the ACTIVE snapshot's chunk locators
// with print pages derived from an enriched EPUB sibling (#233) — no
// conversion, no re-chunking, no embedding, no new snapshot. The flow and
// the safety guarantees (snapshot freeze, one transaction, doc-update
// re-index, refuse-never-guess) live in internal/backfill.
//
// Env: AXIOM_DATABASE_URL (required), AXIOM_OPENSEARCH_URL (+ optional
// AXIOM_OPENSEARCH_USERNAME/PASSWORD) required for real runs, AXIOM_RUNNER_
// PYTHON (runner venv python; default: repo-relative axiom_ng_runner/.venv).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/backfill"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
)

func main() {
	doc := flag.String("doc", "", "document zotero_key (required)")
	epub := flag.String("epub", "", "candidate EPUB path (default: auto-discover, injected preferred)")
	dry := flag.Bool("dry-run", false, "print the plan without writing")
	skipIndex := flag.Bool("skip-index", false, "skip the OpenSearch re-index")
	budget := flag.Duration("budget", 15*time.Minute, "wall-clock budget for the alignment engine")
	dsn := flag.String("dsn", os.Getenv("AXIOM_DATABASE_URL"), "database DSN (default AXIOM_DATABASE_URL)")
	flag.Parse()
	if *doc == "" || *dsn == "" {
		fmt.Fprintln(os.Stderr, "locator-backfill: -doc and AXIOM_DATABASE_URL/-dsn are required")
		os.Exit(2)
	}

	ctx := context.Background()
	database, err := db.Open(ctx, *dsn)
	if err != nil {
		fatal("connect: %v", err)
	}
	defer database.Close()

	rep, err := backfill.Run(ctx, database.Pool(), backfill.Options{
		DocKey:   *doc,
		EpubPath: *epub,
		DryRun:   *dry,
		Budget:   *budget,
		Python:   os.Getenv("AXIOM_RUNNER_PYTHON"),
		OSBaseURL: func() string {
			if *skipIndex {
				return ""
			}
			return os.Getenv("AXIOM_OPENSEARCH_URL")
		}(),
		OSUser: os.Getenv("AXIOM_OPENSEARCH_USERNAME"),
		OSPass: os.Getenv("AXIOM_OPENSEARCH_PASSWORD"),
		Logf:   func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})
	if err != nil {
		fatal("%v", err)
	}
	if rep.Refused {
		fmt.Printf("REFUSED (whole backfill): %s\n", rep.RefusedReason)
		os.Exit(1)
	}
	if *dry {
		fmt.Printf("dry-run: aligned=%d anchors, %d/%d targets enriched, %d refused — nothing written\n",
			rep.Plan.AnchorCount, rep.Plan.PagesEnriched, rep.Plan.EnrichmentTargets, rep.Plan.PagesRefused)
		for _, r := range rep.Plan.Results {
			if r.Enrich {
				fmt.Printf("  would enrich %s -> S. %d-%d (conf %.2f)\n",
					shortID(r.ChunkID), deref(r.PageStart), deref(r.PageEnd), r.Confidence)
			}
		}
		return
	}
	fmt.Printf("done: %d locators enriched, %d index docs updated (re-run must report 0)\n",
		rep.Updated, rep.Reindexed)
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "locator-backfill: "+f+"\n", a...)
	os.Exit(1)
}
