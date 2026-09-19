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
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// cliMode is one operator mode. apply controls whether the shared runner
// parses --apply from the remaining args (repoint always applies, so it
// declares apply=false and its body ignores the flag). args is the raw
// remaining argv so a mode can parse its own flags (#290: retention's
// --timeout / --batch).
type cliMode struct {
	flag   string
	prefix string
	apply  bool
	run    func(logger *log.Logger, apply bool, rp *repo.Repo, args []string)
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
		run: func(logger *log.Logger, apply bool, rp *repo.Repo, args []string) {
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
		run: func(logger *log.Logger, apply bool, rp *repo.Repo, args []string) {
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
		run: func(logger *log.Logger, apply bool, rp *repo.Repo, args []string) {
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
		run: func(logger *log.Logger, apply bool, rp *repo.Repo, args []string) {
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
		run: func(logger *log.Logger, apply bool, rp *repo.Repo, args []string) {
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
		run: func(logger *log.Logger, apply bool, rp *repo.Repo, args []string) {
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
		//
		// #290 execution form: the apply deletes in TRANCHES (one commit per
		// tranche, progress line each) and the whole run is deadline-bounded —
		// the first production apply died to a 40-min monolithic DELETE whose
		// connection the host<->VM port-forward silently dropped, and the
		// client then hung 9h with no deadline. An expired deadline (or any
		// dead connection) fails the run loudly; committed tranches stay and
		// a re-run resumes — hence modeMultiPass on failure.
		run: func(logger *log.Logger, apply bool, rp *repo.Repo, args []string) {
			age := repo.RetentionJobMinAgeDefault
			if v := os.Getenv("AXIOM_RETENTION_JOB_DAYS"); v != "" {
				if d, err := strconv.Atoi(v); err == nil && d > 0 {
					age = time.Duration(d) * 24 * time.Hour
				} else {
					logger.Fatalf("AXIOM_RETENTION_JOB_DAYS must be a positive integer, got %q", v)
				}
			}
			timeout := retentionTimeoutDefault
			if v := envOrFlag(args, "AXIOM_RETENTION_TIMEOUT", "--timeout"); v != "" {
				if d, err := time.ParseDuration(v); err == nil && d > 0 {
					timeout = d
				} else {
					logger.Fatalf("retention timeout must be a positive Go duration (e.g. 2h, 90m), got %q", v)
				}
			}
			batch := 0 // 0 -> repo.RetentionBatchDefault
			if v := envOrFlag(args, "AXIOM_RETENTION_BATCH", "--batch"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					batch = n
				} else {
					logger.Fatalf("retention batch must be a positive integer, got %q", v)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if !apply {
				rep, err := rp.RetentionPlan(ctx, age)
				if err != nil {
					modeFail(logger, modeSingleTx, "dry-run: %v", err)
				}
				out, _ := json.MarshalIndent(rep, "", "  ")
				logger.Printf("dry-run (use --apply to execute): snapshots to remove=%d (chunks=%d, dense=%d, sparse=%d, entities=%d, relationships=%d/%d, artifacts=%d); stale job attempts to remove=%d; never-delete: latest-per-document=%d repair-linked=%d active-snapshot=%d non-terminal=%d; superseded kept for outbox rows=%d",
					rep.Snapshots.Remove, rep.Snapshots.Chunks, rep.Snapshots.DenseEmbeddings, rep.Snapshots.SparseEmbeddings,
					rep.Snapshots.Entities, rep.Snapshots.ChunkRelationships, rep.Snapshots.EntityRelationships, rep.Snapshots.Artifacts,
					rep.Jobs.Remove, rep.Jobs.KeepLatest, rep.Jobs.KeepRepairLinked, rep.Jobs.KeepActiveSnap, rep.Jobs.KeepNonTerminal,
					rep.Snapshots.KeepOutboxHeld)
				fmt.Println(string(out))
				return
			}
			progress := func(phase string, done, total int) {
				logger.Printf("apply progress: %s removed %d/%d", phase, done, total)
			}
			rem, plan, err := rp.ApplyRetention(ctx, age, batch, progress)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					modeFail(logger, modeMultiPass, "apply ABORTED: run deadline (%s) exceeded — committed tranches are persisted and safe, a re-run resumes where this one stopped: %v", timeout, err)
				}
				modeFail(logger, modeMultiPass, "apply: %v", err)
			}
			// artifact BYTES after the commit: rows are already gone; a
			// failed unlink leaves an orphaned file (reported), never a
			// dangling row (#270 review: "deleted means gone" includes the
			// storage under AXIOM_ARTIFACT_ROOT)
			unlinked := 0
			for _, p := range rem.ArtifactPaths {
				if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					logger.Printf("artifact unlink failed (orphaned file remains): %s: %v", p, err)
					continue
				}
				unlinked++
			}
			logger.Printf("retention APPLIED: %d superseded snapshot(s) + %d stale job attempt(s) removed, %d/%d artifact file(s) unlinked (plan had %d/%d)",
				rem.Snapshots, rem.Jobs, unlinked, len(rem.ArtifactPaths), plan.Snapshots.Remove, plan.Jobs.Remove)
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
		run: func(logger *log.Logger, apply bool, rp *repo.Repo, args []string) {
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

// retentionTimeoutDefault bounds a retention run (#290): generous default
// 2h (the 182-snapshot backlog applied in ~40 min of pure DELETE; the
// bound exists so a DEAD connection ends the run loudly instead of the
// 9-hour hang the incident produced), overridable via AXIOM_RETENTION_TIMEOUT
// or --timeout=.
const retentionTimeoutDefault = 2 * time.Hour

// flagValue returns the value of the first --name=value arg in args, or "".
func flagValue(args []string, name string) string {
	prefix := name + "="
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix)
		}
	}
	return ""
}

// envOrFlag resolves a mode setting: env var if set, else --name=value
// flag. Empty means unset (caller keeps its default).
func envOrFlag(args []string, env, flag string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return flagValue(args, flag)
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
		m.run(logger, apply, repo.New(d.Pool()), args[2:])
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
