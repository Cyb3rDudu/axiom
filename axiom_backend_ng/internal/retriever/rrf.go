// Package retriever orchestrates the hybrid search pipeline for
// axiom-ng: dense pgvector + sparse JSONB + OpenSearch BM25, fused
// via RRF, with an optional cross-encoder rerank pass through the
// Python gpu_worker.
//
// Parity target: axiom_backend/ai_researcher/core_rag/retriever.py.
package retriever

import (
	"sort"
)

// DefaultRRFConstant matches retriever.py (k=60). Exposed so tests
// can pin.
const DefaultRRFConstant = 60

// Ranked is a result from one of the search channels. Score is the
// channel-native score (cosine similarity, BM25 _score, sparse dot)
// — RRF ignores the absolute value and uses rank position only.
type Ranked struct {
	ChunkID string
	Payload map[string]any
	Score   float64
}

// FusionInput bundles one channel's results with the weight it should
// contribute to the final fused ranking.
type FusionInput struct {
	Name   string
	Weight float64
	Hits   []Ranked
}

// FusedHit is the output of RRF: a ChunkID with per-channel ranks and
// a combined score.
type FusedHit struct {
	ChunkID       string
	CombinedScore float64
	ChannelScores map[string]float64 // raw score per channel (0 if absent)
	ChannelRanks  map[string]int     // 1-indexed rank per channel (0 if absent)
	Payload       map[string]any     // prefer the highest-ranked channel's payload
}

// RRF fuses N result lists using the reciprocal rank fusion formula.
// Weights are normalised so callers can pass unnormalised numbers
// (0.7 / 0.3) and get the expected behaviour.
//
//	score(chunk) = Σ (weight_i · 1/(k + rank_i))
//
// k defaults to DefaultRRFConstant when 0 is passed. Missing channels
// contribute 0.
func RRF(inputs []FusionInput, k int) []FusedHit {
	if k <= 0 {
		k = DefaultRRFConstant
	}
	weights := normaliseWeights(inputs)

	// map chunk_id → accumulated FusedHit.
	acc := make(map[string]*FusedHit)

	for i, in := range inputs {
		w := weights[i]
		for rank, r := range in.Hits {
			one := 1.0 / float64(k+rank+1) // rank is 0-indexed
			f, ok := acc[r.ChunkID]
			if !ok {
				f = &FusedHit{
					ChunkID:       r.ChunkID,
					ChannelScores: map[string]float64{},
					ChannelRanks:  map[string]int{},
					Payload:       r.Payload,
				}
				acc[r.ChunkID] = f
			}
			f.CombinedScore += w * one
			f.ChannelScores[in.Name] = r.Score
			f.ChannelRanks[in.Name] = rank + 1
			// Prefer the payload from the higher-ranked (earlier)
			// channel since later writes would otherwise win.
			if f.Payload == nil {
				f.Payload = r.Payload
			}
		}
	}

	out := make([]FusedHit, 0, len(acc))
	for _, f := range acc {
		out = append(out, *f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CombinedScore > out[j].CombinedScore
	})
	return out
}

// normaliseWeights divides each input's weight by the total so
// downstream scoring is invariant to the absolute weight magnitudes.
// Zero-sum inputs fall back to equal weighting.
func normaliseWeights(inputs []FusionInput) []float64 {
	w := make([]float64, len(inputs))
	var total float64
	for i, in := range inputs {
		if in.Weight < 0 {
			w[i] = 0
		} else {
			w[i] = in.Weight
		}
		total += w[i]
	}
	if total == 0 {
		for i := range w {
			w[i] = 1.0 / float64(len(w))
		}
		return w
	}
	for i := range w {
		w[i] /= total
	}
	return w
}
