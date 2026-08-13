package dispatcher

// Unit tests for the Gate-4 wiring helpers (capDim, processorIdentity). These
// prove the capability-derived values are pulled from the negotiated
// /v1/capabilities response, not hardcoded — the specific wiring correctness
// claim Hivemind flagged for the dispatcher-persistence integration.

import (
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

func TestCapDimFromCapabilitiesInt(t *testing.T) {
	d := &Dispatcher{caps: &processor.Capabilities{
		Models: map[string]any{
			"dense_embedding": map[string]any{"name": "bge-m3", "dimensions": 1024},
		},
	}}
	if got := d.capDim(); got != 1024 {
		t.Fatalf("capDim = %d, want 1024 (must come from capabilities, not hardcoded)", got)
	}
}

func TestCapDimFloatFromCapabilities(t *testing.T) {
	// JSON unmarshal into map[string]any yields float64 for numbers; the helper
	// must coerce cleanly to int.
	d := &Dispatcher{caps: &processor.Capabilities{
		Models: map[string]any{
			"dense_embedding": map[string]any{"dimensions": float64(768)},
		},
	}}
	if got := d.capDim(); got != 768 {
		t.Fatalf("capDim (float64 source) = %d, want 768", got)
	}
}

func TestCapDimNoStringFallback(t *testing.T) {
	// Hivemind Gate-3 hint: dimensions is an int — a string value must NOT be
	// coerced. capDim returns 0 (validation skips the dim check) rather than
	// silently atoi-ing a string.
	d := &Dispatcher{caps: &processor.Capabilities{
		Models: map[string]any{
			"dense_embedding": map[string]any{"dimensions": "1024"},
		},
	}}
	if got := d.capDim(); got != 0 {
		t.Fatalf("capDim (string source) = %d, want 0 (no string fallback)", got)
	}
}

func TestCapDimMissingOrMalformed(t *testing.T) {
	for name, caps := range map[string]*processor.Capabilities{
		"nil caps":           nil,
		"no dense_embedding": {Models: map[string]any{}},
		"dense not a map":    {Models: map[string]any{"dense_embedding": "oops"}},
		"no dimensions key":  {Models: map[string]any{"dense_embedding": map[string]any{"name": "x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			d := &Dispatcher{caps: caps}
			if got := d.capDim(); got != 0 {
				t.Fatalf("capDim (%s) = %d, want 0", name, got)
			}
		})
	}
}

func TestProcessorIdentityFromCapabilities(t *testing.T) {
	d := &Dispatcher{caps: &processor.Capabilities{}}
	d.caps.Processor.Name = "axiom-python-marker"
	d.caps.Processor.Version = "0.2.0"
	name, ver := d.processorIdentity()
	if name != "axiom-python-marker" || ver != "0.2.0" {
		t.Fatalf("processorIdentity = (%q,%q), want (axiom-python-marker,0.2.0)", name, ver)
	}
}

func TestProcessorIdentityMissingCaps(t *testing.T) {
	d := &Dispatcher{caps: nil}
	name, ver := d.processorIdentity()
	if name != "unknown" || ver != "unknown" {
		t.Fatalf("processorIdentity (nil caps) = (%q,%q), want (unknown,unknown)", name, ver)
	}
}
