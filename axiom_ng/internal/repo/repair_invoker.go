// repair_invoker.go — #206 fixer invoker support: the queue-side methods
// the invoker needs beyond the #184 state machine. Three additions:
//
//   - RepairCaseItem: the Zotero coordinates + metadata of a case's
//     attachment (the JOIN the custody sequence and the schema filename
//     are built from).
//   - RequeueStaleRepairCases: lease recovery. The invoker claims
//     queued → in_repair and runs fix.sh; if the invoker dies mid-case
//     the case would sit in in_repair forever (the documented B3
//     limitation). A case whose updated_at is older than the fixer's
//     hard runtime window is stale — its claim died with the invoker, so
//     it goes back to queued. The loop guard still caps total attempts.
//   - FailOrRequeueRepairCase: the retry policy. A failed fixer run
//     requeues while case attempts remain, else parks the case failed
//     with a clear reason (dudu reads it) — escalation is the loop
//     guard's blocked_for_dudu on the NEXT claim of a retried case.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

// RepairItem is everything the fixer invoker needs to know about a case's
// attachment: the Zotero keys (invocation key + apply target), the source
// pdf path, and the metadata the schema filename is built from.
type RepairItem struct {
	CaseID        string
	AttachmentID  string
	AttachmentKey string
	DocumentKey   string
	Title         string
	Creators      []zotero.Creator
	Year          int
	Publisher     string
	LocalPath     string
	ContentType   string
}

// RepairCaseItem resolves a repair case to its attachment coordinates.
// Returns pgx.ErrNoRows when the attachment or document row is gone at the
// source — the caller parks such a case (mirror of the W3a queue rule).
func (r *Repo) RepairCaseItem(ctx context.Context, caseID string) (*RepairItem, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT c.id::text, a.id::text, a.zotero_key, d.zotero_key,
		       d.title, d.creators, COALESCE(d.publication_year, 0), COALESCE(d.publisher, ''),
		       a.local_path, COALESCE(a.content_type, '')
		FROM repair_cases c
		JOIN zotero_attachments a ON a.id = c.attachment_id AND a.deleted = false
		JOIN zotero_documents d ON d.id = a.document_id
		WHERE c.id = $1`, caseID)
	var it RepairItem
	var creators []byte
	if err := row.Scan(&it.CaseID, &it.AttachmentID, &it.AttachmentKey, &it.DocumentKey,
		&it.Title, &creators, &it.Year, &it.Publisher, &it.LocalPath,
		&it.ContentType); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(creators, &it.Creators)
	return &it, nil
}

// RequeueStaleRepairCases flips in_repair cases older than stale back to
// queued (invoker crash recovery). Returns the number of requeued cases.
// Attempts were already counted at claim time — the loop guard still caps
// the total, so a crash-looping invoker cannot mint infinite attempts.
func (r *Repo) RequeueStaleRepairCases(ctx context.Context, stale time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE repair_cases SET status='queued', updated_at=now()
		WHERE status='in_repair' AND updated_at < now() - make_interval(secs => $1)`, stale.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FailOrRequeueRepairCase closes a failed fixer invocation according to
// the retry policy: while the case has attempts left (below maxAttempts,
// default RepairMaxAttempts), it goes back to queued for one more run;
// otherwise it is parked failed with the reason. Returns the effective
// status so callers can log the transition. The 0-rows case (case no
// longer in_repair — closed elsewhere) is NOT an error.
func (r *Repo) FailOrRequeueRepairCase(ctx context.Context, caseID, reason string, maxAttempts int) (RepairStatus, error) {
	if maxAttempts <= 0 {
		maxAttempts = RepairMaxAttempts
	}
	var status string
	err := r.pool.QueryRow(ctx, `
		UPDATE repair_cases
		SET status = CASE WHEN attempts < $2 THEN 'queued' ELSE 'failed' END::repair_status,
		    blocked_reason = $3, updated_at = now()
		WHERE id = $1 AND status='in_repair'
		RETURNING status::text`, caseID, maxAttempts, reason).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return RepairStatus(status), nil
}
