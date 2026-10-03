// capabilities.go — the search stack's honest equipment report (F12
// #306): the arms and runners THIS service was constructed with, wired
// or not — never the config intent alone. The Store's CapabilityReporter
// seam consumes this type (defined here so the layering stays one-way:
// store → search); the withdrawal sonde in internal/store proves a
// removed capability surfaces as missing.
package search

// Capabilities reports the retrieval stack's equipment:
//
//   - DenseVector: the dense arm AND a runner whose PROBED roles carry
//     query_embedding (a wired-but-incapable runner is NOT equipment);
//   - BM25: the BM25 arm over the OpenSearch index;
//   - Hybrid: both recall arms (the fusion needs both);
//   - Rerank: rerank enabled AND a runner with the probed reranking role;
//   - Graph: the graph arm AND a wired graph source;
//   - Sparse: the learned-lexical rank_features arm (report-only extra;
//     the Store's five-capability vocabulary folds it under BM25).
//
// Roles are the startup probe's verdict (SetRunnerRoles); unprobed means
// not vouched — the report can only shrink toward the truth.
type Capabilities struct {
	DenseVector bool
	BM25        bool
	Hybrid      bool
	Rerank      bool
	Graph       bool
	Sparse      bool
}

// Capabilities reports what this stack actually carries.
func (s *Service) Capabilities() Capabilities {
	c := Capabilities{
		DenseVector: s.DenseArm && s.processor != nil && s.runnerRoles.queryEmbedding.Load(),
		BM25:        s.BM25Arm,
		Rerank:      s.Rerank && s.processor != nil && s.runnerRoles.reranking.Load(),
		Graph:       s.GraphArm && s.graph != nil,
		Sparse:      s.SparseArm && s.DenseArm && s.processor != nil && s.runnerRoles.queryEmbedding.Load(),
	}
	c.Hybrid = c.DenseVector && c.BM25
	return c
}
