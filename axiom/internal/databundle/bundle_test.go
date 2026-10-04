// bundle_test.go — engine-free format tests (DM03 #312): canonical
// JSON permutation stability, number-token preservation, timestamp
// form, NULL/missing/empty distinction, enum teeth, manifest
// tamper-evidence (bit-flip), batch digest isolation. No database
// needed — the bundle is files, its codec is pure.
package databundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// permuteKeys recursively reverses every object's key order (a
// worst-case permutation for byte-oriented encoders).
func permuteKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		for i := len(keys) - 1; i >= 0; i-- { // reversed
			out[keys[i]] = permuteKeys(x[keys[i]])
		}
		return out
	case []any:
		for i := range x {
			x[i] = permuteKeys(x[i])
		}
		return x
	default:
		return v
	}
}

func decodeJSON(t *testing.T, raw string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// TestCanonicalJSONPermutationStable — the DM03 acceptance: key order
// is irrelevant to hashes. Same value, recursively permuted key order,
// identical canonical bytes.
func TestCanonicalJSONPermutationStable(t *testing.T) {
	raw := `{"zotero-key":"ABC123","data":{"itemType":"report","title":"x","creators":[{"first":"A","last":"B"}],"nested":{"b":1,"a":[3,2,{"q":false,"p":null}]}},"version":42}`
	orig := decodeJSON(t, raw)
	permuted := permuteKeys(decodeJSON(t, raw))

	c1, err := CanonicalizeJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	// re-encode the permuted tree through Go's own marshaler (different
	// key order in the byte stream) and canonicalize THAT
	pb, err := json.Marshal(permuted)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := CanonicalizeJSON(pb)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c1, c2) {
		t.Fatalf("canonical form is not permutation-stable:\n%s\n%s", c1, c2)
	}
	if !jsonEqual(t, decodeJSON(t, string(c1)), orig) {
		t.Fatal("canonicalization changed the value")
	}
	// and stable under repeated canonicalization (idempotent)
	c3, err := CanonicalizeJSON(c1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c1, c3) {
		t.Fatal("canonicalization is not idempotent")
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ab, _ := canonicalBytes(a)
	bb, _ := canonicalBytes(b)
	return bytes.Equal(ab, bb)
}

func canonicalBytes(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TestCanonicalJSONNumberTokenPreserved — int64-scale numbers survive
// without float round-trips (the classic JSONB portability loss), and
// tokens BEYOND float64 range stay verbatim (legal PostgreSQL numeric
// — lexical validation, never ParseFloat).
func TestCanonicalJSONNumberTokenPreserved(t *testing.T) {
	raw := `{"id":9223372036854775807,"small":0.100000,"exp":1e3,"huge":1e400}`
	c, err := CanonicalizeJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"9223372036854775807", "0.100000", "1e3", "1e400"} {
		if !bytes.Contains(c, []byte(want)) {
			t.Fatalf("token %s mangled: %s", want, c)
		}
	}
	// invalid number grammar still refused
	if _, err := CanonicalizeJSON([]byte(`{"x":01}`)); err == nil {
		t.Fatal("leading-zero number accepted")
	}
	if _, err := CanonicalizeJSON([]byte(`{"x":1.}`)); err == nil {
		t.Fatal("trailing-dot number accepted")
	}
}

// TestCanonicalJSONRejectsGarbage — two documents or broken JSON is a
// loud error, never a best-effort prefix.
func TestCanonicalJSONRejectsGarbage(t *testing.T) {
	for _, raw := range []string{`{"a":1} {"b":2}`, `{`, ``, `nul`} {
		if _, err := CanonicalizeJSON([]byte(raw)); err == nil {
			t.Fatalf("canonicalize %q unexpectedly succeeded", raw)
		}
	}
}

// TestTimestampCanonicalForm — µs UTC, no local timezone, roundtrips.
func TestTimestampCanonicalForm(t *testing.T) {
	in := time.Date(2026, 10, 5, 14, 3, 9, 123456000, time.UTC)
	s := canonicalTimestamp(in)
	want := "2026-10-05T14:03:09.123456Z"
	if s != want {
		t.Fatalf("got %s want %s", s, want)
	}
	back, err := parseCanonicalTimestamp(s)
	if err != nil || !back.Equal(in) {
		t.Fatalf("roundtrip failed: %v %v", back, err)
	}
	// A +02:00 local form is refused — no local-timezone timestamps.
	if _, err := parseCanonicalTimestamp("2026-10-05T16:03:09.123456+02:00"); err == nil {
		t.Fatal("local-offset timestamp accepted")
	}
	// Sub-µs precision does not silently truncate.
	if _, err := parseCanonicalTimestamp("2026-10-05T14:03:09.1234567Z"); err == nil {
		t.Fatal("sub-microsecond timestamp accepted")
	}
}

// testCols is a small catalog for codec tests.
func testCols() []ColumnRef {
	return []ColumnRef{
		{Name: "id", Type: TagUUID},
		{Name: "title", Type: TagText},
		{Name: "deleted", Type: TagBool},
		{Name: "count", Type: TagInt64},
		{Name: "score", Type: TagNumeric},
		{Name: "conf", Type: TagFloat64},
		{Name: "at", Type: TagTimestamp},
		{Name: "meta", Type: TagJSONB},
		{Name: "status", Type: TagEnum},
	}
}

func testEnums() map[string][]string {
	return map[string][]string{"status": {"queued", "healed", "blocked_for_dudu"}}
}

// TestRowEncodingRoundtrip — encode then decode yields identical
// canonical bytes and values (the digest basis).
func TestRowEncodingRoundtrip(t *testing.T) {
	cols := testCols()
	vals := []any{
		"0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e",
		"A Title With Ünicode",
		true,
		int64(-7),
		"0.850",
		0.5,
		time.Date(2026, 1, 2, 3, 4, 5, 6000000, time.UTC),
		[]byte(`{"b":2,"a":1}`),
		"blocked_for_dudu",
	}
	var buf bytes.Buffer
	if err := EncodeRow(&buf, cols, vals); err != nil {
		t.Fatal(err)
	}
	line := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	back, err := DecodeRow(line, cols, testEnums(), 1)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var buf2 bytes.Buffer
	if err := EncodeRow(&buf2, cols, back); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Fatalf("re-encoded row differs:\n%s\n%s", buf.Bytes(), buf2.Bytes())
	}
	// numeric token survived verbatim
	if !bytes.Contains(buf.Bytes(), []byte(`"score":0.850`)) {
		t.Fatalf("numeric token mangled: %s", buf.Bytes())
	}
	// jsonb got canonicalized (sorted keys) inside the row
	if !bytes.Contains(buf.Bytes(), []byte(`"meta":{"a":1,"b":2}`)) {
		t.Fatalf("jsonb not canonical inside row: %s", buf.Bytes())
	}
}

// TestNullMissingEmptyDistinguishable — NULL is null, empty string is
// "", a missing column is a structural fault (loud).
func TestNullMissingEmptyDistinguishable(t *testing.T) {
	cols := []ColumnRef{{Name: "a", Type: TagText}, {Name: "b", Type: TagText}}
	vals := []any{nil, ""}
	var buf bytes.Buffer
	if err := EncodeRow(&buf, cols, vals); err != nil {
		t.Fatal(err)
	}
	if string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))) != `{"a":null,"b":""}` {
		t.Fatalf("null/empty not distinguishable: %s", buf.Bytes())
	}
	// missing column → loud decode error (arity check fires first,
	// naming the structural fault; the per-column check names the column)
	_, err := DecodeRow([]byte(`{"a":"x"}`), cols, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "catalog declares") {
		t.Fatalf("missing column not loud: %v", err)
	}
	// extra column → loud decode error
	_, err = DecodeRow([]byte(`{"a":"x","b":"y","c":"z"}`), cols, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "catalog declares") {
		t.Fatalf("extra column not loud: %v", err)
	}
	// NULL decodes back to nil
	back, err := DecodeRow([]byte(`{"a":null,"b":""}`), cols, nil, 1)
	if err != nil || back[0] != nil || back[1] != "" {
		t.Fatalf("null/empty decode broken: %v %v", back, err)
	}
}

// TestDecodeRowEnumTeeth — unknown enum/status aborts loudly, naming
// the vocabulary; no silent default.
func TestDecodeRowEnumTeeth(t *testing.T) {
	cols := []ColumnRef{{Name: "status", Type: TagEnum}}
	_, err := DecodeRow([]byte(`{"status":"queued"}`), cols, testEnums(), 3)
	if err != nil {
		t.Fatalf("known enum rejected: %v", err)
	}
	_, err = DecodeRow([]byte(`{"status":"bogus_status"}`), cols, testEnums(), 3)
	if err == nil || !strings.Contains(err.Error(), "unknown enum") {
		if err != nil && strings.Contains(err.Error(), "not in the declared vocabulary") {
			// the actual wording — check the row number is in there too
			if !strings.Contains(err.Error(), "row 3") {
				t.Fatalf("enum error lacks row position: %v", err)
			}
			return
		}
		t.Fatalf("unknown enum not loud: %v", err)
	}
}

// TestDecodeRowTypeTeeth — wrong JSON types under a tag are loud.
func TestDecodeRowTypeTeeth(t *testing.T) {
	cases := []struct {
		tag  string
		line string
	}{
		{TagUUID, `{"c":"NOT-A-UUID"}`},
		{TagBool, `{"c":"true"}`},
		{TagInt64, `{"c":1.5}`},
		{TagFloat64, `{"c":"0.5"}`},
		{TagTimestamp, `{"c":"2026-01-01"}`},
		{TagText, `{"c":7}`},
		{TagJSONB, `{"c":}`},
	}
	for _, tc := range cases {
		_, err := DecodeRow([]byte(tc.line), []ColumnRef{{Name: "c", Type: tc.tag}}, nil, 1)
		if err == nil {
			t.Fatalf("tag %s accepted %s", tc.tag, tc.line)
		}
	}
}

// TestJSONBNullValueVersusSQLNull — a JSON null under a jsonb column
// is the jsonb VALUE 'null' (legal under NOT NULL — production carries
// 32 documents with jsonb null in at least one document jsonb column,
// 18 of them in creators), never SQL NULL; the drill proved the
// SQL-NULL misreading breaks the import with 23502.
func TestJSONBNullValueVersusSQLNull(t *testing.T) {
	cols := []ColumnRef{{Name: "detail", Type: TagJSONB}}
	vals, err := DecodeRow([]byte(`{"detail":null}`), cols, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] == nil {
		t.Fatal("jsonb null decoded as SQL NULL — the 23502 drill regression")
	}
	if b, ok := vals[0].([]byte); !ok || string(b) != "null" {
		t.Fatalf("jsonb null decoded to %T %v, want []byte(\"null\")", vals[0], vals[0])
	}
	// re-encoding is byte-stable (digest basis)
	var buf bytes.Buffer
	if err := EncodeRow(&buf, cols, vals); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != `{"detail":null}` {
		t.Fatalf("re-encode: %s", got)
	}
}

// craftBundle writes a minimal consistent bundle for tamper tests.
func craftBundle(t *testing.T, dir string, rows [][]byte, mutate func(m *Manifest)) *Manifest {
	t.Helper()
	cols := []ColumnManifest{{Name: "id", Type: TagText}}
	tableDir := filepath.Join(dir, "tables", "library_imports")
	if err := os.MkdirAll(tableDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for _, r := range rows {
		buf.Write(r)
	}
	batchPath := filepath.Join(tableDir, "0000.jsonl")
	if err := os.WriteFile(batchPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Format:        FormatName,
		FormatVersion: FormatVersion,
		Component:     ComponentLib,
		CreatedAt:     "2026-10-05T00:00:00.000000Z",
		Source: SourceManifest{
			Build:        "axiom test",
			Engine:       "PostgreSQL 16.9",
			ExportCutoff: "2026-10-05T00:00:00.000000Z",
			Migrations:   map[string][]string{"library_schema_migrations": {"0001_library.sql"}},
		},
		Tables: []TableManifest{{
			Name:       "library_imports",
			Columns:    cols,
			Key:        []string{"id"},
			Count:      int64(len(rows)),
			Batches:    []BatchManifest{{File: "tables/library_imports/0000.jsonl", Count: int64(len(rows)), SHA256: fmt.Sprintf("%x", sha256.Sum256(buf.Bytes()))}},
			RowsSHA256: fmt.Sprintf("%x", sha256.Sum256(buf.Bytes())),
		}},
	}
	if mutate != nil {
		mutate(m)
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestManifestBitFlipRejected — a flipped byte in manifest.json breaks
// the sidecar pin: LoadManifest aborts before trusting inner digests.
func TestManifestBitFlipRejected(t *testing.T) {
	dir := t.TempDir()
	craftBundle(t, dir, [][]byte{[]byte("{\"id\":\"a\"}\n")}, nil)

	mpath := filepath.Join(dir, ManifestFile)
	raw, err := os.ReadFile(mpath)
	if err != nil {
		t.Fatal(err)
	}
	// flip one bit inside a digit of the cutoff
	flipped := bytes.Replace(raw, []byte("2026"), []byte("2027"), 1)
	if bytes.Equal(flipped, raw) {
		t.Fatal("flip no-op — test broken")
	}
	if err := os.WriteFile(mpath, flipped, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadManifest(dir)
	if err == nil || !strings.Contains(err.Error(), "tamper-evidence failed") {
		t.Fatalf("tampered manifest accepted: %v", err)
	}
}

// TestBatchBitFlipIsolated — a flipped byte in a batch file fails the
// batch digest BEFORE anything is applied; readBatch names both
// digests. The import-level wiring is proven against PostgreSQL in the
// IT suite; here the isolation primitive itself has teeth.
func TestBatchBitFlipIsolated(t *testing.T) {
	dir := t.TempDir()
	craftBundle(t, dir, [][]byte{[]byte("{\"id\":\"a\"}\n")}, nil)
	man, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	bpath := filepath.Join(dir, "tables", "library_imports", "0000.jsonl")
	if err := os.WriteFile(bpath, []byte("{\"id\":\"b\"}\n"), 0o644); err != nil { // same length, one value changed
		t.Fatal(err)
	}
	_, err = readBatch(dir, &man.Tables[0].Batches[0], &man.Tables[0])
	if err == nil || !strings.Contains(err.Error(), "DIGEST MISMATCH") {
		t.Fatalf("tampered batch accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "isolated") || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("mismatch does not document isolation: %v", err)
	}
}

// TestSidecarRemovedRefused — a bundle without its sidecar is not
// trusted (there is no integrity root left).
func TestSidecarRemovedRefused(t *testing.T) {
	dir := t.TempDir()
	craftBundle(t, dir, [][]byte{[]byte("{\"id\":\"a\"}\n")}, nil)
	if err := os.Remove(filepath.Join(dir, SidecarFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(dir); err == nil || !strings.Contains(err.Error(), SidecarFile) {
		t.Fatalf("sidecar-less bundle accepted: %v", err)
	}
}

// TestManifestValidateTeeth — structural invariants (unknown format
// version, keyless table, key column outside the catalog).
func TestManifestValidateTeeth(t *testing.T) {
	m := &Manifest{Format: FormatName, FormatVersion: 99, Component: ComponentLib}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "format_version") {
		t.Fatalf("future format accepted: %v", err)
	}
	m.FormatVersion = FormatVersion
	m.Tables = []TableManifest{{Name: "t", Columns: []ColumnManifest{{Name: "a", Type: TagText}}, Key: nil}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "no key columns") {
		t.Fatalf("keyless table accepted: %v", err)
	}
	m.Tables[0].Key = []string{"missing"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "key column") {
		t.Fatalf("key outside catalog accepted: %v", err)
	}
}

// TestUUIDCanonicalization — uppercase or padded UUIDs normalize; junk
// is refused.
func TestUUIDCanonicalization(t *testing.T) {
	got, err := normalizeUUID("0F14D0AB-960C-4A62-A9C4-A8F1A9E10D0E")
	if err != nil || got != "0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e" {
		t.Fatalf("normalize: %q %v", got, err)
	}
	for _, bad := range []string{"0f14d0ab960c4a62a9c4a8f1a9e10d0e", "0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0g", "", "{0f14d0ab-960c-4a62-a9c4-a8f1a9e10d0e}"} {
		if _, err := normalizeUUID(bad); err == nil {
			t.Fatalf("uuid %q accepted", bad)
		}
	}
}

// TestLibraryTableOrderComplete — every spec appears once, and the
// dependency order lists parents before children for the known FKs.
func TestLibraryTableOrderComplete(t *testing.T) {
	seen := map[string]int{}
	for i, t_ := range LibraryTables {
		if _, dup := seen[t_.Name]; dup {
			t.Fatalf("table %s listed twice", t_.Name)
		}
		seen[t_.Name] = i
	}
	// spot-check the load-bearing dependencies
	deps := [][2]string{
		{"zotero_sources", "zotero_documents"},
		{"zotero_documents", "zotero_attachments"},
		{"zotero_attachments", "repair_cases"},
		{"zotero_items", "zotero_item_collections"},
		{"zotero_collections", "zotero_item_collections"},
		{"repair_cases", "zotero_write_audit"},
		{"library_imports", "library_import_events"},
		{"library_imports", "library_import_steps"},
		{"library_imports", "library_metadata_provenance"},
	}
	for _, d := range deps {
		if seen[d[0]] >= seen[d[1]] {
			t.Fatalf("order: %s must precede %s", d[0], d[1])
		}
	}
	// the SQLite namespace is exactly the library_* set
	for _, t_ := range LibraryTables {
		if t_.SQLite != strings.HasPrefix(t_.Name, "library_") {
			t.Fatalf("table %s SQLite flag inconsistent with namespace", t_.Name)
		}
	}
}
