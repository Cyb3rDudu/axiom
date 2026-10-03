// capabilities_test.go — the Store capability model's honesty proofs
// (F12 #306): the five-capability report from a fully-equipped backend,
// the withdrawal sonde (a removed capability appears MISSING, never
// present), and the no-reporter default (nothing guessed).
package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom/internal/search"
)

// capBackend is a SearchBackend double with a pluggable equipment
// report (the withdrawal sonde's lever).
type capBackend struct {
	caps search.Capabilities
}

func (b *capBackend) Search(ctx context.Context, req search.Request) (*search.Response, error) {
	return nil, nil
}
func (b *capBackend) GetPassage(ctx context.Context, chunkID string) (*search.Passage, error) {
	return nil, nil
}
func (b *capBackend) Capabilities() search.Capabilities { return b.caps }

// equipped is the fully-backed report: everything the production search
// stack carries when all arms and the runner are wired.
func equipped() search.Capabilities {
	return search.Capabilities{DenseVector: true, BM25: true, Hybrid: true, Rerank: true, Graph: true, Sparse: true}
}

func TestCapabilityReportFromEquippedBackend(t *testing.T) {
	svc := New(nil, &capBackend{caps: equipped()}, nil)
	c := svc.Capabilities()
	for _, cap := range AllCapabilities {
		if !c.Has(cap) {
			t.Fatalf("equipped backend must report %s present: %+v", cap, c)
		}
	}
}

func TestCapabilityWithdrawalHonest(t *testing.T) {
	// Withdraw ONE capability at a time: the report must show it MISSING
	// while the others stay present — a withdrawn capability never
	// appears, and honesty never over-reports either.
	for _, cap := range AllCapabilities {
		eq := equipped()
		switch cap {
		case CapDenseVector:
			eq.DenseVector = false
			eq.Hybrid = false // hybrid NEEDS the dense arm — it withdraws too
		case CapBM25:
			eq.BM25 = false
			eq.Hybrid = false
		case CapHybrid:
			eq.Hybrid = false
		case CapRerank:
			eq.Rerank = false
		case CapGraph:
			eq.Graph = false
		}
		svc := New(nil, &capBackend{caps: eq}, nil)
		c := svc.Capabilities()
		if c.Has(cap) {
			t.Fatalf("withdrawn capability %s must be reported MISSING: %+v", cap, c)
		}
		// every NOT-withdrawn capability stays present — except hybrid,
		// which is structurally coupled to both recall arms (its
		// withdrawal is the arms' own case, asserted by them).
		for _, other := range AllCapabilities {
			if other == cap || other == CapHybrid {
				continue
			}
			if !c.Has(other) {
				t.Fatalf("capability %s must stay present while %s is withdrawn: %+v", other, cap, c)
			}
		}
	}
}

// plainBackend cannot report equipment at all (no Capabilities method).
type plainBackend struct{}

func (b *plainBackend) Search(ctx context.Context, req search.Request) (*search.Response, error) {
	return nil, nil
}
func (b *plainBackend) GetPassage(ctx context.Context, chunkID string) (*search.Passage, error) {
	return nil, nil
}

func TestCapabilityNoReporterMeansNothingGuessed(t *testing.T) {
	// A backend that cannot report equipment: the Store reports the
	// empty set — missing by default, never guessed present.
	svc := New(nil, &plainBackend{}, nil)
	c := svc.Capabilities()
	for _, cap := range AllCapabilities {
		if c.Has(cap) {
			t.Fatalf("unreported capability %s must default to MISSING: %+v", cap, c)
		}
	}
}

// TestCapabilityMappingOracle — the Store's report is a hand-maintained
// copy of the backend's (search.Capabilities → store.Capabilities, field
// by field in Service.Capabilities()). The oracle: for EVERY field of the backend report
// (documented extras aside), a report carrying ONLY that field must map
// to exactly that capability store-side — add, rename, or drop a field
// on either side and this goes red instead of silently unmapping.
// (Hybrid is excluded from the oracle: it is DERIVED — DenseVector &&
// BM25 — and its coupling is pinned by TestCapabilityWithdrawalHonest's
// per-arm withdrawal cases.)
func TestCapabilityMappingOracle(t *testing.T) {
	backend := reflect.TypeOf(search.Capabilities{})
	extras := map[string]bool{"Sparse": true} // report-only backend-side
	for i := range backend.NumField() {
		name := backend.Field(i).Name
		if extras[name] {
			continue
		}
		src := reflect.New(backend).Elem()
		src.Field(i).SetBool(true)
		svc := New(nil, &reportBackend{report: src.Interface().(search.Capabilities)}, nil)
		got := reflect.ValueOf(svc.Capabilities())
		target := reflect.TypeOf(Capabilities{})
		if _, ok := target.FieldByName(name); !ok {
			t.Fatalf("backend capability %q has no store-side field — vocabulary drift", name)
		}
		for j := range target.NumField() {
			fn := target.Field(j).Name
			gotV := got.Field(j).Bool()
			wantV := fn == name // only the set field may be true (Hybrid excluded: derived)
			if fn == "Hybrid" {
				continue // derived backend-side (DenseVector && BM25), not an independent field
			}
			if gotV != wantV {
				t.Fatalf("field %q: report-only-%q mapped to %v, want %v — the New mapping drifted", fn, name, gotV, wantV)
			}
		}
	}
}

// reportBackend answers a fixed equipment report.
type reportBackend struct {
	report search.Capabilities
}

func (b *reportBackend) Search(ctx context.Context, req search.Request) (*search.Response, error) {
	return nil, nil
}
func (b *reportBackend) GetPassage(ctx context.Context, chunkID string) (*search.Passage, error) {
	return nil, nil
}
func (b *reportBackend) Capabilities() search.Capabilities { return b.report }

// TestSearchStackReportsItsOwnEquipment — the real search stack over
// its own construction state: no runner wired withdraws the
// runner-backed capabilities (dense, rerank) while index-backed BM25
// stays; the graph ARM flag without a graph source stays missing; the
// sparse arm without the dense runner stays missing.
func TestSearchStackReportsItsOwnEquipment(t *testing.T) {
	s := search.New("http://127.0.0.1:9200", "", "", nil, nil, nil)
	s.GraphArm = true
	s.SparseArm = true
	c := s.Capabilities()
	if c.DenseVector || c.Rerank || c.Hybrid || c.Graph || c.Sparse {
		t.Fatalf("no runner, no graph source: everything runner/wiring-backed must be MISSING: %+v", c)
	}
	if !c.BM25 {
		t.Fatalf("BM25 is index-backed and must stay: %+v", c)
	}
}

// TestCapabilitiesDelegateLiveAfterConstruction — the C1 regression
// witness: the Store's report must reflect the backend's CURRENT
// equipment, not a boot-time snapshot. Flip the reporter AFTER New (the
// runner-role probe lands exactly there in the composition —
// searchSvc.SetRunnerRoles runs after store.New) and the store-side
// report must follow. A regression back to snapshot-in-New fails here.
func TestCapabilitiesDelegateLiveAfterConstruction(t *testing.T) {
	// The real chain end-to-end: the search stack over a stub processor,
	// whose role probe lands after the Store was constructed.
	s := search.New("http://127.0.0.1:9200", "", "", stubProc{}, nil, nil)
	svc := New(nil, s, nil)
	if c := svc.Capabilities(); c.DenseVector || c.Rerank || c.Hybrid {
		t.Fatalf("unprobed runner must not read as equipment: %+v", c)
	}
	// The async probe's verdict arrives NOW — after construction.
	s.SetRunnerRoles(true, true)
	c := svc.Capabilities()
	if !c.DenseVector || !c.Rerank || !c.Hybrid {
		t.Fatalf("the probe's late verdict must be reflected LIVE: %+v", c)
	}
	// And can equally withdraw again.
	s.SetRunnerRoles(false, false)
	if c := svc.Capabilities(); c.DenseVector || c.Rerank {
		t.Fatalf("withdrawal after construction must be reflected too: %+v", c)
	}
}

// stubProc: a wired-but-dumb runner (roles arrive via SetRunnerRoles).
type stubProc struct{}

func (stubProc) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, context.DeadlineExceeded
}
func (stubProc) EmbedQueriesSparse(ctx context.Context, texts []string) ([][]float32, []map[string]float64, error) {
	return nil, nil, context.DeadlineExceeded
}
func (stubProc) Rerank(ctx context.Context, query string, texts []string, topN int) ([]processor.RerankScore, error) {
	return nil, context.DeadlineExceeded
}
