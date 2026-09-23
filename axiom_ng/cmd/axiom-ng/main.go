// Command axiom-ng runs the Zotero RAG sidecar. It orchestrates indexing of a
// Zotero library and exposes the REST API for retrieval and chat. It runs on
// the same host as Zotero and resolves attachments to local files.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/composition"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/version"
)

// main is a thin caller (#298 composition root): CLI mode handling, the
// debug-bind guard and the process-level signal/exit policy stay HERE;
// which components exist, their start order and their ordered shutdown
// live in internal/composition.
func main() {
	// #205 §5: version stamp. Release builds inject Version/Commit/BuildType
	// via -ldflags; a bare `go build` reports the debug default.
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version.Banner())
		return
	}

	// #202 mode-exit discipline: documented mode surface + exit-code contract.
	// Every mode flag runs ONCE and exits — it never falls through to the
	// server boot (the -bind-all-aliases incident class).
	if len(os.Args) > 1 && (os.Args[1] == "-help" || os.Args[1] == "--help" || os.Args[1] == "help") {
		fmt.Print(modeHelp)
		return
	}
	if runCLIMode(os.Args) {
		return
	}
	cfg := config.Load()
	logger := log.New(os.Stderr, "axiom-ng: ", log.LstdFlags)
	logger.Printf("starting %s", version.Banner())
	// #202: long-running mutating KG passes (CLI modes and the standing
	// post-sync consolidation) emit a heartbeat line every 30s through this
	// sink — a supervised run can tell "working" from "hung".
	repo.SetKGProgressLogger(logger.Printf)

	// #205 §5: a debug build must never serve production ports. Production is
	// 8011 (API) and 8013–8015 (dispatchers). Opt out explicitly for local
	// dev with AXIOM_ALLOW_DEBUG_BIND=1.
	if version.DebugBindRefused(version.BuildType, cfg.APIPort, os.Getenv) {
		logger.Fatalf("refusing to bind production port %d with %s — build a release artifact (make rag) or set AXIOM_ALLOW_DEBUG_BIND=1 for local dev",
			cfg.APIPort, version.Banner())
	}

	// Build the composition: same components, same order, same log lines as
	// the pre-F04 inline wiring (F01 goldens witness the behavior identity).
	// A Select/Start error is a loud non-zero exit — a selective start with a
	// missing port refuses to boot half-wired instead of degrading silently.
	root, err := composition.Full(cfg, logger, composition.Ports{})
	if err != nil {
		logger.Fatalf("startup: %v", err)
	}

	// One signal context drives graceful shutdown of BOTH the dispatcher and
	// the HTTP server, so SIGINT/SIGTERM cannot be held off by one half the
	// process.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root.Start(sigCtx); err != nil {
		// Start already stopped the partial composition — no zombie survives
		// the non-zero exit.
		logger.Fatalf("startup: %v", err)
	}

	select {
	case <-sigCtx.Done():
		logger.Printf("signal received; shutting down")
		stop() // idempotent; release the signal handler
	case err := <-root.Fatal():
		// #214 (dispatcher) or a serve error (http): both carry their full
		// diagnosis prefix from the composition; exit non-zero so the
		// supervisor restarts the process cleanly.
		logger.Fatalf("%v", err)
	}

	// Graceful stop within the standing 15s window; the ordered shutdown
	// joins the dispatcher/fixer drains before the pool closes.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root.Stop(shutdownCtx)
}

// modeHelp documents the CLI mode surface and the exit-code contract (#202).
const modeHelp = `axiom-ng — modes and exit codes (#202)

Usage: axiom-ng [mode] [--apply]

Modes (each runs ONCE and exits; never falls through to the server boot):
  --version                       print the version banner
  -help                           this help
  -cleanup-frontmatter-kg         drop/strip KG evidence from gated frontmatter
                                  sections; dry-run default, --apply mutates
  -consolidate-relations          one aggregated edge per (source,target) pair;
                                  dry-run default, --apply mutates
  -normalize-entity-types         deterministic typing rules; --apply mutates
  -bind-all-aliases               guarded exact+flexion alias binding; dry-run
                                  default, --apply mutates
  -bind-flexion-aliases           flexion family alias links; --apply mutates
  -repoint-alias-edges            re-point variant edges to survivors, delete
                                  intra-family self-loops (always applies)
  -consolidate-entities           merge same-form active entities; dry-run
                                  default, --apply mutates
  -maintenance-retention          remove superseded snapshots + stale job
                                  attempts (never the outcome truth); dry-run
                                  default, --apply mutates (#281). The apply
                                  deletes in committed tranches under a run
                                  deadline (#290): --timeout=2h (default,
                                  or AXIOM_RETENTION_TIMEOUT) and --batch=N
                                  (snapshots per transaction, or
                                  AXIOM_RETENTION_BATCH); one progress line
                                  per tranche; an interrupted run is resumed
                                  by simply re-running it
  (no mode flag)                  start the API server + optional dispatcher

Exit codes:
  0   done — either applied or nothing to do; the final log line carries
      the counts (zeros mean nothing to do), dry runs end with "use --apply"
  1   failure — the log states whether the KG is consistent:
      "state consistent (transaction rolled back)" for single-transaction
      modes, "state partial ... re-run the mode" for multi-pass modes
      (all passes are idempotent). A DB/connect failure exits 1 before any
      state is touched.

Long mutating passes emit a heartbeat log line every 30s (elapsed, items
done/total, current item) so a supervised run can tell working from hung.
`

// modeFail terminates a mutating CLI mode with the documented non-zero exit
// (#202). consistent=modeSingleTx: the mode is single-transaction — the failure
// rolled it back, the KG is in its pre-run state. consistent=modeMultiPass: the
// mode runs multiple sequential passes and earlier passes already committed;
// every pass is idempotent, so re-running the mode is the documented recovery.
const (
	modeSingleTx  = true
	modeMultiPass = false
)

func modeFail(logger *log.Logger, consistent bool, format string, args ...any) {
	state := "state consistent (transaction rolled back, nothing applied)"
	if !consistent {
		state = "state partial: earlier passes already committed; all passes are idempotent — re-run the mode"
	}
	// Copy args before appending — the caller's variadic slice may be exactly
	// sized (append would otherwise share/overwrite backing arrays).
	logger.Printf("MODE FAILED (exit 1): "+format+" — %s", append(append([]any{}, args...), state)...)
	os.Exit(1)
}
