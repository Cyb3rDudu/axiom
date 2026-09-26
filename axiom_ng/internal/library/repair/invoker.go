// invoker.go — the Library-owned repair orchestrator (F08 #302, born as
// the #206 mail-ingest-side fixer caller in internal/fixerinvoker).
//
// Owner contract: the repair worker is an EVENT RUNNER (no launchd, no
// KeepAlive); this orchestrator is its only systematic caller. It polls
// the repair_cases queue (status queued), claims one case at a time
// (bounded concurrency), executes the isolated axiom-repair-worker ONCE
// per attachment key — strictly behind the RepairExecutor seam — and
// drives the case through the #184 state machine this package owns:
//
//	queued →(claim)→ in_repair →(worker exit 0 + healed pdf)→ healed
//	                              →(non-zero/timeout)→ queued (retry) | failed
//
// Crash-safety (no new crash-loop class, owner nail 5):
//   - the orchestrator NEVER dies on a hung worker: every execution runs
//     under its own budget (a backstop ABOVE the wrapper's built-in 30-min
//     kill, so the lockdir normally does the reaping) and a failing or
//     panicking case is logged and failed/requeued, never propagated;
//   - a dead orchestrator loses no case: the queue lives in the DB; stale
//     in_repair cases (claim older than the runtime window) are requeued
//     on the next start — the dispatcher lease-recovery pattern. The
//     per-attachment loop guard still caps total attempts.
//
// The per-key lockdir inside the worker wrapper additionally serializes
// against NON-orchestrator concurrency (an operator running a manual
// repair): such an invocation exits 3 and is treated as an ordinary
// retryable failure.
package repair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
	"github.com/jackc/pgx/v5"
)

// Config bounds one orchestrator run.
type Config struct {
	// Command is the worker wrapper executed as: Command <key> --apply.
	// Default CanonicalWorkerCommand (axiom-repair-worker; legacy
	// axiom-fixer fallback in LocalExecutor).
	Command string
	// WorkRoot is the worker's WORK_ROOT: the healed pdf of a key is read
	// from WorkRoot/<key>/work.pdf after a successful execution.
	// Default ~/.local/state/axiom/runs (worker default).
	WorkRoot string
	// Interval is the queue poll interval. Default 30s.
	Interval time.Duration
	// Timeout is the per-invocation backstop. It must sit ABOVE fix.sh's
	// own 30-min timeout so the wrapper (lockdir + timeout binary) does
	// the primary killing; this context only catches a wedged wrapper.
	// Default 35m.
	Timeout time.Duration
	// OCRTimeout (#284/#293) is the WEDGE-GUARD backstop for OCR-class
	// repairs (scan_ocr_rebuild): the OCR process runs as long as it runs
	// — NOBODY computes a budget up front, and the tool itself carries no
	// internal kill. This backstop exists ONLY to prevent orphans (a
	// wedged process that stopped working), NEVER to limit tempo; it is
	// therefore configured generously. Default 24h. fix.sh's timeout
	// binary stays the primary killer (budget minus 5m slack via
	// AXIOM_FIX_SH_TIMEOUT) — same layering as Timeout.
	OCRTimeout time.Duration
	// Concurrency caps parallel worker executions per host (owner nail 3:
	// max 1-2). Values below 1 clamp to 1, above 2 clamp to 2.
	Concurrency int
	// StaleAfter bounds how long an in_repair claim may sit before the
	// reaper requeues it (dead-invoker recovery). Default 40m (Timeout
	// plus slack so a live, merely slow invocation is never requeued
	// under a second invoker).
	StaleAfter time.Duration
	// OCRStaleAfter (#284) is the same bound for OCR-class cases — derived
	// from OCRTimeout so a live, merely slow rebuild is never requeued under
	// a second claim mid-run. Default OCRTimeout + 5m (24h5m with the #293
	// wedge-guard default).
	OCRStaleAfter time.Duration
}

func (c *Config) fillDefaults() {
	if c.Command == "" {
		// F08 (#302, ADR 0001 §4): the canonical worker name; the local
		// executor falls back to the legacy axiom-fixer shim on installs
		// that have not re-run install_dist.sh yet (0.2.x: swap optional).
		c.Command = CanonicalWorkerCommand
	}
	if c.WorkRoot == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.WorkRoot = filepath.Join(home, ".local", "state", "axiom", "runs")
		} else {
			c.WorkRoot = "."
		}
	}
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 35 * time.Minute
	}
	if c.OCRTimeout <= 0 {
		// #293: 24h wedge-guard, not a tempo budget — orphan prevention only
		c.OCRTimeout = 24 * time.Hour
	}
	if c.Concurrency < 1 {
		c.Concurrency = 1
	}
	if c.Concurrency > 2 {
		c.Concurrency = 2
	}
	if c.StaleAfter <= c.Timeout {
		// structural invariant: a live-but-slow invocation must never be
		// requeued under a second claim while it is still running
		c.StaleAfter = c.Timeout + 5*time.Minute
	}
	if c.OCRStaleAfter <= c.OCRTimeout {
		c.OCRStaleAfter = c.OCRTimeout + 5*time.Minute
	}
}

// Deps are the orchestrator's outward effects. Apply is the shared custody
// sequence (ApplyDeps in this package) so tests can fake the Zotero writes.
// Sync is the #282 post-heal auto-sync: after a successful heal the
// orchestrator runs a targeted sync (include = the healed document) so the
// healed attachment enqueues and processes without operator action. nil
// disables the hook (log-only — the wave gate surfaces the stranded heal,
// see Store.WaveRepairGate).
//
// Executor (F08 #302) is the worker-execution PORT: nil binds the local
// binding (LocalExecutor over the configured worker command — the
// process-group-hardened exec); the composition root's fake-binding proof
// and any future transport inject here without touching the state machine.
type Deps struct {
	Store          *Store
	Apply          ApplyDeps
	QuarantineRoot string
	Sync           HealSyncer
	Executor       RepairExecutor
}

// HealSyncer is the post-heal sync surface (#282). *sync.Service satisfies
// it — Run with a one-run include override enqueues exactly the healed
// document (selection rules still apply on top).
type HealSyncer interface {
	Run(ctx context.Context, override *sync.SyncOverride) (sync.Result, error)
}

// Invoker drives the repair queue.
type Invoker struct {
	cfg    Config
	deps   Deps
	logger *log.Logger
	sem    chan struct{}
	// inFlight joins the per-case goroutines so Run can drain them before
	// returning (#298 ordered shutdown: a claimed case's fate is decided
	// under the run context, never orphaned mid-write).
	inFlight stdsync.WaitGroup
	// stopped closes when Run (including its in-flight drain) returned.
	stopped chan struct{}
}

// New builds an orchestrator (cfg defaults are filled here).
func New(cfg Config, deps Deps, logger *log.Logger) *Invoker {
	cfg.fillDefaults()
	if logger == nil {
		logger = log.New(os.Stderr, "repair-invoker ", log.LstdFlags)
	}
	return &Invoker{cfg: cfg, deps: deps, logger: logger, sem: make(chan struct{}, cfg.Concurrency), stopped: make(chan struct{})}
}

// Stopped closes when Run and every in-flight case goroutine have returned —
// the drain signal the composition root waits on during an ordered shutdown.
func (inv *Invoker) Stopped() <-chan struct{} { return inv.stopped }

// Run polls the queue until ctx is done. It only returns on ctx
// cancellation — every per-case failure is handled, never propagated
// (owner nail 5: no crash-loop class). Run is SINGLE-SHOT like
// Dispatcher.Run: Stopped closes exactly once, a second Run call panics
// (no production path does; tests construct a fresh Invoker per run).
func (inv *Invoker) Run(ctx context.Context) error {
	defer close(inv.stopped)
	inv.logger.Printf("repair orchestrator starting: cmd=%s interval=%s timeout=%s concurrency=%d workroot=%s",
		inv.cfg.Command, inv.cfg.Interval, inv.cfg.Timeout, inv.cfg.Concurrency, inv.cfg.WorkRoot)
	// Lease recovery FIRST (a previous invoker may have died mid-case).
	inv.reapStale(ctx)

	t := time.NewTicker(inv.cfg.Interval)
	defer t.Stop()
	for {
		// reap EVERY tick (one UPDATE): in-process orphans (a DB error after
		// claim, a recovered panic) must not sit in in_repair until the next
		// process restart — the reaper is the single recovery net.
		inv.reapStale(ctx)
		inv.pollOnce(ctx)
		select {
		case <-ctx.Done():
			// Ordered shutdown (#298): cancel first, then join the in-flight
			// case goroutines. Their execs are killed by the shared ctx; the
			// status writes under a cancelled ctx fail loudly-but-harmlessly
			// and the case stays in_repair for the stale-reaper (dead-invoker
			// recovery) — claims are handed back, never lost.
			inv.inFlight.Wait()
			inv.logger.Printf("repair orchestrator stopped")
			return nil
		case <-t.C:
		}
	}
}

func (inv *Invoker) reapStale(ctx context.Context) {
	n, err := inv.deps.Store.RequeueStaleRepairCases(ctx, inv.cfg.StaleAfter, inv.cfg.OCRStaleAfter)
	if err != nil {
		inv.logger.Printf("requeue-stale: %v", err)
		return
	}
	if n > 0 {
		inv.logger.Printf("requeued %d stale in_repair case(s) (dead invoker recovery)", n)
	}
}

func (inv *Invoker) pollOnce(ctx context.Context) {
	cases, err := inv.deps.Store.ListRepairQueue(ctx)
	if err != nil {
		inv.logger.Printf("queue: %v", err)
		return
	}
	for i := range cases {
		c := cases[i]
		select {
		case inv.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		inv.inFlight.Add(1)
		go func() {
			defer inv.inFlight.Done()
			defer func() { <-inv.sem }()
			defer func() {
				if r := recover(); r != nil {
					inv.logger.Printf("case %s: panic recovered: %v", c.ID, r)
				}
			}()
			inv.processCase(ctx, c.ID)
		}()
	}
}

// processCase takes ONE case item→claim → worker run → status transition.
// The item resolves BEFORE the claim: attachment-gone blocks a case while
// it is still queued (BlockRepairCase refuses in_repair by design nail —
// a mid-flight case is never touched from outside).
func (inv *Invoker) processCase(ctx context.Context, caseID string) {
	item, err := inv.deps.Store.RepairCaseItem(ctx, caseID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// attachment/document gone at the source — park, don't re-serve
			// forever (W3a rule mirrored from the queue listing)
			if berr := inv.deps.Store.BlockRepairCase(ctx, caseID, "attachment-gone"); berr != nil {
				inv.logger.Printf("case %s: attachment-gone block: %v", caseID, berr)
			}
			return
		}
		inv.logger.Printf("case %s: item: %v", caseID, err)
		return
	}

	// claim: queued → in_repair (loop guard escalation included — a case
	// past max attempts is blocked_for_dudu by the claim itself)
	if _, err := inv.deps.Store.ClaimRepairCase(ctx, caseID); err != nil {
		inv.logger.Printf("case %s: claim: %v", caseID, err)
		return
	}

	inv.logger.Printf("case %s: executing repair worker for key %s", caseID, item.AttachmentKey)
	rc, out, runErr := inv.runWorker(ctx, item)

	if rc == 0 && runErr == nil {
		inv.handleSuccess(ctx, caseID, item, out)
		return
	}
	inv.handleFailure(ctx, caseID, rc, runErr)
}

// buildRequest assembles the worker execution for one item: format,
// source, OCR routing (#284: metadata default mapped to tesseract codes,
// per-case override beats it) — the invocation contract fix.sh established
// (#205/#220), now carried by RepairRequest instead of CLI args.
func buildRequest(item *RepairItem) RepairRequest {
	req := RepairRequest{AttachmentKey: item.AttachmentKey}
	if strings.Contains(item.ContentType, "epub") {
		req.Format = "epub"
		req.SourcePath = strings.TrimPrefix(item.LocalPath, "file://")
		return req
	}
	req.Language = ocrLanguage(item)
	req.OCRForce = ocrForceMode(item)
	return req
}

// ocrAnalysis is the OCR-relevant slice of a repair case's analysis JSON.
type ocrAnalysis struct {
	OCR struct {
		Mode string `json:"mode"`
		Lang string `json:"lang"`
	} `json:"ocr"`
	PaginationState string `json:"pagination_state"`
}

func parseOCRAnalysis(item *RepairItem) ocrAnalysis {
	var a ocrAnalysis
	_ = json.Unmarshal(item.Analysis, &a)
	return a
}

// ocrCase reports whether this repair item is an OCR-class case (#284).
// NOTE: a second, SQL-side predicate exists in RequeueStaleRepairCases
// (analysis ? 'ocr' — deliberately a SUPERSET so the reaper is never
// stricter than the budget selection; keep both in mind when touching
// either):
// the analysis carries the #254/#219 pagination_state marker (needs_ocr —
// the dispatcher's preflight writes it) or a per-case ocr override.
// Renaming the finding string (#283) cannot break this predicate: it keys
// on the stable English analysis field, not the operator-facing label.
func ocrCase(item *RepairItem) bool {
	a := parseOCRAnalysis(item)
	return a.PaginationState == "needs_ocr" || a.OCR.Mode != "" || a.OCR.Lang != ""
}

// ocrForceMode reports the per-case force override (#284): a broken text
// layer (Reder-class word segmentation) must be rasterized away even
// though a text layer exists. Carried as analysis.ocr.mode = "force".
func ocrForceMode(item *RepairItem) bool {
	return parseOCRAnalysis(item).OCR.Mode == "force"
}

// tesseractLang maps the Zotero document language (ISO 639-1/-2 as stored)
// onto tesseract codes. Unknown/empty falls back to the owner default
// (deu — beat deu+eng in the pilot); a stored 3-letter code passes
// through (operators may store tesseract codes directly).
func tesseractLang(docLanguage string) string {
	l := strings.ToLower(strings.TrimSpace(docLanguage))
	switch l {
	case "", "de", "ger", "deu", "de-de":
		return "deu"
	case "en", "eng", "en-us", "en-gb":
		return "eng"
	case "it", "ita":
		return "ita"
	case "es", "spa":
		return "spa"
	case "nl", "nld", "dut":
		return "nld"
	case "ru", "rus":
		return "rus"
	case "pl", "pol":
		return "pol"
	case "pt", "por":
		return "por"
	case "cs", "cze", "ces":
		return "ces"
	case "sv", "swe":
		return "swe"
	case "da", "dan":
		return "dan"
	case "fi", "fin":
		return "fin"
	case "no", "nor", "nob", "nno":
		return "nor"
	case "fr", "fra", "fre":
		return "fra"
	}
	// NO 3-letter passthrough (#286 review): the artifact bundles exactly
	// the mapped set (scripts/lib/ocr_languages.txt is the single source —
	// the build prunes tessdata to it, TestOCRLanguageAllowlistMatchesMap
	// pins the map against that file). A passthrough would promise models
	// the artifact may not carry; unknown metadata falls back to deu.
	return "deu"
}

// ocrLanguage resolves the OCR language for a case (#284): per-case
// override (analysis.ocr.lang) beats the document metadata default; both
// beat the worker's internal default (deu).
func ocrLanguage(item *RepairItem) string {
	if !ocrCase(item) {
		return ""
	}
	a := parseOCRAnalysis(item)
	if a.OCR.Lang != "" {
		return a.OCR.Lang
	}
	return tesseractLang(item.Language)
}

// repairArtifactName is the healed file the wrapper must leave under
// WorkRoot/<key>/ after a successful run — work.epub for EPUB cases.
func repairArtifactName(item *RepairItem) string {
	if strings.Contains(item.ContentType, "epub") {
		return "work.epub"
	}
	return "work.pdf"
}

// runWorker executes one case through the RepairExecutor seam. It
// returns (exit code, captured output tail, error) — the output feeds
// the HALT-terminal classification in handleSuccess (#253).
// #284/#293: OCR-class repairs run under their OWN wedge-guard budget
// (the rebuild takes as long as it takes — 10m or 10h; the backstop
// only catches a WEDGED process). The worker wrapper's own timeout
// binary stays the primary killer — the Go backstop sits ABOVE it with
// slack (AXIOM_FIX_SH_TIMEOUT, set by the local executor; same layering
// as the normal Timeout over the wrapper's 30m default).
func (inv *Invoker) runWorker(ctx context.Context, item *RepairItem) (int, string, error) {
	req := buildRequest(item)
	req.Budget = inv.cfg.Timeout
	if ocrCase(item) {
		// #293: OCR-class repairs run under their OWN wedge-guard budget
		// (the rebuild takes as long as it takes — 10m or 10h; the
		// backstop only catches a WEDGED process). The wrapper's timeout
		// binary stays the primary killer — the Go backstop sits ABOVE it
		// with 5m slack, handed down via AXIOM_FIX_SH_TIMEOUT (the
		// class-coupled env: normal-class runs pass NOTHING and the
		// wrapper's internal default applies — the pre-F08 layering
		// contract, pinned by TestRunFixerOCRBudgetEnv).
		req.Budget = inv.cfg.OCRTimeout
		inv.logger.Printf("case: key %s: OCR-class wedge-guard %s (wrapper kills at %s) — no tempo limit, orphan prevention only", item.AttachmentKey, req.Budget, fixShBudget(req.Budget))
		req.Env = append(os.Environ(), fmt.Sprintf("AXIOM_FIX_SH_TIMEOUT=%d", int(fixShBudget(req.Budget).Seconds())))
	}
	res, err := inv.executor().Execute(ctx, req)
	return res.ExitCode, res.Output, err
}

// fixShBudget is the slack-layered budget handed to the worker's own
// timeout binary (#293: the Go backstop sits ABOVE it).
func fixShBudget(budget time.Duration) time.Duration {
	fb := budget - 5*time.Minute
	if fb <= 0 {
		return budget
	}
	return fb
}

// executor resolves the execution binding: the injected RepairExecutor
// (F08 #302 — composition fake/transport bindings) or the local v1
// adapter over the configured worker command.
func (inv *Invoker) executor() RepairExecutor {
	if inv.deps.Executor != nil {
		return inv.deps.Executor
	}
	return LocalExecutor{Command: inv.cfg.Command}
}

func (inv *Invoker) handleSuccess(ctx context.Context, caseID string, item *RepairItem, out string) {
	artifact := filepath.Join(inv.cfg.WorkRoot, item.AttachmentKey, repairArtifactName(item))
	pdf, err := os.ReadFile(artifact)
	if err != nil || len(pdf) == 0 {
		// #253: exit 0 without an artifact is the worker's honest verdict
		// report. A HALT verdict terminally parks the case (reason
		// no-healable-defect-evidenced / needs-evidence) — never the old
		// endless requeue. Anything unparsable keeps the retry policy.
		if reason, ok := haltTerminalReason(out); ok {
			if terr := inv.deps.Store.MarkRepairFailed(ctx, caseID, reason); terr != nil {
				inv.logger.Printf("case %s: halt-terminal: %v", caseID, terr)
			}
			inv.logger.Printf("case %s: HALT terminally parked (%s)", caseID, reason)
			return
		}
		inv.failOrRequeue(ctx, caseID, fmt.Sprintf("worker exit 0 aber kein geheiltes Artefakt unter %s", artifact))
		return
	}
	if _, err := Apply(ctx, inv.deps.Apply, inv.deps.QuarantineRoot, ApplyCase{
		CaseID:        caseID,
		AttachmentID:  item.AttachmentID,
		AttachmentKey: item.AttachmentKey,
		DocumentKey:   item.DocumentKey,
		Title:         item.Title,
		Creators:      item.Creators,
		Year:          item.Year,
		ExistingNames: item.ExistingNames,
		SrcPath:       strings.TrimPrefix(item.LocalPath, "file://"),
		ContentType:   item.ContentType,
	}, pdf); err != nil {
		// repair.Apply already marked the case failed with the step named.
		inv.logger.Printf("case %s: apply: %v", caseID, err)
		return
	}
	inv.logger.Printf("case %s: healed (new attachment uploaded, post-heal sync follows)", caseID)
	inv.postHealSync(ctx, item)
}

// postHealSync runs the #282 auto-sync after a successful heal: a targeted
// sync (include = the healed document) enqueues the healed attachment so it
// flows into processing without operator action — the wave semantics'
// "sync → next document" step. Exactly ONE sync per heal (bounded by
// construction: one call site, called once per healed case); the sync itself
// is idempotent (content-hash dedup). A failed sync does NOT fail the heal
// — the case is already healed; the wave gate (Store.WaveRepairGate) holds
// claims until the enqueue lands and the log names the document, so the
// gap is operator-visible instead of a silent strand.
func (inv *Invoker) postHealSync(ctx context.Context, item *RepairItem) {
	if inv.deps.Sync == nil {
		inv.logger.Printf("case %s: post-heal sync disabled (no syncer wired) — healed attachment %s waits for the next sync",
			item.CaseID, item.AttachmentKey)
		return
	}
	if item.DocumentID == "" {
		inv.logger.Printf("case %s: post-heal sync skipped — no document id resolvable", item.CaseID)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// Bounded retry (#270 review: a transient Zotero outage or lock wait
	// reproduced the original stranding). Three attempts, short backoff —
	// the sync is idempotent, and the wave gate keeps the healed case
	// visible until an enqueue lands (or the 1h window passes).
	var res sync.Result
	var err error
	const attempts = 3
	for i := 1; i <= attempts; i++ {
		res, err = inv.deps.Sync.Run(sctx, &sync.SyncOverride{Include: []string{item.DocumentID}})
		if err == nil {
			break
		}
		inv.logger.Printf("case %s: post-heal sync attempt %d/%d FAILED (document %s): %v",
			item.CaseID, i, attempts, item.DocumentID, err)
		if i < attempts {
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
		}
	}
	if err != nil {
		inv.logger.Printf("case %s: post-heal sync FAILED after %d attempts (document %s) — healed attachment %s not enqueued; wave gate holds: %v",
			item.CaseID, attempts, item.DocumentID, item.AttachmentKey, err)
		return
	}
	if res.Enqueued == 0 {
		// 0 is NOT success for this call: the entire point is the healed
		// document's re-enqueue (selection boundaries can suppress it —
		// loud, named, and the gate bounds the strand to its 1h window).
		inv.logger.Printf("case %s: post-heal sync enqueued 0 jobs for document %s (selection boundary or unchanged hash) — healed attachment %s waits for the next sync; wave gate holds ≤1h",
			item.CaseID, item.DocumentID, item.AttachmentKey)
		return
	}
	inv.logger.Printf("case %s: post-heal sync ok — document %s enqueued %d job(s) (#282)",
		item.CaseID, item.DocumentID, res.Enqueued)
}

func (inv *Invoker) handleFailure(ctx context.Context, caseID string, rc int, runErr error) {
	inv.failOrRequeue(ctx, caseID, fmt.Sprintf("worker: %v", runErr))
}

// haltTerminalReason extracts the worker's report verdict from the captured
// output (the repair_agent prints its JSON report as the LAST thing on
// stdout) and classifies a HALT terminally (#253):
//
//   - verdict != "halt" (or no parsable report)  → not terminal ("", false)
//   - halt with an unmeasurability ground         → needs-evidence:
//     stelle1_druckseite — … (the M-source was unmeasurable; worth a
//     retry once evidence/tooling changes — #278)
//   - halt with another runtime evidence gap      → needs-evidence: …
//   - every other halt                            → no-healable-defect-evidenced: …
//
// #278: the reason LEADS with final_step.reason — the model's
// evidence-bound escalation ground. The old first-match scan over
// `unproven` surfaced the STATIC stelle3 declaration ("nicht prüfbare"
// prose of a slot nothing reads) while the actual ground was Stelle 1
// (unmeasurable folio signal) — three production parks sent the operator
// down a false trail, including an owner-supplied annotation the code
// could not consume. The unproven scan survives only as the fallback when
// the model gave no reason, and only over RUNTIME findings (stelle3 is
// honestly NOT IMPLEMENTED in probe output since #278 and no longer
// matches).
//
// The reason is surfaced verbatim for the outcome API (#252).
func haltTerminalReason(out string) (string, bool) {
	// the report is the last JSON object in the output — find the last '{'
	// whose suffix parses (logs precede it)
	for i := strings.LastIndex(out, "{"); i >= 0; i = strings.LastIndex(out[:i], "{") {
		var report struct {
			Verdict  string   `json:"verdict"`
			Unproven []string `json:"unproven"`
			// final_step.reason is the model's evidence-bound escalation
			// ground — the ONLY final_step field the classification reads
			// (the action is stop/escalate/report, all halts here).
			FinalStep struct {
				Reason string `json:"reason"`
			} `json:"final_step"`
		}
		if err := json.Unmarshal([]byte(out[i:]), &report); err != nil {
			continue
		}
		if report.Verdict != "halt" {
			return "", false
		}
		ground := firstLine(strings.TrimSpace(report.FinalStep.Reason))
		lg := strings.ToLower(ground)
		if ground != "" {
			// #284 review: a TIMEOUT is transient (long rebuild under a
			// short budget, slow OCR) — the case requeues while attempts
			// remain instead of parking terminally. Not-terminal ("", false)
			// routes into failOrRequeue.
			if strings.Contains(lg, "timeout") {
				return "", false
			}
			// unmeasurability is always a Stelle-1 statement: stelle2/3 are
			// corroborating witnesses that never gate the diagnosis (truth
			// ordering) — the prefix attributes the ground honestly.
			// "kein messbares folio" (not the broader "kein messbar") so a
			// STOP report echoing the system prompt's stop guidance („wenn
			// kein messbares Heilungspotenzial vorliegt, schließe mit stop")
			// does NOT classify as a stelle1 evidence gap.
			if strings.Contains(lg, "nicht messbar") ||
				strings.Contains(lg, "kein messbares folio") ||
				strings.Contains(lg, "unmessbar") ||
				strings.Contains(lg, "unmeasurable") {
				return "needs-evidence: stelle1_druckseite — " + ground, true
			}
			// #284: the scan_ocr_rebuild rule names missing OCR tooling
			// explicitly as an evidence gap — park as recoverable, not as
			// an unrepairable defect (installing the toolchain changes the
			// evidence; the requeue route re-arms).
			if strings.Contains(lg, "needs-evidence") ||
				strings.Contains(lg, "ocr-werkzeuge fehlen") {
				return "needs-evidence: " + ground, true
			}
			if strings.Contains(lg, "nicht erreichbar") ||
				strings.Contains(lg, "nicht prüfbar") ||
				strings.Contains(lg, "offene stelle") {
				return "needs-evidence: " + ground, true
			}
			return "no-healable-defect-evidenced: " + ground, true
		}
		// fallback (model gave no reason): first RUNTIME evidence gap in
		// unproven leads — the pre-#278 behavior
		for _, u := range report.Unproven {
			lu := strings.ToLower(u)
			if strings.Contains(lu, "nicht erreichbar") ||
				strings.Contains(lu, "nicht prüfbar") ||
				strings.Contains(lu, "offene stelle") {
				return "needs-evidence: " + firstLine(u), true
			}
		}
		return "no-healable-defect-evidenced: " + report.Verdict, true
	}
	return "", false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (inv *Invoker) failOrRequeue(ctx context.Context, caseID, reason string) {
	status, err := inv.deps.Store.FailOrRequeueRepairCase(ctx, caseID, reason, 0)
	if err != nil {
		inv.logger.Printf("case %s: fail-or-requeue: %v", caseID, err)
		return
	}
	switch status {
	case RepairQueued:
		inv.logger.Printf("case %s: failed → requeued for retry (%s)", caseID, reason)
	case RepairFailed:
		inv.logger.Printf("case %s: failed terminally, parked for dudu (%s)", caseID, reason)
	}
}
