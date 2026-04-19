package ingest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/ingest"
)

func TestChainRunsInOrderAndAbortsOnError(t *testing.T) {
	t.Parallel()
	var calls []int
	one := ingest.ProcessorFunc(func(_ context.Context, _ ingest.Job) error {
		calls = append(calls, 1)
		return nil
	})
	two := ingest.ProcessorFunc(func(_ context.Context, _ ingest.Job) error {
		calls = append(calls, 2)
		return errors.New("two failed")
	})
	three := ingest.ProcessorFunc(func(_ context.Context, _ ingest.Job) error {
		calls = append(calls, 3)
		return nil
	})

	chain := ingest.Chain{one, nil, two, three} // nil entry is skipped
	err := chain.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || err.Error() != "two failed" {
		t.Fatalf("expected 'two failed', got %v", err)
	}
	if len(calls) != 2 || calls[0] != 1 || calls[1] != 2 {
		t.Errorf("call order: %v (expected [1 2])", calls)
	}
}

func TestChainEmptySucceeds(t *testing.T) {
	t.Parallel()
	if err := (ingest.Chain{}).Process(context.Background(), ingest.Job{}); err != nil {
		t.Errorf("empty chain should succeed: %v", err)
	}
}
