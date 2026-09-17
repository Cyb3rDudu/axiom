package fixerinvoker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
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
	// „kein messbares folio" negation form too, not only „nicht messbar".
	out5 := `{"verdict": "halt", "final_step": {"reason": "plan_class unclassifiable — kein messbares Folio-Signal, Diagnose nicht tragfähig"}, "unproven": ["stelle3_zitat: NOT IMPLEMENTED"]}`
	got5, _ := haltTerminalReason(out5)
	if !strings.HasPrefix(got5, "needs-evidence: stelle1_druckseite — ") {
		t.Fatalf("kein-messbares-folio ground must classify unmeasurability, got %q", got5)
	}

	// marker collision (follow-up review): a STOP report echoing the
	// system prompt's own stop guidance („wenn kein messbares
	// Heilungspotenzial vorliegt, schließe mit stop") must NOT classify
	// as a stelle1 evidence gap — only the folio-specific negation form
	// does. Otherwise every honest stop echoes itself into needs-evidence.
	out6 := `{"verdict": "halt", "final_step": {"action": "stop", "reason": "kein messbares Heilungspotenzial — stop gemäß Kodex"}, "unproven": []}`
	got6, _ := haltTerminalReason(out6)
	if !strings.HasPrefix(got6, "no-healable-defect-evidenced: kein messbares Heilungspotenzial") {
		t.Fatalf("stop-guidance echo must classify no-healable, got %q", got6)
	}
}

// --- #284: OCR-class routing, budgets, language, mode separation ---------

func mkItem(analysis string, lang string) *repo.RepairItem {
	return &repo.RepairItem{
		AttachmentKey: "KEY1",
		ContentType:   "application/pdf",
		Language:      lang,
		Analysis:      json.RawMessage(analysis),
	}
}

// TestOCRCaseDetection (#284): the OCR-class predicate keys on the STABLE
// analysis fields (pagination_state marker / ocr override), never on the
// operator-facing finding string — the #283 rename cannot break routing.
func TestOCRCaseDetection(t *testing.T) {
	cases := []struct {
		analysis string
		want     bool
	}{
		{`{"pagination_state": "needs_ocr"}`, true},
		{`{"finding": "🔴 unpaginiert"}`, false},          // string alone routes nothing
		{`{"ocr": {"mode": "force"}}`, true},             // per-case override
		{`{"pagination_state": "physical_only"}`, false}, // text-bearing, not OCR
		{`{}`, false},
	}
	for _, c := range cases {
		if got := ocrCase(mkItem(c.analysis, "")); got != c.want {
			t.Fatalf("ocrCase(%s) = %v, want %v", c.analysis, got, c.want)
		}
	}
}

// TestOCRLanguagePrecedence (#284 DoD): per-case override beats document
// metadata; metadata maps onto tesseract codes; unknown falls back to the
// owner default deu (deu beat deu+eng in the pilot).
func TestOCRLanguagePrecedence(t *testing.T) {
	// override beats metadata
	if got := ocrLanguage(mkItem(`{"ocr": {"lang": "eng"}, "pagination_state": "needs_ocr"}`, "de")); got != "eng" {
		t.Fatalf("override lang = %q, want eng", got)
	}
	// metadata default (ISO 639-1 → tesseract)
	if got := ocrLanguage(mkItem(`{"pagination_state": "needs_ocr"}`, "de")); got != "deu" {
		t.Fatalf("de → %q, want deu", got)
	}
	if got := ocrLanguage(mkItem(`{"pagination_state": "needs_ocr"}`, "en")); got != "eng" {
		t.Fatalf("en → %q, want eng", got)
	}
	// empty metadata → owner default
	if got := ocrLanguage(mkItem(`{"pagination_state": "needs_ocr"}`, "")); got != "deu" {
		t.Fatalf("empty → %q, want deu", got)
	}
	// non-OCR case: no lang arg at all (the fixer's own default applies)
	if got := ocrLanguage(mkItem(`{"pagination_state": "physical_only"}`, "de")); got != "" {
		t.Fatalf("non-OCR case lang = %q, want none", got)
	}
}

// TestFixerArgsOCR (#284 DoD): OCR-class cases carry --lang and, only for
// the force override, --ocr-mode force — the mode separation between pure
// scans (plain OCR) and broken text layers (rasterize away).
func TestFixerArgsOCR(t *testing.T) {
	// pure scan: language only, NO force
	args := fixerArgs(mkItem(`{"pagination_state": "needs_ocr"}`, "de"))
	if !slices.Contains(args, "--lang") || !slices.Contains(args, "deu") {
		t.Fatalf("pure scan args missing --lang deu: %v", args)
	}
	if slices.Contains(args, "force") {
		t.Fatalf("pure scan must NOT force: %v", args)
	}
	// broken text layer: force
	args = fixerArgs(mkItem(`{"ocr": {"mode": "force"}}`, "de"))
	if !slices.Contains(args, "--ocr-mode") || !slices.Contains(args, "force") {
		t.Fatalf("force case args missing --ocr-mode force: %v", args)
	}
}

// TestOCRTimeoutBudgetIndependent (#284 DoD): OCR-class repairs run under
// their OWN time budget — the general fixer timeout (35m) does not fit a
// 658-page rebuild. Config defaults pin the separation; the selection is
// observable through the env the wrapper receives.
func TestOCRTimeoutBudgetIndependent(t *testing.T) {
	cfg := Config{}
	cfg.fillDefaults()
	if cfg.Timeout != 35*time.Minute {
		t.Fatalf("normal timeout = %v, want 35m", cfg.Timeout)
	}
	if cfg.OCRTimeout <= cfg.Timeout {
		t.Fatalf("OCR timeout %v must sit ABOVE the normal %v", cfg.OCRTimeout, cfg.Timeout)
	}
	if cfg.OCRStaleAfter <= cfg.OCRTimeout {
		t.Fatalf("OCR stale %v must sit ABOVE the OCR timeout %v (live-but-slow never requeued)", cfg.OCRStaleAfter, cfg.OCRTimeout)
	}
}
