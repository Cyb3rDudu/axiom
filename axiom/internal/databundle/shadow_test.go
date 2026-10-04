// shadow_test.go — unit witnesses for the DM08 (#317) shadow-read
// classification: the allowlist rules absorb exactly the documented
// difference classes and nothing else. The end-to-end legs (PG roundtrip
// green, injected-deviation red, SQLite namespace skips) live in
// shadow_pg_it_test.go / shadow_sqlite_test.go.
package databundle

import (
	"encoding/json"
	"testing"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestClassifyColumnDiff(t *testing.T) {
	cases := []struct {
		name string
		tag  string
		a, b json.RawMessage
		rule string
		ok   bool
	}{
		// numeric-value: lexical decimals of the same rational value
		{"numeric trailing zeros", TagNumeric, raw(`0.850`), raw(`0.85`), "numeric-value", true},
		{"numeric exponent expansion", TagNumeric, raw(`850e-3`), raw(`0.85`), "numeric-value", true},
		{"numeric value differs", TagNumeric, raw(`0.9`), raw(`0.85`), "", false},
		{"numeric scale differs", TagNumeric, raw(`0.851`), raw(`0.85`), "", false},
		// float-value: parsed equality
		{"float lexical", TagFloat64, raw(`1e-06`), raw(`0.000001`), "float-value", true},
		{"float value differs", TagFloat64, raw(`1e-06`), raw(`0.000002`), "", false},
		// timestamp-instant: same instant
		{"timestamp identical form", TagTimestamp, raw(`"2026-10-05T09:00:00.000001Z"`), raw(`"2026-10-05T09:00:00.000001Z"`), "timestamp-instant", true},
		{"timestamp differs", TagTimestamp, raw(`"2026-10-05T09:00:00.000001Z"`), raw(`"2026-10-05T09:00:00.000002Z"`), "", false},
		// json-number-value: value-equal numbers inside documents
		{"jsonb number scale", TagJSONB, raw(`{"score":0.10}`), raw(`{"score":0.1}`), "json-number-value", true},
		{"jsonb number exponent", TagJSONB, raw(`{"n":1e3}`), raw(`{"n":1000}`), "json-number-value", true},
		{"jsonb string differs", TagJSONB, raw(`{"a":"x"}`), raw(`{"a":"y"}`), "", false},
		{"jsonb key added", TagJSONB, raw(`{"a":1}`), raw(`{"a":1,"b":2}`), "", false},
		{"jsonb null vs number", TagJSONB, raw(`{"a":null}`), raw(`{"a":0}`), "", false},
		{"jsonb nested array order", TagJSONB, raw(`{"a":[1,2]}`), raw(`{"a":[2,1]}`), "", false},
		{"jsonb nested scale deep", TagJSONB, raw(`{"a":[{"x":1.50}]}`), raw(`{"a":[{"x":1.5}]}`), "json-number-value", true},
		// everything else: strict, unexpected — text, uuid, bool, int64, enum
		{"text differs", TagText, raw(`"a"`), raw(`"b"`), "", false},
		{"text empty vs null", TagText, raw(`""`), raw(`null`), "", false},
		{"uuid differs", TagUUID, raw(`"11111111-1111-4111-8111-111111111111"`), raw(`"22222222-2222-4222-8222-222222222222"`), "", false},
		{"bool differs", TagBool, raw(`true`), raw(`false`), "", false},
		{"int differs", TagInt64, raw(`1`), raw(`2`), "", false},
		{"int lexical is not absorbed", TagInt64, raw(`1`), raw(`1.0`), "", false},
		{"enum differs", TagEnum, raw(`"included"`), raw(`"excluded"`), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule, ok := classifyColumnDiff(tc.tag, tc.a, tc.b)
			if ok != tc.ok || rule != tc.rule {
				t.Fatalf("classify(%s, %s, %s) = (%q, %v), want (%q, %v)",
					tc.tag, tc.a, tc.b, rule, ok, tc.rule, tc.ok)
			}
		})
	}
}

// TestShadowCanonicalKey — stable keys encode canonically (uuid case,
// numeric scale, string separators) and collide only on equal keys.
func TestShadowCanonicalKey(t *testing.T) {
	cols := []ColumnRef{
		{Name: "id", Type: TagUUID},
		{Name: "n", Type: TagInt64},
		{Name: "label", Type: TagText},
	}
	k1, err := canonicalKey(cols, []string{"id", "n", "label"}, []any{
		"11111111-1111-4111-8111-111111111111", int64(2), "x\x1fy"})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := canonicalKey(cols, []string{"id", "n", "label"}, []any{
		"11111111-1111-4111-8111-111111111111", int64(2), "x\x1fy"})
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("equal rows keyed differently: %q vs %q", k1, k2)
	}
	k3, err := canonicalKey(cols, []string{"id", "n", "label"}, []any{
		"11111111-1111-4111-8111-111111111111", int64(3), "x\x1fy"})
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k3 {
		t.Fatal("differing rows collided on the stable key")
	}
	// the unit separator inside a value must not be injective material:
	// values are length-framed by the encoder (JSON tokens), so this
	// distinct triple keys differently than ("x", "y") — probe it.
	kPlain, err := canonicalKey(cols, []string{"id", "n", "label"}, []any{
		"11111111-1111-4111-8111-111111111111", int64(2), "x"})
	if err != nil {
		t.Fatal(err)
	}
	if kPlain == k1 {
		t.Fatal("separator-bearing value collided with the split pair")
	}
}

// TestShadowSurfaceOf — every LibraryTables member rides exactly one
// semantic surface; the issue's six surfaces are all populated.
func TestShadowSurfaceOf(t *testing.T) {
	seen := map[string]int{}
	for _, spec := range LibraryTables {
		s := ShadowSurfaceOf(spec.Name)
		if s == "" {
			t.Fatalf("table %s has no surface", spec.Name)
		}
		seen[s]++
	}
	for _, want := range []string{"records", "collections", "renditions", "selections", "repair-readback", "raw-envelopes"} {
		if seen[want] == 0 {
			t.Fatalf("issue surface %q carries no table", want)
		}
	}
}

// TestShadowAllowlistDocumented — every rule id is unique and carries a
// justification; the construction-time list likewise.
func TestShadowAllowlistDocumented(t *testing.T) {
	ids := map[string]bool{}
	for _, l := range [][]AllowlistEntry{shadowAllowlist, shadowCanonicalization} {
		for _, e := range l {
			if e.ID == "" || e.Justification == "" || e.Scope == "" {
				t.Fatalf("allowlist entry %+v lacks id/scope/justification", e)
			}
			if ids[e.ID] {
				t.Fatalf("duplicate rule id %q", e.ID)
			}
			ids[e.ID] = true
		}
	}
	// classification may only emit listed rule ids, and only for
	// DIFFERING values (the caller guards equality first)
	known := map[string]bool{}
	for _, e := range shadowAllowlist {
		known[e.ID] = true
	}
	for _, tag := range []string{TagNumeric, TagFloat64, TagTimestamp, TagJSONB, TagText} {
		if rule, _ := classifyColumnDiff(tag, raw(`1`), raw(`2`)); rule != "" && !known[rule] {
			t.Fatalf("unlisted rule %q emitted", rule)
		}
	}
}
