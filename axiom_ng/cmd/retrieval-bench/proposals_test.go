package main

import (
	"encoding/json"
	"os"
	"testing"
)

// Pins the committed v2 proposals artifact — the materializer's input
// schema (mirrors TestGoldSuiteLoads): enough entries, every best chunk
// and scope set, decisions pending (empty) in the committed state.
func TestGoldSuiteV2ProposalsPinSchema(t *testing.T) {
	data, err := os.ReadFile("gold_suite_v2_proposals.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Proposals []proposal `json:"proposals"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Proposals) < 20 {
		t.Fatalf("proposals too small: %d", len(doc.Proposals))
	}
	ids := map[string]bool{}
	for _, pr := range doc.Proposals {
		if pr.QueryID == "" || pr.Q == "" {
			t.Fatalf("incomplete proposal %+v", pr)
		}
		if pr.Best.ChunkID == "" {
			t.Fatalf("proposal %s: best chunk missing", pr.QueryID)
		}
		if len(pr.ScopeIDs) == 0 || len(pr.ScopeBooks) == 0 {
			t.Fatalf("proposal %s: scope missing", pr.QueryID)
		}
		if pr.Decision != "" {
			t.Fatalf("proposal %s: committed artifact must be pending (empty decision), got %q", pr.QueryID, pr.Decision)
		}
		if ids[pr.QueryID] {
			t.Fatalf("duplicate query id %s", pr.QueryID)
		}
		ids[pr.QueryID] = true
	}
}
