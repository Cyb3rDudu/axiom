// search_test.go — golden search queries with Treffer-Klassen asserts
// (#295 Ziel 3). The fixture pins CLASS expectations (tags, citation
// class, title class, rank windows) — never raw scores — so the asserts
// stay honest and semantic while still having teeth: a degraded ranking
// (wrong class at the top) or a hollowed-out result class fails.
//
// The comparator is pure and unit-probed (TestSearchProbe*): the
// Mutations-Sonde the DoD demands — a deliberately worsened ranking must
// turn the comparator red — runs without needing a broken live system.
package baseline

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// searchHit is the projection of a /api/search hit the class rules see.
type searchHit struct {
	ChunkID        string   `json:"chunk_id"`
	Title          string   `json:"title"`
	Tags           []string `json:"tags"`
	CitationClass  string   `json:"citation_class"`
	LocatorPresent bool     `json:"locator_present"`
}

type searchRule struct {
	// tag_top: a hit carrying Tag must appear at rank <= MaxRank.
	Kind string `json:"kind"` // tag_top | title_top | min_class_top
	Tag  string `json:"tag,omitempty"`
	// title_top: a hit whose title starts with TitlePrefix must appear at
	// rank <= MaxRank (prefix, not equality — subtitle variants stay in class).
	TitlePrefix string `json:"title_prefix,omitempty"`
	MaxRank     int    `json:"max_rank,omitempty"`
	// min_class_top: at least Min hits within the first Top carry Tag.
	Top int `json:"top,omitempty"`
	Min int `json:"min,omitempty"`
}

type goldenQuery struct {
	Query string       `json:"query"`
	Rules []searchRule `json:"rules"`
}

type searchGoldenFile struct {
	Queries []goldenQuery `json:"queries"`
}

func loadSearchGolden(t *testing.T) searchGoldenFile {
	t.Helper()
	raw, err := os.ReadFile("fixtures/search_golden.json")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var f searchGoldenFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func hasTag(h searchHit, tag string) bool {
	for _, t := range h.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// evaluateSearchRules returns one violation string per broken rule.
func evaluateSearchRules(hits []searchHit, rules []searchRule) []string {
	var violations []string
	for _, r := range rules {
		switch r.Kind {
		case "tag_top":
			ok := false
			for i, h := range hits {
				if i+1 <= r.MaxRank && hasTag(h, r.Tag) {
					ok = true
					break
				}
			}
			if !ok {
				violations = append(violations,
					"tag_top: no hit with tag "+r.Tag+" within rank "+strconv.Itoa(r.MaxRank))
			}
		case "title_top":
			ok := false
			for i, h := range hits {
				if i+1 <= r.MaxRank && len(h.Title) >= len(r.TitlePrefix) &&
					h.Title[:len(r.TitlePrefix)] == r.TitlePrefix {
					ok = true
					break
				}
			}
			if !ok {
				violations = append(violations,
					"title_top: no hit with prefix "+r.TitlePrefix+" within rank "+strconv.Itoa(r.MaxRank))
			}
		case "min_class_top":
			n := 0
			for i, h := range hits {
				if i < r.Top && hasTag(h, r.Tag) {
					n++
				}
			}
			if n < r.Min {
				violations = append(violations,
					"min_class_top: only "+strconv.Itoa(n)+" hits with tag "+r.Tag+" in top "+strconv.Itoa(r.Top)+", want >= "+strconv.Itoa(r.Min))
			}
		default:
			violations = append(violations, "unknown rule kind: "+r.Kind)
		}
	}
	return violations
}

// TestSearchGoldenLive — every fixture query against the freeze bits.
// Also snapshots the observed class composition per query (actual dir)
// for the determinism proof — scores are deliberately NOT recorded.
func TestSearchGoldenLive(t *testing.T) {
	liveEnabled(t)
	assertFreezeBits(t)

	f := loadSearchGolden(t)
	summary := map[string]any{}
	for _, q := range f.Queries {
		var resp struct {
			Hits []struct {
				ChunkID string `json:"chunk_id"`
				Source  struct {
					Title         string   `json:"title"`
					Tags          []string `json:"tags"`
					CitationClass string   `json:"citation_class"`
				} `json:"source"`
				Locator json.RawMessage `json:"locator"`
			} `json:"hits"`
		}
		if code := httpJSON(t, "POST", ragBase()+"/api/search",
			map[string]any{"query": q.Query}, &resp); code != 200 {
			t.Fatalf("search %q: status %d", q.Query, code)
		}
		hits := make([]searchHit, 0, len(resp.Hits))
		classes := make([]string, 0, len(resp.Hits))
		for _, h := range resp.Hits {
			hits = append(hits, searchHit{
				ChunkID:        h.ChunkID,
				Title:          h.Source.Title,
				Tags:           h.Source.Tags,
				CitationClass:  h.Source.CitationClass,
				LocatorPresent: len(h.Locator) > 2,
			})
			classes = append(classes, joinStrings(h.Source.Tags, "+"))
		}
		summary[q.Query] = classes
		if v := evaluateSearchRules(hits, q.Rules); len(v) > 0 {
			t.Errorf("golden query %q degraded:\n  %s", q.Query, joinStrings(v, "\n  "))
		}
	}
	goldenCompare(t, "search_classes.json", summary)
}

func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// --- Mutations-Sonden (comparator teeth, no live system needed) ----------

// bartscherHits is the calibrated shape of the Personalmanagement top-5
// under the freeze bits (rank class composition, scores irrelevant).
func bartscherHits(swapped bool) []searchHit {
	base := []searchHit{
		{Title: "Bedeutung des Managements und Personalmanagements (VLU1)", Tags: []string{"contextual", "PER_VL"}, CitationClass: "contextual"},
		{Title: "Personalmanagement: Grundlagen, Handlungsfelder, Praxis", Tags: []string{"neutral", "secondary source"}, CitationClass: "citable"},
		{Title: "Bedeutung des Managements und Personalmanagements (VLU1)", Tags: []string{"contextual", "PER_VL"}, CitationClass: "contextual"},
		{Title: "Einstiegsvorlesung Personalmanagement — Transkript", Tags: []string{"contextual", "PER_VL"}, CitationClass: "contextual"},
		{Title: "Einstiegsvorlesung Personalmanagement — Transkript", Tags: []string{"contextual", "PER_VL"}, CitationClass: "contextual"},
	}
	if swapped {
		// degraded ranking: a neutral textbook pushed to rank 1
		base[0], base[1] = base[1], base[0]
	}
	return base
}

// personalmanagementRules is gone: the mutation probes consume the REAL
// fixture rules (probeRules) so a weakened search_golden.json weakens the
// probes too — the fixture cannot rot independently of its teeth.
func probeRules(t *testing.T) []searchRule {
	t.Helper()
	f := loadSearchGolden(t)
	if len(f.Queries) == 0 || len(f.Queries[0].Rules) == 0 {
		t.Fatal("fixture search_golden.json has no rules for the first query — mutation probes would assert nothing")
	}
	return f.Queries[0].Rules
}

// TestSearchProbeSwappedRank — Mutations-Sonde (DoD): a deliberately
// worsened query ranking must turn the class asserts red.
func TestSearchProbeSwappedRank(t *testing.T) {
	if v := evaluateSearchRules(bartscherHits(false), probeRules(t)); len(v) != 0 {
		t.Fatalf("calibrated ranking must be green, got: %v", v)
	}
	v := evaluateSearchRules(bartscherHits(true), probeRules(t))
	if len(v) == 0 {
		t.Fatal("swapped ranking (non-PER_VL at rank 1) did NOT violate the golden rules — asserts have no teeth")
	}
}

// TestSearchProbeHollowedClass — Mutations-Sonde: thinning the contextual
// class below the min_class_top floor must violate.
func TestSearchProbeHollowedClass(t *testing.T) {
	hits := bartscherHits(false)
	for i := range hits {
		if i != 1 { // keep one contextual, hollow out the rest
			hits[i].Tags = []string{"neutral"}
			hits[i].CitationClass = "citable"
		}
	}
	v := evaluateSearchRules(hits, probeRules(t))
	if len(v) == 0 {
		t.Fatal("hollowed-out contextual class did NOT violate min_class_top — no teeth")
	}
}
