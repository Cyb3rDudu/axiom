// Unit tests for profile canonicalization (no PostgreSQL needed): strict decode
// rejects unknown keys and wrong types, semantically-equivalent profiles share
// one canonical hash, and the canonical form is what gets hashed (never raw JSON).
package repo

import "testing"

func TestDecodeProcessingRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name    string
		profile string
	}{
		{"unknown key", `{"profile":"x","bogus":true}`},
		{"wrong boolean type", `{"profile":"x","extract_images":"yes"}`},
		{"missing profile name", `{"language_hint":"de"}`},
		{"empty", ``},
		{"not an object", `"profile"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := decodeProcessing([]byte(c.profile)); err == nil {
				t.Errorf("decodeProcessing(%s) = nil error, want rejection", c.profile)
			}
		})
	}
}

func TestEquivalentProfilesShareOneHash(t *testing.T) {
	// Different key order must canonicalize to the same bytes and hash.
	a := []byte(`{"profile":"full-rag-v1","extract_images":true,"language_hint":"de"}`)
	b := []byte(`{"extract_images":true,"language_hint":"de","profile":"full-rag-v1"}`)
	ca, ha, errA := profileCanonical(a)
	cb, hb, errB := profileCanonical(b)
	if errA != nil || errB != nil {
		t.Fatalf("canonical errors: %v / %v", errA, errB)
	}
	if string(ca) != string(cb) {
		t.Errorf("canonical bytes differ for equivalent profiles:\n %s\n %s", ca, cb)
	}
	if ha != hb {
		t.Errorf("hash differs for equivalent profiles: %s vs %s", ha, hb)
	}
}

func TestHashIsOverCanonicalFormNotRawJSON(t *testing.T) {
	// The hash must reflect the canonical FrozenProcessing, so reordering keys or
	// adding ignorable whitespace must not change it, and an unknown field must
	// be rejected (never hashed).
	_, h1, err := profileCanonical([]byte(`{"profile":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, h2, err := profileCanonical([]byte("{\n  \"profile\": \"x\"\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("whitespace changed the hash: %s vs %s", h1, h2)
	}
}
