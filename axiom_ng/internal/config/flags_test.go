package config

import "testing"

// R5/R6 review pins: the search-arm flags' documented defaults and their
// env overrides (all case-insensitive), plus the unified envBool behavior.
func TestSearchArmFlagDefaults(t *testing.T) {
	c := Load()
	if c.SearchSparseArm {
		t.Fatal("AXIOM_SEARCH_SPARSE_ARM must default OFF (R7 benchmark: no quality gain, ~3.3s p95 cost)")
	}
	if c.SearchGraphArm {
		t.Fatal("AXIOM_SEARCH_GRAPH_ARM must default OFF (R7: slight quality loss, slow expansion)")
	}
	if !c.SearchRerank {
		t.Fatal("AXIOM_SEARCH_RERANK must default ON (R7: small consistent gain over hybrid)")
	}
}

func TestSearchArmFlagOverrides(t *testing.T) {
	for _, off := range []string{"0", "false", "FALSE", "no"} {
		t.Setenv("AXIOM_SEARCH_SPARSE_ARM", off)
		if Load().SearchSparseArm {
			t.Fatalf("AXIOM_SEARCH_SPARSE_ARM=%q must disable", off)
		}
	}
	t.Setenv("AXIOM_SEARCH_SPARSE_ARM", "TRUE")
	if !Load().SearchSparseArm {
		t.Fatal("AXIOM_SEARCH_SPARSE_ARM=TRUE must enable (case-insensitive)")
	}

	for _, on := range []string{"1", "true", "TRUE"} {
		t.Setenv("AXIOM_SEARCH_GRAPH_ARM", on)
		if !Load().SearchGraphArm {
			t.Fatalf("AXIOM_SEARCH_GRAPH_ARM=%q must enable", on)
		}
	}
	t.Setenv("AXIOM_SEARCH_GRAPH_ARM", "0")
	if Load().SearchGraphArm {
		t.Fatal("AXIOM_SEARCH_GRAPH_ARM=0 must disable")
	}
}

func TestEnvBoolCaseInsensitive(t *testing.T) {
	// envBool used to accept only lowercase forms — AXIOM_DISPATCHER_ENABLED=TRUE
	// silently meant OFF. Unification: every true-form enables.
	t.Setenv("AXIOM_DISPATCHER_ENABLED", "TRUE")
	if !Load().DispatcherEnabled {
		t.Fatal("AXIOM_DISPATCHER_ENABLED=TRUE must enable")
	}
	t.Setenv("AXIOM_DISPATCHER_ENABLED", "Yes")
	if !Load().DispatcherEnabled {
		t.Fatal("AXIOM_DISPATCHER_ENABLED=Yes must enable")
	}
	t.Setenv("AXIOM_DISPATCHER_ENABLED", "0")
	if Load().DispatcherEnabled {
		t.Fatal("AXIOM_DISPATCHER_ENABLED=0 must disable")
	}
}
