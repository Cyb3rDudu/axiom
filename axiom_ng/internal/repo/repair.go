// #184 — repair queue: state machine + loop guard.
//
// rejected → queued → in_repair → healed | failed | blocked_for_dudu
//
// Loop guard (design nail 1): zotero_attachments.repair_attempts counts
// EVERY claim per attachment; the third attempt is impossible by check —
// the case goes blocked_for_dudu('loop-guard') and never enters the loop.
// 'unpaginiert' originals never enter the loop: QueueRepairCase refuses
// them (their rejected case stays as a dudu-visible tombstone).
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
// Contract: returns (nil, nil) when the insert conflicted AND the competing
// case is already closed (closed between the ON CONFLICT and the re-read) —
// callers MUST nil-check the case before use.
func (r *Repo) CreateRepairCase(ctx context.Context, attachmentID, documentID, suspicionClass string, analysis json.RawMessage) (*RepairCase, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO repair_cases (attachment_id, document_id, suspicion_class, analysis, status)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, 'rejected')
		ON CONFLICT (attachment_id) WHERE status IN ('rejected','queued','in_repair') DO NOTHING
		RETURNING `+repairCaseCols,
		attachmentID, documentID, suspicionClass, analysis)
	c, err := scanRepairCase(row)
	if err == nil {
		return c, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return r.OpenRepairCase(ctx, attachmentID)
	}
	return nil, err
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
// rejected → queued. Unpaginiert originals NEVER queue (design nail 1:
// "Klasse unreparierbar → nie in der Schleife") — enforced here, not in
// comments.
func (r *Repo) QueueRepairCase(ctx context.Context, caseID, suspicionClass string, analysis json.RawMessage) error {
	if strings.Contains(strings.ToLower(suspicionClass), "unpaginiert") {
		return fmt.Errorf("unpaginierte Originale gehen nie in die Reparatur-Schleife (case %s)", caseID)
	}
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

// AuditWrite records every Zotero mutation (Was/Wann/Warum).
func (r *Repo) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	d, _ := json.Marshal(detail)
	_, err := r.pool.Exec(ctx, `
		INSERT INTO zotero_write_audit (case_id, attachment_id, action, detail)
		VALUES (NULLIF($1,'')::uuid, NULLIF($2,'')::uuid, $3, $4)`,
		caseID, attachmentID, action, d)
	return err
}
