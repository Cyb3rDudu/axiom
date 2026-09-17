// #252 outcome-truth derivation pins. Each subtest pins ONE branch of
// DeriveOutcome — mutation: flip or reorder any branch and its subtest(s)
// go red (that is the issue's mutation criterion, kept as plain asserts).
package repo

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDeriveOutcome(t *testing.T) {
	cases := []struct {
		name                          string
		selMode, jobStatus            string
		errCode, errMsg               string
		paginationState, repairStatus string
		wantOutcome, wantReason       string
	}{
		// excluded: selection beats everything — a deliberately held doc is
		// not "failed", and never "pending".
		{"excluded wins over failed job", "excluded", "failed", "X", "boom", "", "", "excluded", "selection-excluded"},
		{"excluded wins over completed job", "excluded", "completed", "", "", "", "", "excluded", "selection-excluded"},
		{"excluded beats pending job", "excluded", "pending", "", "", "", "", "excluded", "selection-excluded"},

		// running: pending / claimed / processing (issue: pending/processing while running)
		{"pending job", "", "pending", "", "", "", "", "pending", ""},
		{"claimed job", "", "claimed", "", "", "", "", "processing", ""},
		{"processing job", "", "processing", "", "", "", "", "processing", ""},
		{"running beats repair case", "", "claimed", "", "", "", "in_repair", "processing", ""},
		{"running beats needs_ocr", "", "claimed", "", "", "needs_ocr", "", "processing", ""},

		// needs_ocr: unpaginiert dead end from the #254 pagination_state —
		// explicitly BEFORE the repair track (unpaginiert cases sit rejected
		// forever; they must read needs_ocr, not in_repair).
		{"needs_ocr from pagination_state", "", "skipped", "SKIPPED", "preflight:unpaginiert", "needs_ocr", "", "needs_ocr", "scan-ohne-textlayer: text-less scan — OCR rebuild heals it (scan_ocr_rebuild, #284)"},
		{"needs_ocr beats rejected repair case", "", "skipped", "", "", "needs_ocr", "rejected", "needs_ocr", "scan-ohne-textlayer: text-less scan — OCR rebuild heals it (scan_ocr_rebuild, #284)"},
		{"physical_only is NOT needs_ocr", "", "completed", "", "", "physical_only", "", "completed", ""},

		// in_repair: live from the repair case status
		{"in_repair queued", "", "skipped", "SKIPPED", "preflight:reparierbar", "", "queued", "in_repair", "repair case: queued"},
		{"in_repair claimed by fix-service", "", "skipped", "", "", "", "in_repair", "in_repair", "repair case: in_repair"},
		{"in_repair rejected candidate", "", "skipped", "", "", "", "rejected", "in_repair", "repair case: rejected"},
		{"in_repair blocked for dudu", "", "skipped", "", "", "", "blocked_for_dudu", "in_repair", "repair case: blocked_for_dudu"},
		// an OPEN case is live even over a stale completed job: repair beats
		// completed (the repaired re-ingest has not landed yet).
		{"repair case beats completed job", "", "completed", "", "", "", "queued", "in_repair", "repair case: queued"},
		{"healed repair falls through to job", "", "completed", "", "", "", "healed", "completed", ""},
		// healed = CLOSED case: the job status is the truth until reprocessing
		// re-runs the doc (a healed repair over its own skipped job still reads
		// failed until the re-ingest completes).
		{"healed repair falls through to old skipped job", "", "skipped", "SKIPPED", "preflight:unpaginiert", "", "healed", "failed", "SKIPPED: preflight:unpaginiert"},
		{"failed repair case falls through to job reason", "", "skipped", "SKIPPED", "preflight:reparierbar", "", "failed", "failed", "SKIPPED: preflight:reparierbar"},

		// completed
		{"completed", "", "completed", "", "", "", "", "completed", ""},

		// failed + reason excerpt
		{"failed with code and message", "", "failed", "RETRY_EXHAUSTED", "lease expired", "", "", "failed", "RETRY_EXHAUSTED: lease expired"},
		{"skipped reason excerpt", "", "skipped", "SKIPPED", "preflight:no_text_layer", "", "", "failed", "SKIPPED: preflight:no_text_layer"},
		{"failed without message falls back to code", "", "failed", "SOURCE_URL_STALE", "", "", "", "failed", "SOURCE_URL_STALE"},
		{"cancelled without any reason falls back to status", "", "cancelled", "", "", "", "", "failed", "cancelled"},

		// no job at all: nothing happened yet — pending, not held-lie
		{"no job never enqueued", "", "", "", "", "", "", "pending", "never enqueued"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, gotReason := DeriveOutcome(c.selMode, c.jobStatus, c.errCode, c.errMsg, c.paginationState, c.repairStatus)
			if got != c.wantOutcome {
				t.Fatalf("outcome = %q, want %q", got, c.wantOutcome)
			}
			if gotReason != c.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, c.wantReason)
			}
		})
	}
}

// Long error excerpts are capped so the listing stays readable — rune-safe:
// the cap counts runes (160 + "…"), never splitting a multi-byte rune.
func TestDeriveOutcomeExcerptCap(t *testing.T) {
	long := strings.Repeat("äöü—", 100) // 2-byte and 3-byte runes: byte slicing would split them
	// no error_code: the reason IS the capped excerpt (160 runes + ellipsis)
	_, reason := DeriveOutcome("", "failed", "", long, "", "")
	if runes := len([]rune(reason)); runes > 162 {
		t.Fatalf("excerpt not capped: %d runes (want <= 162: 160 cap + ellipsis)", runes)
	}
	if !strings.HasSuffix(reason, "…") {
		t.Fatalf("capped excerpt must end with ellipsis, got tail %q", tail(reason))
	}
	// a byte-level cap would split a rune mid-sequence → invalid UTF-8
	if !utf8.ValidString(strings.TrimSuffix(reason, "…")) {
		t.Fatalf("cap split a multi-byte rune: %q", tail(reason))
	}
}

// tail returns the last few runes of s for failure messages.
func tail(s string) string {
	if r := []rune(s); len(r) > 12 {
		return string(r[len(r)-12:])
	}
	return s
}
