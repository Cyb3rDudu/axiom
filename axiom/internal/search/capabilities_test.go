// capabilities_test.go — the search stack's capability honesty (F12
// #306): the PROBED runner roles gate the runner-backed capabilities —
// a wired-but-incapable runner (the stub without query roles, exactly
// the topology the composition warns about) must report MISSING, and
// the reflection drift-gate pins the store vocabulary mapping.
package search

import (
	"context"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/processor"
)

// stubProcessor is a wired-but-dumb runner: every call errors. Wiring
// alone must never vouch for a capability.
type stubProcessor struct{}

func (stubProcessor) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, context.DeadlineExceeded
}
func (stubProcessor) EmbedQueriesSparse(ctx context.Context, texts []string) ([][]float32, []map[string]float64, error) {
	return nil, nil, context.DeadlineExceeded
}
func (stubProcessor) Rerank(ctx context.Context, query string, texts []string, topN int) ([]processor.RerankScore, error) {
	return nil, context.DeadlineExceeded
}

func TestWiredButRolelessRunnerIsNotEquipment(t *testing.T) {
	s := New("http://127.0.0.1:9200", "", "", stubProcessor{}, nil, nil)
	// The exact over-report the review caught: processor non-nil, arms on,
	// but the runner has no query roles (no probe vouched for them).
	c := s.Capabilities()
	if c.DenseVector || c.Rerank || c.Hybrid || c.Sparse {
		t.Fatalf("a wired but unprobed/incapable runner must not read as equipment: %+v", c)
	}
	if !c.BM25 {
		t.Fatalf("BM25 is index-backed and must stay: %+v", c)
	}
	// The probe vouches → the runner-backed capabilities appear.
	s.SetRunnerRoles(true, true)
	c = s.Capabilities()
	if !c.DenseVector || !c.Rerank || !c.Hybrid {
		t.Fatalf("a role-probed runner is equipment: %+v", c)
	}
	// A PARTIAL probe is honest partial: embedding without reranking.
	s.SetRunnerRoles(true, false)
	c = s.Capabilities()
	if !c.DenseVector || c.Rerank {
		t.Fatalf("partial roles must report exactly their half: %+v", c)
	}
	if !c.Hybrid {
		t.Fatalf("hybrid needs dense+bm25 only — both present here: %+v", c)
	}
}
