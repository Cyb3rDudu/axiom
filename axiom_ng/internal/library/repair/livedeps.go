// livedeps.go — the production ApplyDeps wiring, single authority since
// review round 3 (#302): the Library-owned repair Store plus a Zotero
// write client adapted to the custody-sequence interface. The HTTP
// surface and the composition root BOTH wire through NewLiveApplyDeps —
// one implementation, no mirror to drift (the pre-F08 shape had two
// pass-through copies; a future ApplyDeps extension could land in only
// one of them).
package repair

import (
	"context"
)

// AttachmentWriter is the Zotero write surface the custody sequence
// needs — *zoteroprovider.WriteClient satisfies it structurally (the
// variadic extraTags mirror the concrete method's shape; custody never
// passes any). The slim interface keeps this adapter testable without
// the concrete client (F11 retyping may replace it with a Library port).
type AttachmentWriter interface {
	DeleteAttachmentItem(key string) error
	CreateAttachmentWithFile(parent, filename, contentType string, pdf []byte, extraTags ...string) (string, error)
}

type liveApplyDeps struct {
	store *Store
	write AttachmentWriter
}

// NewLiveApplyDeps wires the real implementations for Apply.
func NewLiveApplyDeps(store *Store, write AttachmentWriter) ApplyDeps {
	return liveApplyDeps{store: store, write: write}
}

func (d liveApplyDeps) Quarantine(root, key, src string) (string, error) {
	return Quarantine(root, key, src)
}
func (d liveApplyDeps) DeleteAttachment(key string) error {
	return d.write.DeleteAttachmentItem(key)
}
func (d liveApplyDeps) CreateAttachmentWithFile(parent, filename, contentType string, pdf []byte) (string, error) {
	return d.write.CreateAttachmentWithFile(parent, filename, contentType, pdf)
}
func (d liveApplyDeps) MarkRepairFailed(ctx context.Context, caseID, reason string) error {
	return d.store.MarkRepairFailed(ctx, caseID, reason)
}
func (d liveApplyDeps) MarkRepairHealed(ctx context.Context, caseID string) error {
	return d.store.MarkRepairHealed(ctx, caseID)
}
func (d liveApplyDeps) AuditWrite(ctx context.Context, caseID, attachmentID, action string, detail map[string]any) error {
	return d.store.AuditWrite(ctx, caseID, attachmentID, action, detail)
}
