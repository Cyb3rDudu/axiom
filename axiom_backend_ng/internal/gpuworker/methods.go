package gpuworker

import (
	"context"
	"fmt"
)

// HealthInfo is the typed shape of the worker's `health` RPC result.
type HealthInfo struct {
	PID       int             `msgpack:"pid" json:"pid"`
	UptimeSec float64         `msgpack:"uptime_sec" json:"uptime_sec"`
	Loaded    map[string]bool `msgpack:"loaded" json:"loaded"`
	VRAMMB    *float64        `msgpack:"vram_mb" json:"vram_mb"`
}

// Health returns current worker state. Useful for readiness probes
// and the /api/system/gpu-status endpoint.
func (c *Client) Health(ctx context.Context) (HealthInfo, error) {
	var out HealthInfo
	if err := c.Call(ctx, "health", nil, &out); err != nil {
		return HealthInfo{}, err
	}
	return out, nil
}

// EmbedResult wraps the `embed_query` RPC return. The Python worker
// returns a dict shaped like {"vector": [...], optionally sparse,
// dense, colbert fields}; Go preserves it as a loose map so later
// slices can parse whichever keys the retriever needs.
type EmbedResult map[string]any

// EmbedQuery runs the BGE-M3 embedder on a single query string.
func (c *Client) EmbedQuery(ctx context.Context, text string) (EmbedResult, error) {
	var out EmbedResult
	args := map[string]any{"text": text}
	if err := c.Call(ctx, "embed_query", args, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EmbedChunks runs the embedder over a batch of chunk dicts. Each
// chunk must at minimum carry a `text` field (the Python facade
// convention). The returned slice mirrors the input and adds
// embedding fields to each chunk map.
func (c *Client) EmbedChunks(ctx context.Context, chunks []map[string]any) ([]map[string]any, error) {
	if chunks == nil {
		chunks = []map[string]any{}
	}
	var out []map[string]any
	args := map[string]any{"chunks": chunks}
	if err := c.Call(ctx, "embed_chunks", args, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RerankHit matches the Python worker's `rerank` return element:
// [score, original_index].
type RerankHit struct {
	Score float64
	Index int
}

// Rerank runs the BGE-Reranker over a list of candidate strings,
// returning a sorted-desc slice of (score, original-index) pairs.
// items carries only the text payload; callers typically re-map
// indices back onto whatever Pydantic/Go object they started with.
func (c *Client) Rerank(ctx context.Context, query string, items []string, topN int) ([]RerankHit, error) {
	payload := make([]map[string]any, len(items))
	for i, s := range items {
		payload[i] = map[string]any{"text": s}
	}
	args := map[string]any{
		"query": query,
		"items": payload,
	}
	if topN > 0 {
		args["top_n"] = topN
	}
	// Worker returns list[[float, int]] — a slice of 2-element slices.
	var raw [][]any
	if err := c.Call(ctx, "rerank", args, &raw); err != nil {
		return nil, err
	}
	out := make([]RerankHit, 0, len(raw))
	for i, pair := range raw {
		if len(pair) != 2 {
			return nil, fmt.Errorf("gpu-worker rerank: pair %d has length %d, want 2", i, len(pair))
		}
		score, ok := asFloat(pair[0])
		if !ok {
			return nil, fmt.Errorf("gpu-worker rerank: pair %d score is %T", i, pair[0])
		}
		idx, ok := asInt(pair[1])
		if !ok {
			return nil, fmt.Errorf("gpu-worker rerank: pair %d index is %T", i, pair[1])
		}
		out = append(out, RerankHit{Score: score, Index: idx})
	}
	return out, nil
}

// Entity matches the Python worker's `extract_entities` return shape.
type Entity struct {
	Text  string  `msgpack:"text" json:"text"`
	Label string  `msgpack:"label" json:"label"`
	Score float64 `msgpack:"score" json:"score"`
	Start int     `msgpack:"start" json:"start"`
	End   int     `msgpack:"end" json:"end"`
}

// ExtractEntities runs GLiNER zero-shot NER over text. Returns an
// empty slice when GLiNER isn't loaded (matches Python behaviour).
func (c *Client) ExtractEntities(ctx context.Context, text string, labels []string, threshold float64, multiLabel bool) ([]Entity, error) {
	var out []Entity
	args := map[string]any{
		"text":        text,
		"labels":      labels,
		"threshold":   threshold,
		"multi_label": multiLabel,
	}
	if err := c.Call(ctx, "extract_entities", args, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Shutdown asks the worker to stop accepting new requests and exit
// after any in-flight ones complete. Owner-mode only — a no-op on
// the worker side when the client is running in client-mode.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.Call(ctx, "shutdown", nil, nil)
}

// asFloat coerces the common numeric types msgpack produces into
// float64. int8..int64 and uint8..uint64 all get promoted.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

// asInt coerces integer-like msgpack values.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int8:
		return int(n), true
	case int16:
		return int(n), true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case uint:
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		return int(n), true
	case uint64:
		return int(n), true
	default:
		return 0, false
	}
}
