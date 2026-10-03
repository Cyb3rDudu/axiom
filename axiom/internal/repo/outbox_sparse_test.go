package repo

import "testing"

// Hermetic pins for ParseSparse (R5 #135): the JSONB duality (§10 strings vs
// reference-backend native numbers) and the positive-finite weight guard
// rank_features requires.
func TestParseSparseForms(t *testing.T) {
	// §10 canonical string form.
	m, err := ParseSparse(`{"12":"0.5","7":"2"}`)
	if err != nil {
		t.Fatalf("string form: %v", err)
	}
	if m["12"] != 0.5 || m["7"] != 2 {
		t.Fatalf("string form decoded wrong: %v", m)
	}

	// Native number form (the reference backend bypasses stringification).
	m, err = ParseSparse(`{"12":0.5,"7":2}`)
	if err != nil {
		t.Fatalf("number form: %v", err)
	}
	if m["12"] != 0.5 || m["7"] != 2 {
		t.Fatalf("number form decoded wrong: %v", m)
	}

	// Mixed form decodes.
	m, err = ParseSparse(`{"12":"0.5","7":2}`)
	if err != nil || m["7"] != 2 {
		t.Fatalf("mixed form: %v %v", m, err)
	}

	// Empty object is legal (a chunk with an all-pruned sparse vector).
	m, err = ParseSparse(`{}`)
	if err != nil || len(m) != 0 {
		t.Fatalf("empty object: %v %v", m, err)
	}
}

func TestParseSparseRejectsBadWeights(t *testing.T) {
	for _, bad := range []string{
		`{"x":"NaN"}`, // non-finite via string
		`{"x":"Inf"}`, // infinity via string
		`{"x":-1}`,    // negative number
		`{"x":0}`,     // zero: rank_features wants positive
		`{"x":"0"}`,   // zero via string
		`{"x":"abc"}`, // non-numeric string
		`{"x":true}`,  // neither number nor string
		`{"x":null}`,  // null weight
		`{x:1}`,       // malformed JSON
	} {
		if _, err := ParseSparse(bad); err == nil {
			t.Fatalf("%s must be rejected", bad)
		}
	}
}
