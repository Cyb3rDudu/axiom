package repo

import "testing"

// TestLabeledCaptionText (#257): both caption sources feed caption_text with
// source labels — machine captions and document figure captions stay
// distinguishable; deterministic order; malformed JSON degrades to skip.
// Mutation probe: dropping the figure merge (SWIFT case) goes red.
func TestLabeledCaptionText(t *testing.T) {
	caps := `{"image-0001":"a line chart of flows"}`
	figs := `{"image-0000":"Figure 1. Annual SWIFT Messages in Millions*"}`
	got := LabeledCaptionText(&caps, &figs)
	want := "[machine image caption: a line chart of flows] [document figure caption: Figure 1. Annual SWIFT Messages in Millions*]"
	if got != want {
		t.Fatalf("labeledCaptionText = %q, want %q", got, want)
	}
	if got := LabeledCaptionText(nil, nil); got != "" {
		t.Fatalf("no captions must yield empty, got %q", got)
	}
	bad := `not json`
	if got := LabeledCaptionText(&bad, nil); got != "" {
		t.Fatalf("malformed JSON must degrade to empty, got %q", got)
	}
}
