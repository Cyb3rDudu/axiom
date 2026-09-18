// #184 — repair queue: state machine + loop guard.
//
// rejected → queued → in_repair → healed | failed | blocked_for_dudu
//
// Loop guard (design nail 1): zotero_attachments.repair_attempts counts
// EVERY claim per attachment; the third attempt is impossible by check —
// the case goes blocked_for_dudu('loop-guard') and never enters the loop.
// #284: scan-class cases (historically "unpaginiert", now repairable via
// scan_ocr_rebuild) queue like any repairable class — the old refusal is
// gone; loop safety is the claim guard + the document-level healed-count
// guard on the dispatcher's auto-queue path (#282).
//
// Foundation limitation (B3): in_repair has NO reaper/timeout yet — a
// fix-service crash mid-case burns that attempt and leaves the case stuck
// in in_repair until wiring adds a reaper; the status-guarded transitions
// below already refuse double-closing, so nothing corrupts, it just waits.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const RepairMaxAttempts = 2

// RepairAutoApplyMinScore is the RAG-side auto-apply threshold (#184 gate
// hierarchy): footer-verification coverage must reach it with ZERO
// contradictions, else the case goes to dudu.
const RepairAutoApplyMinScore = 0.95

type RepairStatus string

const (
	RepairRejected RepairStatus = "rejected"
	RepairQueued   RepairStatus = "queued"
	RepairInRepair RepairStatus = "in_repair"
	RepairHealed   RepairStatus = "healed"
	RepairFailed   RepairStatus = "failed"
	RepairBlocked  RepairStatus = "blocked_for_dudu"
)

// RepairCase is one repair flow per attachment.
type RepairCase struct {
	ID                  string          `json:"id"`
	AttachmentID        string          `json:"attachment_id"`
	DocumentID          string          `json:"document_id"`
	Status              RepairStatus    `json:"status"`
	Attempts            int             `json:"attempts"`
	SuspicionClass      string          `json:"suspicion_class"`
	Analysis            json.RawMessage `json:"analysis"`
	Plan                json.RawMessage `json:"plan,omitempty"`
	PlanVersion         int             `json:"plan_version"`
	VerifyScore         float64         `json:"verify_score"`
	VerifyContradiction int             `json:"verify_contradictions"`
	Verdict             string          `json:"verdict,omitempty"`
	BlockedReason       string          `json:"blocked_reason,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// repairCaseCols is the canonical column list for repair_cases reads — ONE
// definition so the four read paths can never drift apart (B4).
const repairCaseCols = `id::text, attachment_id::text, COALESCE(document_id::text,''), status::text, attempts,
	suspicion_class, analysis, plan_version, verify_score, verify_contradictions,
	verdict, blocked_reason, created_at, updated_at`

// scanRepairCase fills a RepairCase from any row-like (QueryRow or Rows),
// mirroring scanKGEntities in kg.go.
func scanRepairCase(sc interface{ Scan(dest ...any) error }) (*RepairCase, error) {
	var c RepairCase
	if err := sc.Scan(&c.ID, &c.AttachmentID, &c.DocumentID, &c.Status, &c.Attempts,
		&c.SuspicionClass, &c.Analysis, &c.PlanVersion, &c.VerifyScore, &c.VerifyContradiction,
		&c.Verdict, &c.BlockedReason, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateRepairCase opens a case for a preflight-rejected attachment.
// Idempotent per attachment: an existing OPEN case is returned unchanged.
//
// Contract: returns (nil, false, nil) when the insert conflicted AND the
// competing case is already closed (closed between the ON CONFLICT and the
// re-read) — callers MUST nil-check the case before use. The created flag
// distinguishes a FRESH case from a recycled open one; #238 auto-queue
// relies on it so a stale open case of an auto-queueable class can never
// be queued by a NEWER verdict of a different class.
func (r *Repo) CreateRepairCase(ctx context.Context, attachmentID, documentID, suspicionClass string, analysis json.RawMessage) (*RepairCase, bool, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO repair_cases (attachment_id, document_id, suspicion_class, analysis, status)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, 'rejected')
		ON CONFLICT (attachment_id) WHERE status IN ('rejected','queued','in_repair') DO NOTHING
		RETURNING `+repairCaseCols,
		attachmentID, documentID, suspicionClass, analysis)
	c, err := scanRepairCase(row)
	if err == nil {
		return c, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		c, err := r.OpenRepairCase(ctx, attachmentID)
		return c, false, err
	}
	return nil, false, err
}

// OpenRepairCase fetches the open case of an attachment (nil if none).
func (r *Repo) OpenRepairCase(ctx context.Context, attachmentID string) (*RepairCase, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT `+repairCaseCols+`
		FROM repair_cases WHERE attachment_id=$1 AND status IN ('rejected','queued','in_repair')`,
		attachmentID)
	c, err := scanRepairCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// QueueRepairCase attaches the fix-service input (analysis) and flips
// rejected → queued. The historical "unpaginiert never queues" refusal is
// GONE (#284): the scan class became repairable — scan_ocr_rebuild heals
// textless scans AND broken text layers, so their cases now enter the
// loop like any repairable class. Loop safety is owned by the claim guard
// (per attachment) and the dispatcher's document-level healed-count guard
// (#282) — not by refusing the class.
func (r *Repo) QueueRepairCase(ctx context.Context, caseID, suspicionClass string, analysis json.RawMessage) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE repair_cases SET status='queued', suspicion_class=$2, analysis=$3, updated_at=now()
		WHERE id=$1 AND status='rejected'`, caseID, suspicionClass, analysis)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("case %s nicht in rejected", caseID)
	}
	return nil
}

// ListRepairQueue returns queued cases for the fix-service poll.
func (r *Repo) ListRepairQueue(ctx context.Context) ([]RepairCase, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+repairCaseCols+`
		FROM repair_cases WHERE status='queued' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepairCase
	for rows.Next() {
		c, err := scanRepairCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ClaimRepairCase moves queued → in_repair and enforces the loop guard:
// attempts (per attachment, across cases) may not exceed RepairMaxAttempts.
func (r *Repo) ClaimRepairCase(ctx context.Context, caseID string) (*RepairCase, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var attempts int
	if err := tx.QueryRow(ctx, `
		SELECT repair_attempts FROM zotero_attachments a
		JOIN repair_cases c ON c.attachment_id = a.id
		WHERE c.id=$1 FOR UPDATE OF a`, caseID).Scan(&attempts); err != nil {
		return nil, err
	}
	if attempts >= RepairMaxAttempts {
		if _, err := tx.Exec(ctx, `
			UPDATE repair_cases SET status='blocked_for_dudu', blocked_reason='loop-guard', updated_at=now()
			WHERE id=$1 AND status IN ('queued','rejected')`, caseID); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("case %s: loop-guard (attachment bereits %d× repariert)", caseID, attempts)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE zotero_attachments SET repair_attempts = repair_attempts + 1, updated_at=now()
		WHERE id = (SELECT attachment_id FROM repair_cases WHERE id=$1)`, caseID); err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE repair_cases SET status='in_repair', attempts = attempts + 1, updated_at=now()
		WHERE id=$1 AND status='queued'`, caseID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("case %s nicht in queued", caseID)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.getRepairCase(ctx, caseID)
}

func (r *Repo) getRepairCase(ctx context.Context, caseID string) (*RepairCase, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT `+repairCaseCols+`
		FROM repair_cases WHERE id=$1`, caseID)
	return scanRepairCase(row)
}

// SubmitRepairVerdict stores the judge result. The AUTO-APPLY gate is
// enforced HERE (RAG side), not trusted from the service: score >= 0.95 AND
// zero contradictions AND verdict == auto_apply. Everything else blocks.
// Returns the effective status so the caller knows whether to apply writes.
//
// TRUST BOUNDARY (documented, accepted by #184's design): score and
// contradictions are SERVICE-ATTESTED — the mechanical footer verification
// runs in the fix-service, not here. Blast radius of a lying service is
// bounded: loop guard (max 2), quarantine keeps the original, healed needs
// the next preflight GREEN, every mutation audited. Re-verification RAG-side
// is a possible later hardening, not part of the nail.
func (r *Repo) SubmitRepairVerdict(ctx context.Context, caseID string, plan json.RawMessage, planVersion int, score float64, contradictions int, verdict, blockedReason string) (RepairStatus, error) {
	effective := RepairBlocked
	if verdict == "auto_apply" && score >= RepairAutoApplyMinScore && contradictions == 0 {
		effective = RepairInRepair // stays in_repair; the caller now applies writes, then MarkHealed
	} else if verdict == "failed" {
		effective = RepairFailed
	}
	reason := blockedReason
	if effective == RepairBlocked && reason == "" {
		// every blocked case carries a reason — dudu reads WHY (review C2:
		// unknown verdicts used to land blocked with an empty reason)
		if verdict == "auto_apply" {
			reason = fmt.Sprintf("auto-apply-gate: score=%.3f widersprüche=%d (Schwelle %.2f/0)", score, contradictions, RepairAutoApplyMinScore)
		} else {
			reason = fmt.Sprintf("verdict %q unterhalb des auto-apply-gates: score=%.3f widersprüche=%d", verdict, score, contradictions)
		}
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE repair_cases SET plan=$2, plan_version=$3, verify_score=$4, verify_contradictions=$5,
			verdict=$6, blocked_reason=$7,
			status = $8::repair_status,
			updated_at=now()
		WHERE id=$1 AND status='in_repair'`,
		caseID, plan, planVersion, score, contradictions, verdict, reason, string(effective))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", fmt.Errorf("case %s nicht in in_repair", caseID)
	}
	return effective, nil
}

// RequeueRepairCase is the loop-guard reset route (#278): re-arms a
// PARKED case (failed/blocked_for_dudu) — and since #284 also a rejected
// manual-track case — for a fresh attempt without DB surgery. Evidence
// conditions can change under a parked case — new fixer tooling (the
// #278 forensics completeness fix, the #284 OCR force mode), a manual
// repair, or newly available evidence. The operator decides and
// documents WHY (reason is mandatory, lands in the audit trail); the
// route makes the decision cheap and reversible: zotero_attachments.
// repair_attempts → 0, case → queued, blocked_reason cleared. in_repair
// REFUSES (mid-flight cases are never touched from outside — same nail
// as BlockRepairCase); healed refuses too (nothing to redo — a new
// suspicion opens a new case).
// analysisPatch (#284 review): an optional JSON object merged into the
// case's analysis (jsonb ||) — the per-case override surface for OCR
// routing (e.g. {"ocr": {"mode": "force", "lang": "eng"}} for the
// broken-text-layer class: the invoker keys its budget and --ocr-mode on
// exactly these fields). The patch is audited with the reason. NOTE:
// jsonb || is a SHALLOW merge — a patched "ocr" object REPLACES any
// existing one, so patches must carry the complete override object
// (mode AND lang together), not single keys.
func (r *Repo) RequeueRepairCase(ctx context.Context, caseID, reason string, analysisPatch json.RawMessage) error {
	return r.requeueRepairCase(ctx, caseID, reason, analysisPatch, "")
}

// RequeueRepairCaseWithOrphanAck is the #285-guarded requeue: a case with
// an UNRESOLVED ambiguous-create orphan (repair.Apply audited
// create_attachment_orphan — item minted in Zotero, upload failed, cleanup
// delete failed too) refuses the requeue until the operator names the
// orphan key in orphanAck, confirming the EMPTY item was deleted in Zotero.
// A blind re-run would mint a second sibling attachment while the orphan
// survives only in human-readable reason text — the same hazard class the
// manual custody endpoint guards with its 409. Resolution is audited as
// create_attachment_orphan_resolved (machine-readable on the same table).
func (r *Repo) RequeueRepairCaseWithOrphanAck(ctx context.Context, caseID, reason string, analysisPatch json.RawMessage, orphanAck string) error {
	return r.requeueRepairCase(ctx, caseID, reason, analysisPatch, orphanAck)
}

func (r *Repo) requeueRepairCase(ctx context.Context, caseID, reason string, analysisPatch json.RawMessage, orphanAck string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("requeue braucht einen Grund (geänderte Beweislage dokumentieren)")
	}
	if len(analysisPatch) == 0 {
		analysisPatch = json.RawMessage(`{}`)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// #285 ambiguous-create guard: refuse while an unresolved orphan item
	// exists — the operator must delete the EMPTY item in Zotero and ack
	// its key (the audit row names it; the refusal text repeats it).
	// Note on the row comparison below: zotero_write_audit.id is
	// gen_random_uuid() — it is NOT a sequence, it only breaks exact
	// created_at ties. Resolution rows are always written strictly later
	// in every reachable flow, so a tie can at worst re-arm the guard
	// (fail-safe direction: refusal, not sibling minting).
	orphan, resolved, err := func() (string, bool, error) {
		row := tx.QueryRow(ctx, `
			WITH latest AS (
				SELECT COALESCE(detail->>'new_zotero_key','') AS k, created_at, id
				FROM zotero_write_audit
				WHERE case_id=$1::uuid AND action='create_attachment_orphan'
				ORDER BY created_at DESC, id DESC LIMIT 1)
			SELECT l.k, EXISTS (
					SELECT 1 FROM zotero_write_audit a
					WHERE a.case_id=$1::uuid AND a.action='create_attachment_orphan_resolved'
					  AND a.detail->>'new_zotero_key'=l.k
					  AND (a.created_at, a.id) > (l.created_at, l.id))
			FROM latest l`, caseID)
		var k string
		var res bool
		if err := row.Scan(&k, &res); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", false, nil
			}
			return "", false, err
		}
		return k, res, nil
	}()
	if err != nil {
		return err
	}
	if orphan != "" && !resolved {
		if orphanAck != orphan {
			return fmt.Errorf("case %s hat einen abgebrochenen Create-Lauf: leeres Anhang-Item %s in Zotero löschen und mit orphan_resolved='%s' bestätigen — ein blinder Re-Run würde ein zweites leeres Geschwister erzeugen", caseID, orphan, orphan)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO zotero_write_audit (case_id, attachment_id, action, detail)
			SELECT $1::uuid, attachment_id, 'create_attachment_orphan_resolved', $2::jsonb
			FROM repair_cases WHERE id=$1`, caseID, mustMarshal(map[string]any{"new_zotero_key": orphan, "acked_by": reason})); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE zotero_attachments SET repair_attempts = 0, updated_at=now()
		WHERE id = (SELECT attachment_id FROM repair_cases WHERE id=$1)`, caseID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("case %s: attachment nicht gefunden", caseID)
	}
	tag, err = tx.Exec(ctx, `
		UPDATE repair_cases SET status='queued', blocked_reason='',
		    analysis = COALESCE(analysis, '{}'::jsonb) || $2::jsonb,
		    updated_at=now()
		WHERE id=$1 AND status IN ('failed','blocked_for_dudu','rejected')`, caseID, analysisPatch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("case %s nicht geparkt (failed/blocked_for_dudu/rejected)", caseID)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO zotero_write_audit (case_id, attachment_id, action, detail)
		SELECT $1::uuid, attachment_id, 'repair-requeue', $2::jsonb
		FROM repair_cases WHERE id=$1`, caseID, mustMarshal(map[string]any{"reason": reason, "analysis_patch": json.RawMessage(analysisPatch)})); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func mustMarshal(v any) []byte {
	d, _ := json.Marshal(v)
	return d
}

// MarkRepairHealed / MarkRepairFailed close the case after the writes
// (healed is confirmed by the NEXT preflight GREEN — the loop checks itself;
// healed here means "applied, awaiting proof").
func (r *Repo) MarkRepairHealed(ctx context.Context, caseID string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE repair_cases SET status='healed', updated_at=now() WHERE id=$1 AND status='in_repair'`, caseID)
	if err != nil || tag.RowsAffected() == 0 {
		if err == nil {
			err = fmt.Errorf("case %s nicht in in_repair", caseID)
		}
		return err
	}
	return nil
}

// BlockRepairCase parks a queued/rejected case as blocked_for_dudu — used
// by the queue listing when the attachment no longer exists at the source:
// without this the case stays queued forever and every poll re-serves it
// (review W3a). in_repair REFUSES the block (design nail: a mid-flight
// case is never touched from outside) — callers must resolve the item
// BEFORE claiming.
// Mirrors the loop-guard UPDATE shape.
func (r *Repo) BlockRepairCase(ctx context.Context, caseID, reason string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE repair_cases SET status='blocked_for_dudu', blocked_reason=$2, updated_at=now()
		WHERE id=$1 AND status IN ('queued','rejected')`, caseID, reason)
	if err != nil || tag.RowsAffected() == 0 {
		if err == nil {
			err = fmt.Errorf("case %s nicht in queued/rejected", caseID)
		}
		return err
	}
	return nil
}

func (r *Repo) MarkRepairFailed(ctx context.Context, caseID, reason string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE repair_cases SET status='failed', blocked_reason=$2, updated_at=now() WHERE id=$1 AND status='in_repair'`, caseID, reason)
	if err != nil || tag.RowsAffected() == 0 {
		if err == nil {
			err = fmt.Errorf("case %s nicht in in_repair", caseID)
		}
		return err
	}
	return nil
}

// DocumentHealedCases counts HEALED repair cases of a document (#282 loop
// binding): every heal replaces the attachment (fresh repair_attempts
// counter), so the per-attachment guard cannot bound the
// heal→sync→re-reject→heal cycle across generations. The document-level
// count can. Auto-queueing (dispatcher) refuses beyond RepairMaxAttempts
// healed cases on the same document — the manual queue path stays
// operator-governed.
func (r *Repo) DocumentHealedCases(ctx context.Context, documentID string) (int, error) {
	if documentID == "" {
		return 0, nil
	}
	var n int
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM repair_cases WHERE document_id=$1::uuid AND status='healed'`, documentID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// WaveRepairGate reports whether the repair-included wave holds the claim
// gate (#282 owner semantics: dry-run → repair if needed → sync → next
// document; skip only when unrepairable). The gate closes while a repair
// loop-back is still draining:
//
//   - queued / in_repair: a fixer heal is pending or running (the invoker
//     runs the post-heal sync before releasing the loop — see #282);
//   - healed within the last hour with NO ingest job enqueued for the
//     document since the heal: the post-heal sync has not landed yet (in
//     flight, failed, or the invoker died between Apply and sync). The
//     window is bounded so historical healed cases never gate; a stranded
//     heal OLDER than the window stops gating (operator-visible in the
//     invoker log instead of a silent wave stall).
//
// Terminal parks (failed / blocked_for_dudu) and manual-track rejected
// cases NEVER gate — an unrepairable document must not block the wave.
// Observer-only: this never marks jobs, it only defers claiming.
func (r *Repo) WaveRepairGate(ctx context.Context) (bool, string, error) {
	var open, stranded int
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status IN ('queued','in_repair')),
		       count(*) FILTER (WHERE status='healed' AND updated_at > now() - interval '1 hour'
		         AND NOT EXISTS (
		           SELECT 1 FROM ingest_jobs j
		           JOIN zotero_attachments a ON a.id = j.attachment_id
		           WHERE a.document_id = repair_cases.document_id
		             AND j.enqueued_at >= repair_cases.updated_at))
		FROM repair_cases`).Scan(&open, &stranded); err != nil {
		return false, "", err
	}
	if open > 0 {
		return true, fmt.Sprintf("%d repair case(s) queued/in_repair", open), nil
	}
	if stranded > 0 {
		return true, fmt.Sprintf("%d healed case(s) not yet enqueued (post-heal sync pending)", stranded), nil
	}
	return false, "", nil
}

// AuditWrite records every Zotero mutation (Was/Wann/Warum).
func (r *Repo) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	d := mustMarshal(detail)
	_, err := r.pool.Exec(ctx, `
		INSERT INTO zotero_write_audit (case_id, attachment_id, action, detail)
		VALUES (NULLIF($1,'')::uuid, NULLIF($2,'')::uuid, $3, $4)`,
		caseID, attachmentID, action, d)
	return err
}
