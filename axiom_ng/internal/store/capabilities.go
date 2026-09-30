// capabilities.go — the Store component's capability model (F12, #306):
// five retrieval/persistence capabilities, reported HONESTLY from the
// actual backend equipment. A capability the stack does not carry is
// reported absent — never faked, never guessed (the SQLite-Store
// follow-up epic will report a DIFFERENT honest set over the same
// interface; that is the non-blockade guarantee).
//
// The five (the issue's vocabulary):
//
//	dense_vector — kNN embedding recall backed
//	bm25         — lexical BM25 recall backed
//	hybrid       — fused dense+BM25 retrieval (needs BOTH arms)
//	rerank       — cross-encoder reranking wired through a runner
//	graph        — graph-expanded candidate source wired
//
// Reporters: the search stack knows its own arms; the Store composes.
// The withdrawal sonde (TestCapabilityWithdrawalHonest) proves a
// capability that was taken away shows up as MISSING — the model can
// only shrink toward the truth, never invent.
package store

import "github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"

// Capability is one named retrieval/persistence capability.
type Capability string

const (
	CapDenseVector Capability = "dense_vector"
	CapBM25        Capability = "bm25"
	CapHybrid      Capability = "hybrid"
	CapRerank      Capability = "rerank"
	CapGraph       Capability = "graph"
)

// AllCapabilities is the full vocabulary, fixed order (reporting order).
var AllCapabilities = []Capability{CapDenseVector, CapBM25, CapHybrid, CapRerank, CapGraph}

// Capabilities is the honest equipment report: a capability is present
// ONLY when the wired backend actually carries it.
type Capabilities struct {
	DenseVector bool
	BM25        bool
	Hybrid      bool
	Rerank      bool
	Graph       bool
}

// Has reports one capability by name.
func (c Capabilities) Has(cap Capability) bool {
	switch cap {
	case CapDenseVector:
		return c.DenseVector
	case CapBM25:
		return c.BM25
	case CapHybrid:
		return c.Hybrid
	case CapRerank:
		return c.Rerank
	case CapGraph:
		return c.Graph
	}
	return false
}

// CapabilityReporter is implemented by backends that know their own
// equipment (the search stack reports search.Capabilities; the seam is
// over the BACKEND's type so the layering stays one-way store → search).
// A test fake can withdraw single capabilities to prove the honesty.
type CapabilityReporter interface {
	Capabilities() search.Capabilities
}

// Capabilities reports the Store's honest equipment, LIVE from the
// wired backend's reporter: what the intake seam and retrieval stack
// carry RIGHT NOW (the runner-role probe's verdict included once it
// lands). No reporter — empty set; the default is MISSING, never
// guessed present.
func (s *Service) Capabilities() Capabilities {
	if s.reporter == nil {
		return Capabilities{}
	}
	c := s.reporter.Capabilities()
	return Capabilities{
		DenseVector: c.DenseVector,
		BM25:        c.BM25,
		Hybrid:      c.Hybrid,
		Rerank:      c.Rerank,
		Graph:       c.Graph,
	}
}
