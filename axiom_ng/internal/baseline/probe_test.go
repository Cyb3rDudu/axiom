// probe_test.go — passage and KG golden probes (#295 Ziel 3, read sides).
//
// The fixtures pin one stable, freeze-day object each: a known chunk id
// (passage, including locator fields) and a known KG node (entity lookup,
// neighbors, relations). Both are id-addressed and therefore immune to
// ranking drift; both go red if field semantics change in 0.2.0.
package baseline

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// --- passage ---------------------------------------------------------------

type passageFixture struct {
	ChunkID string `json:"chunk_id"`
	Expect  struct {
		LocatorKind   string   `json:"locator_kind"`
		LocatorFields []string `json:"locator_fields"`
		HasText       bool     `json:"has_text"`
		HasSource     bool     `json:"has_source"`
	} `json:"expect"`
}

func TestLivePassageGolden(t *testing.T) {
	liveEnabled(t)
	assertFreezeBits(t)

	var f passageFixture
	raw, err := os.ReadFile("fixtures/passage_probe.json")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}

	var p map[string]any
	if code := httpJSON(t, "GET", ragBase()+"/api/passage/"+f.ChunkID, nil, &p); code != 200 {
		t.Fatalf("passage status %d", code)
	}
	goldenCompare(t, "passage_shape.json", shapeOf(p))

	if _, ok := p["document_id"].(string); !ok || p["document_id"] == "" {
		t.Error("passage: document_id missing/empty")
	}
	if f.Expect.HasText {
		if s, _ := p["text"].(string); len(s) < 50 {
			t.Errorf("passage: text suspiciously short (%d chars)", len(s))
		}
	}
	if f.Expect.HasSource && p["source"] == nil {
		t.Error("passage: source block missing")
	}
	loc, _ := p["locator"].(map[string]any)
	if loc == nil {
		t.Fatalf("passage: locator missing (Locator-Felder sind Teil des Auftrags)")
	}
	if k, _ := loc["kind"].(string); k != f.Expect.LocatorKind {
		t.Errorf("passage: locator.kind = %q, want %q", k, f.Expect.LocatorKind)
	}
	for _, field := range f.Expect.LocatorFields {
		if _, ok := loc[field]; !ok {
			t.Errorf("passage: locator field %q missing", field)
		}
	}
}

// shapeOf reduces a JSON object to its structural skeleton: keys with
// value KINDS (string/number/bool/object/array/null), recursively for
// objects. This snapshots the response CONTRACT (field surface) without
// pinning volatile values.
//
// UUID-keyed map entries (data identity, e.g. the neighbors `sources`
// map) collapse to ONE "(uuid)" representative — the sorted-first key —
// so a changed document set cannot flip the golden red spuriously.
// Contract keys (locator kind/label/cfi, source fields, …) stay explicit.
//
// ponytail: the representative is sorted-first; shape differences
// BETWEEN data entries are not pinned (pin per-entry explicitly if a
// 0.2.0 step needs that). Array ELEMENT fields are likewise not pinned —
// arrays collapse to "[]<kind>" of their first element.
func shapeOf(v any) map[string]any {
	out := map[string]any{}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{"(kind)": kindOf(v)}
	}
	var uuidKeys []string
	for k := range m {
		if isUUIDKey(k) {
			uuidKeys = append(uuidKeys, k)
		}
	}
	if len(uuidKeys) > 0 {
		sort.Strings(uuidKeys)
		out["(uuid)"] = shapeOf(m[uuidKeys[0]])
	}
	for k, val := range m {
		if isUUIDKey(k) {
			continue
		}
		if sub, isObj := val.(map[string]any); isObj {
			out[k] = shapeOf(sub)
			continue
		}
		if arr, isArr := val.([]any); isArr {
			if len(arr) == 0 {
				out[k] = "[]"
			} else {
				out[k] = "[]" + kindOf(arr[0])
			}
			continue
		}
		out[k] = kindOf(val)
	}
	return out
}

// isUUIDKey reports whether k is a data-identity key (UUID shape:
// 8-4-4-4-12 hex, dashes at the fixed positions) rather than a contract
// key ("kind", "label", …).
func isUUIDKey(k string) bool {
	if len(k) != 36 {
		return false
	}
	for i, r := range k {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexRune(r) {
				return false
			}
		}
	}
	return true
}

func isHexRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// TestShapeOfUUIDKeyCollapse — unit teeth for the collapse rule: UUID
// keys vanish into "(uuid)", contract keys survive, mixed maps keep both.
func TestShapeOfUUIDKeyCollapse(t *testing.T) {
	in := map[string]any{
		"kind": "epub_cfi",
		"sources": map[string]any{
			"b215b8c2-0000-4000-8000-000000000002": map[string]any{"title": "x"},
			"a215b8c2-0000-4000-8000-000000000001": map[string]any{"title": "y"},
		},
	}
	got := shapeOf(in)
	src, ok := got["sources"].(map[string]any)
	if !ok {
		t.Fatalf("sources shape missing: %v", got)
	}
	if _, ok := src["(uuid)"]; !ok {
		t.Fatalf("UUID keys not collapsed: %v", src)
	}
	for k := range src {
		if isUUIDKey(k) {
			t.Fatalf("raw UUID key %q leaked into the shape", k)
		}
	}
	if k, _ := got["kind"]; k != "string" {
		t.Fatalf("contract key degraded: %v", got["kind"])
	}
}

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "other"
	}
}

// --- knowledge graph -------------------------------------------------------

type kgFixture struct {
	EntityQuery string `json:"entity_query"`
	Expect      struct {
		TopCanonicalForm string `json:"top_canonical_form"`
		TopType          string `json:"top_type"`
		MinMentions      int    `json:"min_mentions"`
		NeighborsMin     int    `json:"neighbors_min"`
		RelationsMin     int    `json:"relations_min"`
	} `json:"expect"`
}

func TestLiveKGGolden(t *testing.T) {
	liveEnabled(t)
	assertFreezeBits(t)

	var f kgFixture
	raw, err := os.ReadFile("fixtures/kg_probe.json")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}

	var entities []map[string]any
	if code := httpJSON(t, "GET", ragBase()+"/api/kg/entities?q="+f.EntityQuery+"&limit=3", nil, &entities); code != 200 {
		t.Fatalf("kg entities status %d", code)
	}
	if len(entities) == 0 {
		t.Fatal("kg entities: empty")
	}
	top := entities[0]
	if cf, _ := top["canonical_form"].(string); cf != f.Expect.TopCanonicalForm {
		t.Errorf("kg top canonical_form = %q, want %q", cf, f.Expect.TopCanonicalForm)
	}
	if ty, _ := top["type"].(string); ty != f.Expect.TopType {
		t.Errorf("kg top type = %q, want %q", ty, f.Expect.TopType)
	}
	m, ok := top["mentions"].(float64)
	if !ok {
		t.Fatalf("kg top mentions is not a number: %v", top["mentions"])
	}
	if int(m) < f.Expect.MinMentions {
		t.Errorf("kg top mentions = %d, want >= %d", int(m), f.Expect.MinMentions)
	}
	goldenCompare(t, "kg_entities_shape.json", shapeOf(top))

	id, _ := top["id"].(string)
	var neighbors map[string]any
	if code := httpJSON(t, "GET", ragBase()+"/api/kg/entities/"+id+"/neighbors", nil, &neighbors); code != 200 {
		t.Fatalf("kg neighbors status %d", code)
	}
	goldenCompare(t, "kg_neighbors_shape.json", shapeOf(neighbors))
	if n := arrayLen(neighbors); n < f.Expect.NeighborsMin {
		t.Errorf("kg neighbors: got %d arrays/entries, want >= %d", n, f.Expect.NeighborsMin)
	}

	var relations map[string]any
	if code := httpJSON(t, "GET", ragBase()+"/api/kg/relations?limit=5", nil, &relations); code != 200 {
		t.Fatalf("kg relations status %d", code)
	}
	rels, _ := relations["relations"].([]any)
	if len(rels) < f.Expect.RelationsMin {
		t.Errorf("kg relations: got %d, want >= %d", len(rels), f.Expect.RelationsMin)
	}
	if len(rels) > 0 {
		goldenCompare(t, "kg_relation_shape.json", shapeOf(rels[0]))
	}
}

// arrayLen counts entries across the array-valued fields of a response
// object (neighbors may nest under different keys across shapes).
func arrayLen(m map[string]any) int {
	n := 0
	for _, v := range m {
		if arr, ok := v.([]any); ok {
			if len(arr) > n {
				n = len(arr)
			}
		}
	}
	return n
}
