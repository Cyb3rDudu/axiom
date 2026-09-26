// manual.go — #279 manual repair custody: the quarantine-first protocol for
// librarian-repaired files. The fixer's auto-heal path (apply.go) runs the
// SAME custody sequence via ApplyDeps; ManualDeps reuses repair.Apply itself
// so the ordering can never drift between the automatic and the manual path
// (the W4 nail), replacing only the case bookkeeping: a manual repair has no
// repair_cases row — its record is a JSON file in the quarantine area
// (original path, attachment key, reason, date + every completed step).
package repair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

// ManualStep is one completed custody action of one run (audit-equivalent).
type ManualStep struct {
	Run    string         `json:"run"`
	At     string         `json:"at"`
	Action string         `json:"action"` // quarantine|delete_attachment|create_attachment|failed|healed
	Detail map[string]any `json:"detail,omitempty"`
}

// ManualRecord is the custody record of one attachment key (#279 DoD:
// original path, attachment key, reason, date — verifiable on the fixture).
// One JSON file per key under <quarantineRoot>/manual/. Steps append across
// runs, so an aborted protocol shows exactly what completed — the re-run
// basis. Status "healed" is terminal: the endpoint refuses a second repair
// (a re-run would upload a duplicate healed sibling — the Geursen class).
type ManualRecord struct {
	AttachmentKey    string       `json:"attachment_key"`
	DocumentKey      string       `json:"document_key"`
	Reason           string       `json:"reason"`
	OriginalPath     string       `json:"original_path"`
	ContentType      string       `json:"content_type"`
	CreatedAt        string       `json:"created_at"`
	Status           string       `json:"status"` // in_progress|failed|healed
	NewAttachmentKey string       `json:"new_attachment_key,omitempty"`
	Filename         string       `json:"filename,omitempty"`
	QuarantinePath   string       `json:"quarantine_path,omitempty"`
	Steps            []ManualStep `json:"steps"`
}

// manualDir is the record location inside the quarantine root.
func manualDir(root string) string { return filepath.Join(root, "manual") }

// manualRecordPath is where a key's record lives.
func manualRecordPath(root, zoteroKey string) string {
	return filepath.Join(manualDir(root), zoteroKey+".json")
}

// StatusHealed is the terminal record status — the endpoint's 409 guard.
const StatusHealed = "healed"

// LoadManualRecord reads a key's record (nil, nil = no record yet).
func LoadManualRecord(root, zoteroKey string) (*ManualRecord, error) {
	raw, err := os.ReadFile(manualRecordPath(root, zoteroKey))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec ManualRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("custody record %s: %w", zoteroKey, err)
	}
	return &rec, nil
}

// ManualNow is the record timestamp (UTC RFC3339).
func ManualNow() string { return now() }

// ManualRunID identifies one custody run within a key's record.
func ManualRunID(zoteroKey string) string {
	return zoteroKey + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// recordMu serializes record writes WITHIN the process. Cross-process
// concurrency is the operator's discipline (single operator, runbook).
// ponytail: process-local lock only — a cross-process lockfile buys nothing
// for a documented single-operator tool.
var recordMu sync.Mutex

func saveManualRecord(root string, rec *ManualRecord) error {
	recordMu.Lock()
	defer recordMu.Unlock()
	if err := os.MkdirAll(manualDir(root), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename: a reader never sees a half-written record.
	tmp := manualRecordPath(root, rec.AttachmentKey) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, manualRecordPath(root, rec.AttachmentKey))
}

// ManualDeps implements ApplyDeps for the manual custody path: real
// quarantine and real Zotero mutations (the server's authorized write
// client), file-based record instead of repair_cases bookkeeping.
type ManualDeps struct {
	Write  *zoteroprovider.WriteClient // nil in tests -> mutations must be injected differently; production always wires it
	Root   string                      // quarantine root
	Record *ManualRecord
	RunID  string
}

// now is a seam for deterministic record timestamps in tests.
var now = func() string { return time.Now().UTC().Format(time.RFC3339) }

func (d *ManualDeps) persist(action string, detail map[string]any) error {
	d.Record.Steps = append(d.Record.Steps, ManualStep{Run: d.RunID, At: now(), Action: action, Detail: detail})
	// Lift the terminal facts onto the record (the report a re-run reads).
	switch action {
	case "quarantine":
		if p, ok := detail["path"].(string); ok {
			d.Record.QuarantinePath = p
			d.Record.Status = "in_progress"
		}
	case "create_attachment", "create_attachment_orphan":
		// #285: the orphan lift happens in repair.Apply on the shared
		// ApplyDeps seam — this persist is how the manual record receives
		// it (the auto paths audit into zotero_write_audit instead).
		if k, ok := detail["new_zotero_key"].(string); ok {
			d.Record.NewAttachmentKey = k
		}
		if f, ok := detail["filename"].(string); ok {
			d.Record.Filename = f
		}
	}
	if err := saveManualRecord(d.Root, d.Record); err != nil {
		// The record is the re-run basis: a lost step write must be LOUD.
		fmt.Fprintf(os.Stderr, "custody record write failed (key %s, step %s): %v\n", d.Record.AttachmentKey, action, err)
		return err
	}
	return nil
}

func (d *ManualDeps) Quarantine(root, zoteroKey, sourcePath string) (string, error) {
	return Quarantine(root, zoteroKey, sourcePath)
}

// DeleteAttachment deletes the broken item, version-guarded like every
// mutation. A 404 is RESUME-SUCCESS (#279 DoD: abort after quarantine →
// re-run completes): an aborted first run may already have deleted the item.
func (d *ManualDeps) DeleteAttachment(key string) error {
	err := d.Write.DeleteAttachmentItem(key)
	var se *zoteroprovider.StatusError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// CreateAttachmentWithFile uploads the healed artifact. The ambiguous-
// create orphan lift (#285) happens in repair.Apply on the shared
// ApplyDeps seam: when the write gateway returns (key, err) — item minted,
// upload failed, best-effort cleanup ALSO failed — Apply audits
// create_attachment_orphan, which this deps' AuditWrite persists onto the
// custody record (NewAttachmentKey — the endpoint guard's basis, durably
// before the failure return). No interception here: one lift site for the
// manual AND both auto paths.
func (d *ManualDeps) CreateAttachmentWithFile(parentKey, filename, contentType string, pdf []byte) (string, error) {
	return d.Write.CreateAttachmentWithFile(parentKey, filename, contentType, pdf)
}

// MarkRepairFailed records the failing step (caseID is ignored — the manual
// path has no repair_cases row; the record file IS the case). Best-effort:
// the run is already failing; a record error is printed, not doubled.
func (d *ManualDeps) MarkRepairFailed(ctx context.Context, caseID, reason string) error {
	d.Record.Status = "failed"
	_ = d.persist("failed", map[string]any{"reason": reason})
	return nil
}

// MarkRepairHealed is the TERMINAL write: the persist error propagates
// (review W2) — a lost terminal record would silently disarm the 409
// idempotence guard, so Apply fails the run loudly instead (500 with the
// record; the upload itself already succeeded, the operator checks Zotero).
func (d *ManualDeps) MarkRepairHealed(ctx context.Context, caseID string) error {
	d.Record.Status = StatusHealed
	return d.persist("healed", nil)
}

// AuditWrite appends the custody action to the record — the step report.
// Persistence errors PROPAGATE: Apply fail-closes on the quarantine audit
// (no unaudited mutation — the #184 nail) and logs post-mutation audit
// failures as documented residuals.
func (d *ManualDeps) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	return d.persist(action, detail)
}
