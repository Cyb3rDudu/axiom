// #252 outcome-truth derivation pins. Each subtest pins ONE branch of
// DeriveOutcome — mutation: flip or reorder any branch and its subtest(s)
// go red (that is the issue's mutation criterion, kept as plain asserts).
package mirror

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
		hasActiveSnapshot             bool
		wantOutcome, wantReason       string
	}{
		// excluded: selection beats everything — a deliberately held doc is
		// not "failed", and never "pending".
		{"excluded wins over failed job", "excluded", "failed", "X", "boom", "", "", false, "excluded", "selection-excluded"},
		{"excluded wins over completed job", "excluded", "completed", "", "", "", "", false, "excluded", "selection-excluded"},
		{"excluded beats pending job", "excluded", "pending", "", "", "", "", false, "excluded", "selection-excluded"},

		// running: pending / claimed / processing (issue: pending/processing while running)
		{"pending job", "", "pending", "", "", "", "", false, "pending", ""},
		{"claimed job", "", "claimed", "", "", "", "", false, "processing", ""},
		{"processing job", "", "processing", "", "", "", "", false, "processing", ""},
		{"running beats repair case", "", "claimed", "", "", "", "in_repair", false, "processing", ""},
		{"running beats needs_ocr", "", "claimed", "", "", "needs_ocr", "", false, "processing", ""},

		// needs_ocr: unpaginiert dead end from the #254 pagination_state —
		// explicitly BEFORE the repair track (unpaginiert cases sit rejected
		// forever; they must read needs_ocr, not in_repair).
		{"needs_ocr from pagination_state", "", "skipped", "SKIPPED", "preflight:unpaginiert", "needs_ocr", "", false, "needs_ocr", "scan-ohne-textlayer: text-less scan — OCR rebuild heals it (scan_ocr_rebuild, #284)"},
		{"needs_ocr beats rejected repair case", "", "skipped", "", "", "needs_ocr", "rejected", false, "needs_ocr", "scan-ohne-textlayer: text-less scan — OCR rebuild heals it (scan_ocr_rebuild, #284)"},
		{"physical_only is NOT needs_ocr", "", "completed", "", "", "physical_only", "", false, "completed", ""},

		// in_repair: live from the repair case status
		{"in_repair queued", "", "skipped", "SKIPPED", "preflight:reparierbar", "", "queued", false, "in_repair", "repair case: queued"},
		{"in_repair claimed by fix-service", "", "skipped", "", "", "", "in_repair", false, "in_repair", "repair case: in_repair"},
		{"in_repair rejected candidate", "", "skipped", "", "", "", "rejected", false, "in_repair", "repair case: rejected"},
		{"in_repair blocked for dudu", "", "skipped", "", "", "", "blocked_for_dudu", false, "in_repair", "repair case: blocked_for_dudu"},
		// an OPEN case is live even over a stale completed job: repair beats
		// completed (the repaired re-ingest has not landed yet).
		{"repair case beats completed job", "", "completed", "", "", "", "queued", false, "in_repair", "repair case: queued"},
		{"healed repair falls through to job", "", "completed", "", "", "", "healed", false, "completed", ""},
		// healed = CLOSED case: the job status is the truth until reprocessing
		// re-runs the doc (a healed repair over its own skipped job still reads
		// failed until the re-ingest completes).
		{"healed repair falls through to old skipped job", "", "skipped", "SKIPPED", "preflight:unpaginiert", "", "healed", false, "failed", "SKIPPED: preflight:unpaginiert"},
		{"failed repair case falls through to job reason", "", "skipped", "SKIPPED", "preflight:reparierbar", "", "failed", false, "failed", "SKIPPED: preflight:reparierbar"},

		// completed
		{"completed", "", "completed", "", "", "", "", false, "completed", ""},

		// failed + reason excerpt
		{"failed with code and message", "", "failed", "RETRY_EXHAUSTED", "lease expired", "", "", false, "failed", "RETRY_EXHAUSTED: lease expired"},
		{"skipped reason excerpt", "", "skipped", "SKIPPED", "preflight:no_text_layer", "", "", false, "failed", "SKIPPED: preflight:no_text_layer"},
		{"failed without message falls back to code", "", "failed", "SOURCE_URL_STALE", "", "", "", false, "failed", "SOURCE_URL_STALE"},
		{"cancelled without any reason falls back to status", "", "cancelled", "", "", "", "", false, "failed", "cancelled"},

		// #356 serving: an administrative closure (cancelled wave, cleanup,
		// skip) WITH a surviving active snapshot is a healthy document — the
		// closure is named in the reason, never surfaced as data loss.
		{"cancelled wave with active snapshot", "", "cancelled", "", "", "", "", true, "serving", "last job closed as cancelled — the active snapshot keeps the document served"},
		{"cleanup closure with active snapshot", "", "failed", "NIGHT_CLEANUP", "Redundant (Snapshot exists)", "", "", true, "serving", "last job closed as failed (NIGHT_CLEANUP) — the active snapshot keeps the document served"},
		{"skipped with active snapshot", "", "skipped", "SKIPPED", "CONTENT_HASH_MISSING", "", "", true, "serving", "last job closed as skipped (SKIPPED) — the active snapshot keeps the document served"},
		{"no job row with active snapshot", "", "", "", "", "", "", true, "serving", "no job row — active snapshot serves"},

		// no job at all, no snapshot: nothing happened yet — pending, not held-lie
		{"no job never enqueued", "", "", "", "", "", "", false, "pending", "never enqueued"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, gotReason := DeriveOutcome(c.selMode, c.jobStatus, c.errCode, c.errMsg, c.paginationState, c.repairStatus, c.hasActiveSnapshot)
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
	_, reason := DeriveOutcome("", "failed", "", long, "", "", false)
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
