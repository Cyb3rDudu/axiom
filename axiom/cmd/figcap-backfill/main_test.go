package main

import (
	"testing"
)

func row(caps map[string]string, changed bool) engineRow {
	return engineRow{FigureCaptions: caps, Changed: changed}
}

func targetFor(id, doc, docKey, oldFigs string) target {
	return target{ID: id, DocID: doc, DocKey: docKey, OldFigRaw: oldFigs}
}

func TestSummarizeAddsMissedCaptions(t *testing.T) {
	targets := []target{
		targetFor("c1", "d1", "KEY1", `{}`),
		targetFor("c2", "d1", "KEY1", `{}`),
	}
	res := map[string]engineRow{
		"c1": row(map[string]string{"image-0001": "Figure 1. Overview"}, true),
		"c2": row(map[string]string{}, false),
	}
	rep := summarize(targets, res)
	if rep.added != 1 || rep.purged != 0 {
		t.Fatalf("want 1 add / 0 purges, got %d/%d", rep.added, rep.purged)
	}
	if len(rep.docs) != 1 || rep.docs[0].Changed != 1 || rep.docs[0].Added != 1 {
		t.Fatalf("per-document report wrong: %+v", rep.docs)
	}
	if rep.docs[0].Key != "KEY1" {
		t.Fatalf("document key lost: %+v", rep.docs[0])
	}
}

func TestSummarizePurgesProseFalsePositive(t *testing.T) {
	// stored prose FP, new extraction drops it → one purge, no add
	targets := []target{
		targetFor("c1", "d1", "KEY1", `{"image-0001":"Abb. 4.62 zeigt den Zusammenhang"}`),
	}
	res := map[string]engineRow{
		"c1": row(map[string]string{}, true),
	}
	rep := summarize(targets, res)
	if rep.purged != 1 || rep.added != 0 {
		t.Fatalf("want 0 adds / 1 purge, got %d/%d", rep.added, rep.purged)
	}
}

func TestSummarizeReplacementIsPurgeAndAdd(t *testing.T) {
	// a prose caption replaced by the real caption: the FP leaves, the
	// true caption arrives → one of each.
	targets := []target{
		targetFor("c1", "d1", "KEY1", `{"image-0001":"Abb. 6.4 zeigt den Ablauf"}`),
	}
	res := map[string]engineRow{
		"c1": row(map[string]string{"image-0001": "Abb. 6.4 Ablauf eines Zins-Swaps"}, true),
	}
	rep := summarize(targets, res)
	if rep.purged != 1 || rep.added != 1 {
		t.Fatalf("want 1 add / 1 purge, got %d/%d", rep.added, rep.purged)
	}
}

func TestSummarizeUnchangedDocumentsAreAbsent(t *testing.T) {
	targets := []target{targetFor("c1", "d1", "KEY1", `{"image-0001":"Figure 1. X"}`)}
	res := map[string]engineRow{
		"c1": row(map[string]string{"image-0001": "Figure 1. X"}, false),
	}
	rep := summarize(targets, res)
	if len(rep.docs) != 0 || rep.added != 0 || rep.purged != 0 {
		t.Fatalf("unchanged work must not appear in the report: %+v", rep)
	}
}
