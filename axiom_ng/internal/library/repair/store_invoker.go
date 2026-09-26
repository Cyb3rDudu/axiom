// store_invoker.go — queue-side store methods beyond the #184 state
// machine. F08 #302: moved verbatim from internal/repo/repair_invoker.go
// (born as #206 fixer invoker support) into the Library-owned repair
// package. Three additions:
//
//   - RepairCaseItem: the Zotero coordinates + metadata of a case's
//     attachment (the JOIN the custody sequence and the schema filename
//     are built from).
//   - RequeueStaleRepairCases: lease recovery. The invoker claims
//     queued → in_repair and runs fix.sh; if the invoker dies mid-case
//     the case would sit in in_repair forever (the documented B3
//     limitation). A case whose updated_at is older than the worker's
//     hard runtime window is stale — its claim died with the invoker, so
//     it goes back to queued. The loop guard still caps total attempts.
//   - FailOrRequeueRepairCase: the retry policy. A failed worker run
//     requeues while case attempts remain, else parks the case failed
//     with a clear reason (dudu reads it) — escalation is the loop
//     guard's blocked_for_dudu on the NEXT claim of a retried case.
package repair

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

// RepairItem is everything the orchestrator needs to know about a case's
// attachment: the Zotero keys (invocation key + apply target), the source
// pdf path, and the metadata the schema filename is built from.
type RepairItem struct {
	CaseID        string
	AttachmentID  string
	AttachmentKey string
	DocumentKey   string
	DocumentID    string // #282: targets the post-heal sync's include override
	Title         string
	Creators      []zoteroprovider.Creator
	Year          int
	ExistingNames []string        // #291: the document's current attachment filenames (grown-pattern reference)
	Language      string          // #284: OCR language default from document metadata
	Analysis      json.RawMessage // #284: per-case OCR overrides (analysis.ocr.mode/lang)
	LocalPath     string
	ContentType   string
}

// ExistingNamesSubquery aggregates the document's current attachment
// filenames for the #291 grown-pattern reference (preferred first, then
// filename ASC — deterministic and explainable instead of UUID order;
// the single source so the repair orchestrator, verdict-apply and custody
// paths can never drift).
const ExistingNamesSubquery = `(SELECT array_agg(a2.filename ORDER BY a2.preferred DESC, a2.filename ASC)
		        FROM zotero_attachments a2
		        WHERE a2.document_id = d.id AND a2.deleted = false
		          AND COALESCE(a2.filename, '') <> '')`

// RepairCaseItem resolves a repair case to its attachment coordinates.
// Returns pgx.ErrNoRows when the attachment or document row is gone at the
// source — the caller parks such a case (mirror of the W3a queue rule).
func (s *Store) RepairCaseItem(ctx context.Context, caseID string) (*RepairItem, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT c.id::text, a.id::text, a.zotero_key, d.zotero_key, d.id::text,
		       d.title, d.creators, COALESCE(d.publication_year, 0),
		       COALESCE(d.language, ''), c.analysis,
		       a.local_path, COALESCE(a.content_type, ''),
		       `+ExistingNamesSubquery+`
		FROM repair_cases c
		JOIN zotero_attachments a ON a.id = c.attachment_id AND a.deleted = false
		JOIN zotero_documents d ON d.id = a.document_id
		WHERE c.id = $1`, caseID)
	var it RepairItem
	var creators []byte
	if err := row.Scan(&it.CaseID, &it.AttachmentID, &it.AttachmentKey, &it.DocumentKey, &it.DocumentID,
		&it.Title, &creators, &it.Year, &it.Language, &it.Analysis,
		&it.LocalPath, &it.ContentType, &it.ExistingNames); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(creators, &it.Creators)
	return &it, nil
}

// RequeueStaleRepairCases flips in_repair cases older than their class's
// stale bound back to queued (invoker crash recovery). Returns the number
// of requeued cases. Attempts were already counted at claim time — the
// loop guard still caps the total, so a crash-looping invoker cannot mint
// infinite attempts.
// #284: OCR-class cases run under a LARGER budget (658-page rebuilds);
// reaping them at the normal bound would requeue a live, merely slow OCR
// run under a second claim (the per-key lockdir then burns an attempt).
// Class predicate = the stable analysis fields (pagination_state marker
// or the per-case ocr override), not the operator-facing finding string.
func (s *Store) RequeueStaleRepairCases(ctx context.Context, stale, ocrStale time.Duration) (int64, error) {
	if ocrStale < stale {
		ocrStale = stale
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE repair_cases SET status='queued', updated_at=now()
		WHERE status='in_repair' AND updated_at < now() - make_interval(secs => 
			CASE WHEN analysis->>'pagination_state' = 'needs_ocr' OR analysis ? 'ocr'
			     THEN $2::float8 ELSE $1::float8 END)`, stale.Seconds(), ocrStale.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FailOrRequeueRepairCase closes a failed worker execution according to
// the retry policy: while the case has attempts left (below maxAttempts,
// default RepairMaxAttempts), it goes back to queued for one more run;
// otherwise it is parked failed with the reason. Returns the effective
// status so callers can log the transition. The 0-rows case (case no
// longer in_repair — closed elsewhere) is NOT an error.
func (s *Store) FailOrRequeueRepairCase(ctx context.Context, caseID, reason string, maxAttempts int) (RepairStatus, error) {
	if maxAttempts <= 0 {
		maxAttempts = RepairMaxAttempts
	}
	var status string
	err := s.pool.QueryRow(ctx, `
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
