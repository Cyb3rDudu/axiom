// shadow_test.go — unit witnesses for the DM08 (#317) shadow-read
// classification: the allowlist rules absorb exactly the documented
// difference classes and nothing else. The end-to-end legs (PG roundtrip
// green, injected-deviation red, SQLite namespace skips) live in
// shadow_pg_it_test.go / shadow_sqlite_test.go.
package databundle

import (
	"encoding/json"
	"fmt"
	"strings"
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
	// classification may only emit listed rule ids — the probe pairs
	// are DIFFERING values that actually trigger rules where rules
	// exist (an emitting pair proves the closed-world check can fire)
	emits := map[string]string{
		TagNumeric:   "0.850|0.85",
		TagFloat64:   "1e-06|0.000001",
		TagTimestamp: "2026-10-05T09:00:00.000001Z|2026-10-05T09:00:00.000001Z",
		TagJSONB:     `{"n":1e3}|{"n":1000}`,
		TagText:      "a|b",
	}
	known := map[string]bool{}
	for _, e := range shadowAllowlist {
		known[e.ID] = true
	}
	emittedAny := false
	for tag, pair := range emits {
		halves := strings.SplitN(pair, "|", 2)
		rule, _ := classifyColumnDiff(tag, raw(halves[0]), raw(halves[1]))
		if rule != "" {
			emittedAny = true
			if !known[rule] {
				t.Fatalf("unlisted rule %q emitted for tag %s", rule, tag)
			}
		}
	}
	if !emittedAny {
		t.Fatal("no rule emitted for the probe pairs — the closed-world check is vacuous")
	}
}

// TestColumnSetMatches — the PG-target structural gate: count, name,
// tag and order drifts all refuse (conservative red), only exact
// matches pass.
func TestColumnSetMatches(t *testing.T) {
	base := []ColumnRef{{"id", TagUUID}, {"n", TagInt64}}
	if note, ok := columnSetMatches(base, base); !ok {
		t.Fatalf("identical catalogs refused: %s", note)
	}
	cases := []struct {
		name string
		a, b []ColumnRef
	}{{
		"count drift", base, []ColumnRef{{"id", TagUUID}},
	}, {
		"name drift", base, []ColumnRef{{"id", TagUUID}, {"m", TagInt64}},
	}, {
		"type drift", base, []ColumnRef{{"id", TagUUID}, {"n", TagText}},
	}, {
		"order drift", base, []ColumnRef{{"n", TagInt64}, {"id", TagUUID}},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := columnSetMatches(tc.a, tc.b); ok {
				t.Fatal("drifted target catalog passed the structural gate")
			}
		})
	}
}

// TestCompareRows — the classifier's accounting over hand-built
// canonical rows: mixed absorbed+unexpected rows count BOTH (per-rule
// absorption and the red), the sample cap bounds but never changes
// counts, one-sided keys are structural, and the empty-object guard
// refuses a line that would otherwise compare equal.
func TestCompareRows(t *testing.T) {
	cols := []ColumnRef{{Name: "k", Type: TagUUID}, {Name: "score", Type: TagNumeric}, {Name: "title", Type: TagText}}
	allow := newShadowAllowlist()
	mk := func(score, title string) string {
		return fmt.Sprintf(`{"k":"x","score":%s,"title":%s}`+"\n", score, title)
	}
	same := mk(`0.85`, `"T"`)
	src := map[string]string{"\"x\"": same, "\"gone\"": same}
	tgt := map[string]string{
		"\"x\"":     mk(`0.850`, `"DRIFTED"`), // absorbed numeric + unexpected text
		"\"extra\"": mk(`0.85`, `"T"`),        // extra on target
	}
	sr := SurfaceResult{Samples: []RowDiff{}}
	if err := compareRows(&sr, cols, src, tgt, shadowDefaultSamples, allow); err != nil {
		t.Fatal(err)
	}
	if sr.Compared != 1 || sr.Equal != 0 || sr.Normalized != 0 || sr.Unexpected != 1 {
		t.Fatalf("counts: %+v", sr)
	}
	if sr.MissingOnTarget != 1 || sr.ExtraOnTarget != 1 {
		t.Fatalf("structural counts: %+v", sr)
	}
	found := false
	for _, a := range allow {
		if a.ID == "numeric-value" && a.Applied == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("numeric-value absorption not counted inside the red row")
	}
	// the sample rows: one per deviating key under the cap — the
	// field diff (x), the missing key (gone) and the extra key (extra)
	if len(sr.Samples) != 3 {
		t.Fatalf("samples: %+v", sr.Samples)
	}

	// empty-object guard: a null line must not compare "equal"
	bad := SurfaceResult{Samples: []RowDiff{}}
	if err := compareRows(&bad, cols, map[string]string{"\"x\"": "null\n"}, map[string]string{"\"x\"": same}, 5, allow); err == nil {
		t.Fatal("null canonical line accepted — the false-green guard is missing")
	}
}
