// canonical.go — the canonical value codec: type tags, canonical JSON,
// timestamps, and the row encoder whose output IS the digest input.
package databundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Canonical type tags — the closed vocabulary of the bundle format.
// Unknown types abort export (the format must stay portable, not
// lossy).
const (
	TagUUID      = "uuid"
	TagText      = "text"
	TagTimestamp = "timestamp"
	TagBool      = "bool"
	TagInt64     = "int64"
	TagFloat64   = "float64"
	TagJSONB     = "jsonb"
	TagNumeric   = "numeric"
	TagEnum      = "enum"
)

// tsForm is THE canonical timestamp form: UTC, fixed microseconds —
// lexicographic order equals chronological order (the SQLite dialect's
// documented DM03 form).
const tsForm = "2006-01-02T15:04:05.000000Z"

// canonicalTimestamp renders t in the canonical form.
func canonicalTimestamp(t time.Time) string {
	return t.UTC().Format(tsForm)
}

// parseCanonicalTimestamp accepts the canonical form (and nothing
// looser): no local-timezone timestamps exist in a bundle.
func parseCanonicalTimestamp(s string) (time.Time, error) {
	t, err := time.Parse(tsForm, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not the canonical UTC RFC3339µs form", s)
	}
	return t, nil
}

// normalizeUUID canonicalizes a UUID to its lowercase hyphenated form.
func normalizeUUID(s string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(s))
	if len(l) != 36 || l[8] != '-' || l[13] != '-' || l[18] != '-' || l[23] != '-' {
		return "", fmt.Errorf("uuid %q is not the canonical 36-char form", s)
	}
	for _, r := range l {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == '-') {
			return "", fmt.Errorf("uuid %q has non-hex characters", s)
		}
	}
	return l, nil
}

// encodeCanonical writes v as canonical JSON: object keys sorted
// recursively, compact separators, numbers as their verbatim tokens.
// Equal values produce equal bytes regardless of input key order or
// whitespace — the permutation-stability the digests rely on.
func encodeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case string:
		b, err := json.Marshal(x) // deterministic Go escaping
		if err != nil {
			return err
		}
		buf.Write(b)
	case json.Number:
		s := x.String()
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return fmt.Errorf("number token %q is not a valid JSON number", s)
		}
		buf.WriteString(s)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("non-finite float %v cannot be canonicalized", x)
		}
		buf.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case int:
		buf.WriteString(strconv.Itoa(x))
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		buf.WriteByte('{')
		for i, k := range sortedKeys(x) {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := encodeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("value of type %T is not canonicalizable", v)
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// CanonicalizeJSON re-encodes raw JSON canonically (sorted keys,
// compact, verbatim number tokens). The bundle's jsonb mapping.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("json decode: %w", err)
	}
	// Trailing non-whitespace = two documents; a jsonb value is one.
	if dec.More() {
		return nil, fmt.Errorf("json input carries more than one value")
	}
	var buf bytes.Buffer
	if err := encodeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeRow writes one row as a canonical JSON object line: columns in
// catalog order, values canonically encoded under their tag. nil means
// SQL NULL -> JSON null. The returned bytes are the digest input.
func EncodeRow(buf *bytes.Buffer, cols []ColumnRef, values []any) error {
	if len(cols) != len(values) {
		return fmt.Errorf("row arity %d does not match catalog %d", len(values), len(cols))
	}
	buf.WriteByte('{')
	for i, c := range cols {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(c.Name)
		buf.Write(kb)
		buf.WriteByte(':')
		if err := encodeTagged(buf, c.Type, values[i]); err != nil {
			return fmt.Errorf("column %s: %w", c.Name, err)
		}
	}
	buf.WriteByte('}')
	buf.WriteByte('\n')
	return nil
}

// encodeTagged encodes one column value under its canonical tag. The
// tag closes the NULL-vs-empty-vs-type ambiguity: nil is SQL NULL,
// "" is an empty string, and every non-nil value must match the tag.
func encodeTagged(buf *bytes.Buffer, tag string, v any) error {
	if v == nil {
		buf.WriteString("null")
		return nil
	}
	switch tag {
	case TagUUID:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("uuid value %T is not a string", v)
		}
		n, err := normalizeUUID(s)
		if err != nil {
			return err
		}
		kb, _ := json.Marshal(n)
		buf.Write(kb)
	case TagText, TagEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s value %T is not a string", tag, v)
		}
		kb, _ := json.Marshal(s)
		buf.Write(kb)
	case TagTimestamp:
		var s string
		switch t := v.(type) {
		case time.Time:
			s = canonicalTimestamp(t)
		case string:
			parsed, err := parseCanonicalTimestamp(t)
			if err != nil {
				return err
			}
			s = canonicalTimestamp(parsed) // re-format: canonical in, canonical out
		default:
			return fmt.Errorf("timestamp value %T is neither time nor string", v)
		}
		kb, _ := json.Marshal(s)
		buf.Write(kb)
	case TagBool:
		b, ok := v.(bool)
		if !ok {
			// SQLite stores booleans as 0/1 integers.
			if n, isInt := v.(int64); isInt && (n == 0 || n == 1) {
				b = n == 1
			} else {
				return fmt.Errorf("bool value %T is neither bool nor 0/1", v)
			}
		}
		buf.WriteString(strconv.FormatBool(b))
	case TagInt64:
		switch n := v.(type) {
		case int64:
			buf.WriteString(strconv.FormatInt(n, 10))
		case int:
			buf.WriteString(strconv.Itoa(n))
		case int32:
			buf.WriteString(strconv.FormatInt(int64(n), 10))
		default:
			return fmt.Errorf("int64 value %T is not an integer", v)
		}
	case TagFloat64:
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("float64 value %T is not a float", v)
		}
		return encodeCanonical(buf, f)
	case TagNumeric:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("numeric value %T is not a decimal token", v)
		}
		return encodeCanonical(buf, json.Number(s))
	case TagJSONB:
		var raw []byte
		switch j := v.(type) {
		case []byte:
			raw = j
		case string:
			raw = []byte(j)
		default:
			return fmt.Errorf("jsonb value %T is neither bytes nor string", v)
		}
		can, err := CanonicalizeJSON(raw)
		if err != nil {
			return err
		}
		buf.Write(can)
	default:
		return fmt.Errorf("unknown canonical tag %q", tag)
	}
	return nil
}

// DecodeRow parses one JSONL line into ordered values per the column
// catalog, validating: exactly the catalog columns present (missing or
// extra = loud), values type-checked per tag, enum values ∈ vocab,
// canonical timestamps parsed (not trusted), jsonb re-canonicalized
// (asymmetric input is normalized, then byte-compared upstream).
// rowNum is 1-based within the table, for error positions.
func DecodeRow(line []byte, cols []ColumnRef, enums map[string][]string, rowNum int64) ([]any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	m := make(map[string]any)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("row %d: json decode: %w", rowNum, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("row %d: trailing content after the JSON object", rowNum)
	}
	if len(m) != len(cols) {
		return nil, fmt.Errorf("row %d: %d columns, catalog declares %d (missing/extra columns are a structural fault)", rowNum, len(m), len(cols))
	}
	vals := make([]any, len(cols))
	for i, c := range cols {
		raw, ok := m[c.Name]
		if !ok {
			return nil, fmt.Errorf("row %d: column %s missing from the bundle", rowNum, c.Name)
		}
		v, err := decodeTagged(c, raw, enums)
		if err != nil {
			return nil, fmt.Errorf("row %d, column %s: %w", rowNum, c.Name, err)
		}
		vals[i] = v
	}
	return vals, nil
}

func decodeTagged(c ColumnRef, raw any, enums map[string][]string) (any, error) {
	// jsonb first: the JSON value null under a jsonb column is the
	// jsonb VALUE 'null' — NOT SQL NULL. Every jsonb column in the
	// Library schema is NOT NULL (the export refuses nullable jsonb
	// columns loudly), so a null token under jsonb can only be the
	// value — the 32 production documents carrying creators='null'
	// proved this is real data, not a corner.
	if c.Type == TagJSONB {
		if raw == nil {
			return []byte("null"), nil
		}
		var buf bytes.Buffer
		if err := encodeCanonical(&buf, raw); err != nil {
			return nil, fmt.Errorf("jsonb: %w", err)
		}
		return buf.Bytes(), nil
	}
	if raw == nil {
		return nil, nil
	}
	// Vocabulary gate FIRST, for every column that has one — enum-TYPE
	// columns and the component's text status columns alike: unknown
	// enum/status aborts loudly, never a silent default.
	if vocab := enums[c.Name]; vocab != nil {
		if s, ok := raw.(string); ok && !slices.Contains(vocab, s) {
			return nil, fmt.Errorf("value %q is not in the declared vocabulary (unknown enum/status aborts loudly, no silent default)", s)
		}
	}
	switch c.Type {
	case TagUUID:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("uuid is not a string")
		}
		return normalizeUUID(s)
	case TagText:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("text is not a string")
		}
		return s, nil
	case TagTimestamp:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("timestamp is not a string")
		}
		return parseCanonicalTimestamp(s)
	case TagBool:
		b, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("bool is not a JSON boolean")
		}
		return b, nil
	case TagInt64:
		n, ok := raw.(json.Number)
		if !ok {
			return nil, fmt.Errorf("int64 is not a JSON number")
		}
		return strconv.ParseInt(n.String(), 10, 64)
	case TagFloat64:
		n, ok := raw.(json.Number)
		if !ok {
			return nil, fmt.Errorf("float64 is not a JSON number")
		}
		return n.Float64()
	case TagNumeric:
		n, ok := raw.(json.Number)
		if !ok {
			return nil, fmt.Errorf("numeric is not a JSON number")
		}
		return n.String(), nil // verbatim decimal token
	case TagEnum:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("enum is not a string")
		}
		return s, nil // vocabulary already gated above
	default:
		return nil, fmt.Errorf("unknown canonical tag %q", c.Type)
	}
}

// WireValues converts decoded canonical values to engine insert
// parameters for the SQLite writer (PG builds its own via pgWire).
func sqliteWire(cols []ColumnRef, vals []any) ([]any, error) {
	out := make([]any, len(vals))
	for i, c := range cols {
		v := vals[i]
		if v == nil {
			out[i] = nil
			continue
		}
		switch c.Type {
		case TagBool:
			b := v.(bool)
			if b {
				out[i] = int64(1)
			} else {
				out[i] = int64(0)
			}
		case TagFloat64:
			out[i] = v.(float64)
		case TagTimestamp:
			// store the canonical string — the driver's own time.Time
			// binding uses a different format and would break verify
			out[i] = canonicalTimestamp(v.(time.Time))
		case TagJSONB:
			out[i] = string(v.([]byte))
		default:
			out[i] = v // string, int64 stay
		}
	}
	return out, nil
}
