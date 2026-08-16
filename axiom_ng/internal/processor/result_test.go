package processor

import (
	"encoding/json"
	"strings"
	"testing"
)

// R5 review fix: sparse result weights arrive in BOTH forms — §10 strings
// from the real runner, native numbers from the reference backend — and both
// must decode. The internal shape stays string (validate/persist expect it).
func TestSparseValuesDecodeBothForms(t *testing.T) {
	// §10 canonical string form.
	var s SparseEmbedding
	if err := json.Unmarshal([]byte(`{"model":"m","values":{"12":"0.5","7":"2"}}`), &s); err != nil {
		t.Fatalf("string form: %v", err)
	}
	if s.Values["12"] != "0.5" || s.Values["7"] != "2" {
		t.Fatalf("string form wrong: %v", s.Values)
	}

	// Native numbers (reference backend): decoding must not fail, values
	// convert to the canonical string shape.
	var n SparseEmbedding
	if err := json.Unmarshal([]byte(`{"model":"m","values":{"12":0.5,"7":2}}`), &n); err != nil {
		t.Fatalf("number form: %v", err)
	}
	if n.Values["12"] != "0.5" || n.Values["7"] != "2" {
		t.Fatalf("number form wrong: %v", n.Values)
	}

	// Mixed form decodes element-wise.
	var m SparseEmbedding
	if err := json.Unmarshal([]byte(`{"model":"m","values":{"12":"0.5","7":2}}`), &m); err != nil {
		t.Fatalf("mixed form: %v", err)
	}
	if m.Values["12"] != "0.5" || m.Values["7"] != "2" {
		t.Fatalf("mixed form wrong: %v", m.Values)
	}

	// Marshal keeps the §10 string wire shape.
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"12":"0.5"`) || !strings.Contains(string(out), `"7":"2"`) {
		t.Fatalf("marshal must keep the string form: %s", out)
	}
}

func TestSparseValuesRejectBadWeights(t *testing.T) {
	for _, bad := range []string{
		`{"x":null}`,  // null weight
		`{"x":true}`,  // neither string nor number
		`{"x":["1"]}`, // array
	} {
		var v SparseValues
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Fatalf("%s must be rejected", bad)
		}
	}
}
