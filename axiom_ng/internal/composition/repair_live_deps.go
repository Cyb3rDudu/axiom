// repair_live_deps.go — the production ApplyDeps wiring (F08 #302, moved
// from fixerinvoker/live_deps.go): the Library-owned repair Store +
// *zoteroprovider.WriteClient adapted to the custody-sequence interface
// (library/repair.ApplyDeps). Mirror of the server's liveRepairDeps; kept
// here so the composition root wires the orchestrator without reaching
// into the server package.
package composition

import (
	"context"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/repair"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

type liveApplyDepsImpl struct {
	store *repair.Store
	write *zoteroprovider.WriteClient
}

// liveApplyDeps wires the real implementations for repair.Apply.
func liveApplyDeps(store *repair.Store, write *zoteroprovider.WriteClient) repair.ApplyDeps {
	return liveApplyDepsImpl{store: store, write: write}
}

func (d liveApplyDepsImpl) Quarantine(root, key, src string) (string, error) {
	return repair.Quarantine(root, key, src)
}
func (d liveApplyDepsImpl) DeleteAttachment(key string) error {
	return d.write.DeleteAttachmentItem(key)
}
func (d liveApplyDepsImpl) CreateAttachmentWithFile(parent, filename, contentType string, pdf []byte) (string, error) {
	return d.write.CreateAttachmentWithFile(parent, filename, contentType, pdf)
}
func (d liveApplyDepsImpl) MarkRepairFailed(ctx context.Context, caseID, reason string) error {
	return d.store.MarkRepairFailed(ctx, caseID, reason)
}
func (d liveApplyDepsImpl) MarkRepairHealed(ctx context.Context, caseID string) error {
	return d.store.MarkRepairHealed(ctx, caseID)
}
func (d liveApplyDepsImpl) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	return d.store.AuditWrite(ctx, caseID, attachmentID, action, detail)
}

// executorFunc adapts a function to RepairExecutor (test bindings).
type executorFunc func(ctx context.Context, req repair.RepairRequest) (repair.RepairResult, error)

func (f executorFunc) Execute(ctx context.Context, req repair.RepairRequest) (repair.RepairResult, error) {
	return f(ctx, req)
}
