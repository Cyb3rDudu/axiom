// repair_queue_adapter.go — the dispatcher's repair-case seam (F09
// #303): adapts the Library-owned repair store (F08) onto the
// dispatcher's primitive-shaped RepairQueue interface, unwrapping the
// case struct to the ids/classes the dispatcher policy reads. Lives in
// the composition root — the one place allowed to see both sides.
package composition

import (
	"context"
	"encoding/json"

	"github.com/Cyb3rDudu/axiom/axiom/internal/dispatcher"
	"github.com/Cyb3rDudu/axiom/axiom/internal/library/repair"
)

type repairQueueAdapter struct {
	store *repair.Store
}

var _ dispatcher.RepairQueue = (*repairQueueAdapter)(nil)

func (a *repairQueueAdapter) CreateRepairCase(ctx context.Context, attachmentID, documentID, suspicionClass string, analysis json.RawMessage) (string, bool, error) {
	c, created, err := a.store.CreateRepairCase(ctx, attachmentID, documentID, suspicionClass, analysis)
	if err != nil || c == nil {
		return "", created, err
	}
	return c.ID, created, nil
}

func (a *repairQueueAdapter) QueueRepairCase(ctx context.Context, caseID, suspicionClass string, analysis json.RawMessage) error {
	return a.store.QueueRepairCase(ctx, caseID, suspicionClass, analysis)
}

func (a *repairQueueAdapter) DocumentHealedCases(ctx context.Context, documentID string) (int, error) {
	return a.store.DocumentHealedCases(ctx, documentID)
}

func (a *repairQueueAdapter) WaveRepairGate(ctx context.Context) (bool, string, error) {
	return a.store.WaveRepairGate(ctx)
}
