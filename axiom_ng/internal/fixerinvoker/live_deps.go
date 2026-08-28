// live_deps.go — the production ApplyDeps: *repo.Repo + *zotero.WriteClient
// adapted to the custody-sequence interface. Mirror of the server's
// liveRepairDeps; kept here so main.go can wire the invoker without
// reaching into the server package.
package fixerinvoker

import (
	"context"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repair"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

type liveDeps struct {
	rep   *repo.Repo
	write *zotero.WriteClient
}

// LiveApplyDeps wires the real implementations for repair.Apply.
func LiveApplyDeps(rep *repo.Repo, write *zotero.WriteClient) repair.ApplyDeps {
	return liveDeps{rep: rep, write: write}
}

func (d liveDeps) Quarantine(root, key, src string) (string, error) {
	return repair.Quarantine(root, key, src)
}
func (d liveDeps) DeleteAttachment(key string) error {
	return d.write.DeleteAttachmentItem(key)
}
func (d liveDeps) CreateAttachmentWithFile(parent, filename, contentType string, pdf []byte) (string, error) {
	return d.write.CreateAttachmentWithFile(parent, filename, contentType, pdf)
}
func (d liveDeps) MarkRepairFailed(ctx context.Context, caseID, reason string) error {
	return d.rep.MarkRepairFailed(ctx, caseID, reason)
}
func (d liveDeps) MarkRepairHealed(ctx context.Context, caseID string) error {
	return d.rep.MarkRepairHealed(ctx, caseID)
}
func (d liveDeps) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	return d.rep.AuditWrite(ctx, caseID, attachmentID, action, detail)
}
