package dispatcher

import "testing"

// TestAutoQueueRepairClassesLegacyAndNewSpelling (#283 review fix): both
// findings of the scan class auto-queue — the renamed operator-facing
// string AND the legacy one (stored cases, mixed-binary runners). Removing
// either key breaks repair admission for a whole deployment generation.
func TestAutoQueueRepairClassesLegacyAndNewSpelling(t *testing.T) {
	for _, class := range []string{
		"🔴 scan-ohne-textlayer (OCR-Wiederaufbau nötig)",
		"🔴 unpaginiert",
		"🔴 reparierbar",
		"SOURCE_UNREADABLE",
	} {
		if !autoQueueRepairClasses[class] {
			t.Fatalf("class %q must auto-queue", class)
		}
	}
}
