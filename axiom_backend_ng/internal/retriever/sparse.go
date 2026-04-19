package retriever

import (
	"encoding/json"
	"math"
	"strconv"
)

// SparseVector is axiom's canonical sparse embedding shape: token id
// (int-as-string key, to round-trip through JSONB) → weight.
type SparseVector map[string]float64

// Cosine returns the cosine similarity between two sparse vectors.
// Returns 0 when either side has zero magnitude (matches Python's
// _compute_sparse_similarity in pgvector_store.py).
func (s SparseVector) Cosine(other SparseVector) float64 {
	if len(s) == 0 || len(other) == 0 {
		return 0
	}
	var dot, mag1, mag2 float64
	for k, v := range s {
		mag1 += v * v
		if w, ok := other[k]; ok {
			dot += v * w
		}
	}
	for _, v := range other {
		mag2 += v * v
	}
	if mag1 == 0 || mag2 == 0 {
		return 0
	}
	return dot / (math.Sqrt(mag1) * math.Sqrt(mag2))
}

// DecodeSparse parses the Postgres JSONB blob stored in
// document_chunks.sparse_embedding (an object mapping str(token_id)→weight).
// Silently returns an empty map on parse failure so a single bad row
// doesn't poison the whole search.
func DecodeSparse(raw []byte) SparseVector {
	if len(raw) == 0 {
		return SparseVector{}
	}
	out := SparseVector{}
	if err := json.Unmarshal(raw, &out); err != nil {
		// Some chunks may have stored the integer keys directly
		// (map[int]float) in older migrations — retry that shape.
		var asInt map[int]float64
		if err2 := json.Unmarshal(raw, &asInt); err2 == nil {
			for k, v := range asInt {
				out[strconv.Itoa(k)] = v
			}
			return out
		}
		return SparseVector{}
	}
	return out
}
