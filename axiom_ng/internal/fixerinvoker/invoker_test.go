package fixerinvoker

import (
	"context"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The unit tier (no DB): config clamping, output bounding, and the
// runFixer exit-code/timeout mapping against tiny real shell scripts —
// the same surface fix.sh exposes (exit N, output, hanging).

func unitCfg(t *testing.T, script string, timeout time.Duration) Config {
	t.Helper()
	cfg := Config{Command: script, Timeout: timeout, Interval: time.Millisecond}
	cfg.fillDefaults()
	return cfg
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-fixer.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigClamps(t *testing.T) {
	cfg := Config{Concurrency: 0}
	cfg.fillDefaults()
	if cfg.Concurrency != 1 || cfg.Command != "/opt/axiom/bin/axiom-fixer" ||
		cfg.Timeout != 35*time.Minute || cfg.StaleAfter != 40*time.Minute || cfg.Interval != 30*time.Second {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	cfg = Config{Concurrency: 5}
	cfg.fillDefaults()
	if cfg.Concurrency != 2 {
		t.Fatalf("concurrency must clamp to 2, got %d", cfg.Concurrency)
	}
	// StaleAfter must sit above Timeout: a live-but-slow invocation must
	// never be requeued under a second invoker.
	cfg = Config{Timeout: time.Hour}
	cfg.fillDefaults()
	if cfg.StaleAfter <= cfg.Timeout {
		t.Fatalf("StaleAfter (%s) must be > Timeout (%s)", cfg.StaleAfter, cfg.Timeout)
	}
}

func TestRunFixerExitCodeMapping(t *testing.T) {
	inv := &Invoker{cfg: unitCfg(t, writeScript(t, "echo boom; exit 7\n"), time.Minute)}
	rc, _, err := inv.runFixer(context.Background(), &repo.RepairItem{AttachmentKey: "K1"})
	if rc != 7 || !strings.Contains(err.Error(), "fixer exit 7") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("exit mapping: rc=%d err=%v", rc, err)
	}
}

func TestRunFixerTimeoutKills(t *testing.T) {
	inv := &Invoker{cfg: unitCfg(t, writeScript(t, "sleep 30\n"), 150*time.Millisecond)}
	start := time.Now()
	rc, _, err := inv.runFixer(context.Background(), &repo.RepairItem{AttachmentKey: "K1"})
	if rc != -1 || err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout must map to rc=-1 + timeout reason, got rc=%d err=%v", rc, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("hung fixer must be killed by the backstop, took %s", time.Since(start))
	}
}

func TestRunFixerSpawnError(t *testing.T) {
	inv := &Invoker{cfg: unitCfg(t, "/nonexistent/fixer-xyz", time.Minute)}
	rc, _, err := inv.runFixer(context.Background(), &repo.RepairItem{AttachmentKey: "K1"})
	if rc != -1 || err == nil || !strings.Contains(err.Error(), "spawn") {
		t.Fatalf("spawn error: rc=%d err=%v", rc, err)
	}
}

func TestLastLinesBoundsOutput(t *testing.T) {
	if got := lastLines([]byte(strings.Repeat("x", 1000))); len(got) > 404 {
		t.Fatalf("output not bounded: %d", len(got))
	}
	if got := lastLines([]byte("short")); got != "short" {
		t.Fatalf("short output mangled: %q", got)
	}
}

func TestFixerArgsRouteEPUB(t *testing.T) {
	// PDF case: byte-identical to the pre-#205 shape.
	pdf := fixerArgs(&repo.RepairItem{AttachmentKey: "K1", ContentType: "application/pdf"})
	if len(pdf) != 2 || pdf[0] != "K1" || pdf[1] != "--apply" {
		t.Fatalf("pdf args must stay [key --apply], got %v", pdf)
	}
	// EPUB case: format arm + local source (file:// stripped).
	epub := fixerArgs(&repo.RepairItem{
		AttachmentKey: "K2", ContentType: "application/epub+zip",
		LocalPath: "file:///opt/data/x.epub",
	})
	want := []string{"K2", "--apply", "--format", "epub", "--source", "/opt/data/x.epub"}
	if len(epub) != len(want) {
		t.Fatalf("epub args = %v, want %v", epub, want)
	}
	for i := range want {
		if epub[i] != want[i] {
			t.Fatalf("epub args = %v, want %v", epub, want)
		}
	}
	if n := repairArtifactName(&repo.RepairItem{ContentType: "application/epub+zip"}); n != "work.epub" {
		t.Fatalf("epub artifact = %q, want work.epub", n)
	}
	if n := repairArtifactName(&repo.RepairItem{ContentType: "application/pdf"}); n != "work.pdf" {
		t.Fatalf("pdf artifact = %q, want work.pdf", n)
	}
}

// ── #253: HALT verdict classification (pure tier) ─────────────────────────

func TestHaltTerminalReasonClassification(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string // "" = not terminal
	}{
		{"halt plain", `{"verdict": "halt", "unproven": []}`, "no-healable-defect-evidenced"},
		{"halt needs evidence", `{"verdict": "halt", "unproven": ["stelle2_chunk: RAG nicht erreichbar (ConnectError)"]}`, "needs-evidence"},
		{"halt evidence 2", `{"verdict": "halt", "unproven": ["stelle3_zitat: ohne Zotero-Annotation nicht prüfbar"]}`, "needs-evidence"},
		{"report not terminal", `{"verdict": "report", "unproven": []}`, ""},
		{"healed not terminal", `{"verdict": "healed"}`, ""},
		{"garbage not terminal", `fixer log line without json`, ""},
		{"json after logs", "INFO stuff\n{\"verdict\": \"halt\", \"unproven\": []}", "no-healable-defect-evidenced"},
	}
	for _, tc := range cases {
		got, ok := haltTerminalReason(tc.out)
		if tc.want == "" {
			if ok {
				t.Fatalf("%s: must not be terminal, got %q", tc.name, got)
			}
			continue
		}
		if !ok || !strings.HasPrefix(got, tc.want) {
			t.Fatalf("%s: want prefix %q, got ok=%v %q", tc.name, tc.want, ok, got)
		}
	}
}

// #278: the park reason names the slot that ACTUALLY caused the escalation
// (final_step.reason), not a static declaration some evidence slot happens
// to carry. Production shape (DAM2Q443): Stelle-1 folio signal unmeasurable,
// while unproven carries the (now honestly not-implemented) stelle3 slot —
// the old first-match scan led with stelle3 and sent the operator down a
// false trail three times.
func TestHaltReasonNamesTheActualGround(t *testing.T) {
	out := `{"verdict": "halt", "final_step": {"action": "escalate", "reason": "unclassifiable / nicht messbar — Verweigerung statt Ratens (Wahrheits-Ordnung: nicht messbar → verweigern)."}, "unproven": ["stelle3_zitat: NOT IMPLEMENTED — kein Codepfad liest Zotero-Annotationen in diesem Build", "annotation-label: not implemented (kein Annotations-Lesepfad)"]}`
	got, ok := haltTerminalReason(out)
	if !ok {
		t.Fatalf("halt must be terminal")
	}
	if !strings.HasPrefix(got, "needs-evidence: stelle1_druckseite — ") {
		t.Fatalf("unmeasurable ground must be attributed to Stelle 1, got %q", got)
	}
	if !strings.Contains(got, "nicht messbar") {
		t.Fatalf("the ground text must be carried verbatim, got %q", got)
	}
	if strings.Contains(got, "stelle3") {
		t.Fatalf("the static stelle3 declaration must not lead or appear as the ground, got %q", got)
	}

	// ground names a runtime evidence gap (not unmeasurability): classified
	// needs-evidence, still LED by the ground — not by an unproven entry.
	out2 := `{"verdict": "halt", "final_step": {"reason": "Stelle 2 nicht erreichbar nach Retry"}, "unproven": ["stelle3_zitat: NOT IMPLEMENTED"]}`
	got2, _ := haltTerminalReason(out2)
	if !strings.HasPrefix(got2, "needs-evidence: Stelle 2 nicht erreichbar") {
		t.Fatalf("runtime-gap ground must lead, got %q", got2)
	}

	// ground without any evidence-gap markers: honest no-healable verdict,
	// ground verbatim.
	out3 := `{"verdict": "halt", "final_step": {"reason": "chaotische Evidenz, Klasse unclassifiable"}, "unproven": []}`
	got3, _ := haltTerminalReason(out3)
	if !strings.HasPrefix(got3, "no-healable-defect-evidenced: chaotische Evidenz") {
		t.Fatalf("plain ground must classify no-healable with the ground, got %q", got3)
	}

	// not-implemented declarations alone do NOT classify needs-evidence
	// (fallback path, no model reason): they are build facts, not gaps a
	// retry could close.
	out4 := `{"verdict": "halt", "unproven": ["stelle3_zitat: NOT IMPLEMENTED — kein Codepfad"]}`
	got4, _ := haltTerminalReason(out4)
	if !strings.HasPrefix(got4, "no-healable-defect-evidenced") {
		t.Fatalf("not-implemented alone must not be needs-evidence, got %q", got4)
	}

	// production phrasing drift (#278 review): the plan_class prose said
	// „kein messbares Folio-Signal" — the marker set must catch the
	// „kein messbar" negation form too, not only „nicht messbar".
	out5 := `{"verdict": "halt", "final_step": {"reason": "plan_class unclassifiable — kein messbares Folio-Signal, Diagnose nicht tragfähig"}, "unproven": ["stelle3_zitat: NOT IMPLEMENTED"]}`
	got5, _ := haltTerminalReason(out5)
	if !strings.HasPrefix(got5, "needs-evidence: stelle1_druckseite — ") {
		t.Fatalf("kein-messbar ground must classify unmeasurability, got %q", got5)
	}
}
