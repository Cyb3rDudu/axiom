// modes.go — the operator CLI modes (#244): one registry instead of the
// per-mode preamble blocks that used to live in main(). Each mode runs
// ONCE and exits (#202 mode-exit discipline); the shared runner loads
// config, builds the mode's prefixed logger, installs the KG heartbeat
// sink, parses --apply (modes that have it), and opens the database —
// exactly the sequence every hand-written block repeated. The mode body
// is behavior-identical to the former inline blocks; the mode ITs
// (mode_exit_it_test.go) are the passenger proof.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// cliMode is one operator mode. apply controls whether the shared runner
// parses --apply from the remaining args (repoint always applies, so it
// declares apply=false and its body ignores the flag).
type cliMode struct {
	flag   string
	prefix string
	apply  bool
	run    func(logger *log.Logger, apply bool, rp *repo.Repo)
}

var cliModes = []cliMode{
	{
		flag:   "-cleanup-frontmatter-kg",
		prefix: "fmgate: ",
		apply:  true,
		// #198 item 1 — frontmatter cleanup pass: KG relations/entities
		// whose evidence sits in gated frontmatter sections (TOC / author
		// lists / preface / bibliography / index / title lines) leave the
		// active graph. Dry-run by default; --apply executes the drop.
		run: func(logger *log.Logger, apply bool, rp *repo.Repo) {
			rep, err := rp.CleanupFrontmatterKG(context.Background(), apply)
			if err != nil {
				modeFail(logger, modeSingleTx, "cleanup: %v", err)
			}
			out, _ := json.MarshalIndent(rep, "", "  ")
			if apply {
				logger.Printf("frontmatter cleanup APPLIED: %+v", rep.Totals)
			} else {
				logger.Printf("frontmatter cleanup DRY RUN (pass --apply to execute): %+v", rep.Totals)
			}
			fmt.Println(string(out))
		},
	},
	{
		flag:   "-consolidate-relations",
		prefix: "relations: ",
		apply:  true,
		// Wave epilogue mode (#193): consolidation of same-canonical-form
		// entities across active snapshots. Runs ONCE and exits — the wave
		// runbook calls it after the drain (peer of the OS==PG parity
		// check). #198-2: one aggregated edge per (source,target) pair
		// among active snapshots. Dry-run by default; --apply mutates.
		run: func(logger *log.Logger, apply bool, rp *repo.Repo) {
			if !apply {
				_, pairs, err := rp.RelationsConsolidationDryRun(context.Background())
				if err != nil {
					modeFail(logger, modeSingleTx, "dry-run: %v", err)
				}
				logger.Printf("dry-run: %d multi-edge pairs would collapse (use --apply)", pairs)
				return
			}
			rep2, err := rp.ConsolidateRelationsReport(context.Background())
			if err != nil {
				modeFail(logger, modeSingleTx, "consolidate: %v", err)
			}
			logger.Printf("relations consolidation complete: %+v", rep2)
		},
	},
	{
		flag:   "-normalize-entity-types",
		prefix: "typing: ",
		apply:  true,
		// #198-3: deterministic typing rules over active entities.
		// Dry-run by default; --apply mutates.
		run: func(logger *log.Logger, apply bool, rp *repo.Repo) {
			if !apply {
				c, err := rp.EntityTypingCounts(context.Background())
				if err != nil {
					modeFail(logger, modeSingleTx, "dry-run: %v", err)
				}
				logger.Printf("dry-run: %+v (use --apply)", c)
				return
			}
			tr, err := rp.NormalizeEntityTypes(context.Background())
			if err != nil {
				modeFail(logger, modeSingleTx, "normalize: %v", err)
			}
			logger.Printf("entity typing complete: %+v", tr)
		},
	},
	{
		flag:   "-bind-all-aliases",
		prefix: "aliases: ",
		apply:  true,
		// #199 W6: guarded exact+flexion binding in one pass (W3 guards).
		run: func(logger *log.Logger, apply bool, rp *repo.Repo) {
			if !apply {
				c, err := rp.BindExactFormAliasesDryRun(context.Background())
				if err != nil {
					modeFail(logger, modeSingleTx, "dry-run exact: %v", err)
				}
				n, err := rp.EntityAliasCounts(context.Background())
				if err != nil {
					modeFail(logger, modeSingleTx, "dry-run counts: %v", err)
				}
				logger.Printf("dry-run: exact=%+v counts=%+v (use --apply)", c, n)
				return
			}
			ar, err := rp.BindAllAliases(context.Background())
			if err != nil {
				// Two sequential passes (exact, then flexion), each its own
				// transaction: a failure after the first leaves it committed.
				modeFail(logger, modeMultiPass, "bind-all: %v", err)
			}
			logger.Printf("all aliases complete: %+v", ar)
		},
	},
	{
		flag:   "-bind-flexion-aliases",
		prefix: "aliases: ",
		apply:  true,
		// #198-3: flexion family alias links.
		run: func(logger *log.Logger, apply bool, rp *repo.Repo) {
			if !apply {
				c, err := rp.EntityAliasCounts(context.Background())
				if err != nil {
					modeFail(logger, modeSingleTx, "dry-run: %v", err)
				}
				logger.Printf("dry-run: %+v (use --apply)", c)
				return
			}
			ar, err := rp.BindFlexionAliases(context.Background())
			if err != nil {
				modeFail(logger, modeSingleTx, "bind: %v", err)
			}
			logger.Printf("flexion aliases complete: %+v", ar)
		},
	},
	{
		flag:   "-repoint-alias-edges",
		prefix: "repoint: ",
		apply:  false,
		// #198-3 Nachzug: re-point variant edges to family survivors,
		// delete intra-family self-loops, then run -consolidate-relations.
		run: func(logger *log.Logger, apply bool, rp *repo.Repo) {
			if err := rp.RepointAliasEdges(context.Background()); err != nil {
				modeFail(logger, modeSingleTx, "repoint: %v", err)
			}
			logger.Printf("alias-variant edges re-pointed to survivors; intra-family self-loops deleted")
		},
	},
	{
		flag:   "-maintenance-retention",
		prefix: "retention: ",
		apply:  true,
		// #281: retention/GC maintenance run. Dry-run by default (exact
		// removal counts); --apply removes superseded snapshots (chunks/
		// embeddings/relationships cascade) and stale terminal job attempts
		// beyond the retention age. Never deletes: the latest job per
		// document, repair-linked jobs, active-snapshot producers, pending
		// work. Idempotent — a second run reports zero.
		run: func(logger *log.Logger, apply bool, rp *repo.Repo) {
			age := repo.RetentionJobMinAgeDefault
			if v := os.Getenv("AXIOM_RETENTION_JOB_DAYS"); v != "" {
				if d, err := strconv.Atoi(v); err == nil && d > 0 {
					age = time.Duration(d) * 24 * time.Hour
				} else {
					logger.Fatalf("AXIOM_RETENTION_JOB_DAYS must be a positive integer, got %q", v)
				}
			}
			if !apply {
				rep, err := rp.RetentionPlan(context.Background(), age)
				if err != nil {
					modeFail(logger, modeSingleTx, "dry-run: %v", err)
				}
				out, _ := json.MarshalIndent(rep, "", "  ")
				logger.Printf("dry-run (use --apply to execute): snapshots to remove=%d (chunks=%d, dense=%d, sparse=%d, entities=%d, relationships=%d/%d, artifacts=%d); stale job attempts to remove=%d; never-delete: latest-per-document=%d repair-linked=%d active-snapshot=%d non-terminal=%d; superseded kept for pending outbox=%d",
					rep.Snapshots.Remove, rep.Snapshots.Chunks, rep.Snapshots.DenseEmbeddings, rep.Snapshots.SparseEmbeddings,
					rep.Snapshots.Entities, rep.Snapshots.ChunkRelationships, rep.Snapshots.EntityRelationships, rep.Snapshots.Artifacts,
					rep.Jobs.Remove, rep.Jobs.KeepLatest, rep.Jobs.KeepRepairLinked, rep.Jobs.KeepActiveSnap, rep.Jobs.KeepNonTerminal,
					rep.Snapshots.KeepPendingOutbox)
				fmt.Println(string(out))
				return
			}
			rem, plan, err := rp.ApplyRetention(context.Background(), age)
			if err != nil {
				modeFail(logger, modeSingleTx, "apply: %v", err)
			}
			logger.Printf("retention APPLIED: %d superseded snapshot(s) + %d stale job attempt(s) removed (plan had %d/%d)",
				rem.Snapshots, rem.Jobs, plan.Snapshots.Remove, plan.Jobs.Remove)
			out, _ := json.MarshalIndent(rem, "", "  ")
			fmt.Println(string(out))
		},
	},
	{
		flag:   "-consolidate-entities",
		prefix: "epilogue: ",
		apply:  true,
		// #199 W6 hardening: dry-run by default; --apply mutates. This flag
		// shares the same operator discipline as relation consolidation,
		// typing normalization, and alias binding.
		run: func(logger *log.Logger, apply bool, rp *repo.Repo) {
			if !apply {
				report, err := rp.EntityConsolidationDryRun(context.Background())
				if err != nil {
					modeFail(logger, modeSingleTx, "dry-run: %v", err)
				}
				logger.Printf("dry-run: %d guarded groups / %d entities would merge (use --apply)",
					report.DuplicateFormsBefore, report.Merged)
				return
			}
			report, err := rp.ConsolidateEntitiesReport(context.Background())
			if err != nil {
				modeFail(logger, modeSingleTx, "consolidate: %v", err)
			}
			logger.Printf("entity consolidation complete: %d entities merged, duplicate forms %d->%d",
				report.Merged, report.DuplicateFormsBefore, report.DuplicateFormsAfter)
		},
	},
}

// runCLIMode dispatches args[1] to a registered mode and reports whether
// a mode ran (main then returns — a mode NEVER falls through to the server
// boot). The shared preamble preserves the former per-block sequence:
// config -> prefixed logger -> KG heartbeat sink -> --apply parse ->
// db.Open (Fatalf on connect failure, exit 1 before any state is touched).
func runCLIMode(args []string) bool {
	if len(args) < 2 {
		return false
	}
	for _, m := range cliModes {
		if args[1] != m.flag {
			continue
		}
		cfg := config.Load()
		logger := log.New(os.Stderr, m.prefix, log.LstdFlags)
		repo.SetKGProgressLogger(logger.Printf)
		apply := m.apply && hasApplyFlag(args[2:])
		d, err := db.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			logger.Fatalf("postgres: %v", err)
		}
		defer d.Close()
		m.run(logger, apply, repo.New(d.Pool()))
		return true
	}
	return false
}

func hasApplyFlag(args []string) bool {
	for _, a := range args {
		if a == "--apply" {
			return true
		}
	}
	return false
}
