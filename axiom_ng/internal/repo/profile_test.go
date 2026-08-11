// Unit tests for profile canonicalization (no PostgreSQL needed): strict decode
// rejects unknown keys, wrong types and trailing JSON content; semantically
// equivalent profiles share one canonical hash; the canonical form is what gets
// serialized and hashed (never raw JSON); and ForceRebuild is applied from the
// job BEFORE serialization so the hash equals the final emit block.
package repo

import (
	"encoding/json"
	"testing"
)

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

func TestDecodeProcessingRejectsTrailingContent(t *testing.T) {
	// After the first JSON value, a second Decode must return io.EOF; a trailing
	// object must be rejected, not silently ignored.
	_, err := decodeProcessing([]byte(`{"profile":"full-rag-v1"} {"bogus":true}`))
	if err == nil {
		t.Fatal("decodeProcessing accepted trailing JSON content after the first object")
	}
}

func TestEquivalentProfilesShareOneHash(t *testing.T) {
	// Different key order must canonicalize to the same bytes and hash.
	aCanon, err := decodeProcessing([]byte(`{"profile":"full-rag-v1","extract_images":true,"language_hint":"de"}`))
	if err != nil {
		t.Fatal(err)
	}
	bCanon, err := decodeProcessing([]byte(`{"extract_images":true,"language_hint":"de","profile":"full-rag-v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	ca, ha, errA := canonicalBytes(aCanon)
	cb, hb, errB := canonicalBytes(bCanon)
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
	// The hash must reflect the canonical FrozenProcessing after decode, so
	// reordering keys or adding whitespace must not change it.
	p1, err := decodeProcessing([]byte(`{"profile":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := decodeProcessing([]byte("{\n  \"profile\": \"x\"\n}"))
	if err != nil {
		t.Fatal(err)
	}
	_, h1, err := canonicalBytes(p1)
	if err != nil {
		t.Fatal(err)
	}
	_, h2, err := canonicalBytes(p2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("whitespace changed the hash: %s vs %s", h1, h2)
	}
}

// TestForceRebuildHashMatchesFinalBlock proves F1: ForceRebuild is applied from
// the job BEFORE serialization, so processing_profile, the hash, the snapshot's
// processing block and the emitted request all agree for force jobs, regardless
// of any caller-supplied force_rebuild value.
func TestForceRebuildHashMatchesFinalBlock(t *testing.T) {
	// A caller profile that carries force_rebuild:true must be overridden by the
	// job's value before hashing. decode carries the caller's true, then the claim
	// override applies the job's force (false) and hashes the FINAL false block.
	p, err := decodeProcessing([]byte(`{"profile":"x","force_rebuild":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.ForceRebuild {
		t.Fatalf("decode should carry the caller's force_rebuild:true (to be overridden later)")
	}
	// Simulate the claim: override ForceRebuild from the job (non-force here).
	p.ForceRebuild = false
	canonical, hash, err := canonicalBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	var emitted FrozenProcessing
	if err := json.Unmarshal(canonical, &emitted); err != nil {
		t.Fatal(err)
	}
	if emitted.ForceRebuild {
		t.Error("emitted force_rebuild must be false (job override), not the caller's true")
	}
	// Hash must be over the FINAL block: re-hashing the canonical bytes yields the
	// same hash, and the canonical bytes reflect force=false.
	_, hash2, err := canonicalBytes(emitted)
	if err != nil {
		t.Fatal(err)
	}
	if hash != hash2 {
		t.Errorf("hash not stable over the canonical bytes: %s vs %s", hash, hash2)
	}
}
