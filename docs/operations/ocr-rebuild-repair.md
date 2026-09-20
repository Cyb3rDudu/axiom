# OCR-Rebuild Repair — the `scan_ocr_rebuild` class (#284)

The fixer heals **textless scans** and **broken text layers** with
OCRmyPDF. Before #284 nothing in the system could repair this class —
preflight classified it (`🔴 unpaginiert`, now renamed per #283) and
parked it.

## The two findings of the class

| Finding | Detection (deterministic, `tools/scan_ocr_rebuild.diagnose`) | Mode |
| --- | --- | --- |
| Pure image scan (no text layer) | no extractable chars + raster evidence (≥ half the pages carry images) | plain OCR |
| Broken text layer (digitally born, defective word segmentation — words concatenated without spaces; production case: Reder 2024 SSOAR PDF) | text present, space ratio < 0.06 AND mean word length > 12 | `--force-ocr` (rasterizes the broken vector text away) |

The class is a **deterministic catalog rule** in `repair_agent.py` — no
model call, no DeepSeek key needed (same pattern as the #253
labeltree-missing rule). The repair runs before the label rule so a
broken text layer is rasterized before any folio harvest reads it.

## Owner rulings, pinned in code

- **The text layer decides, never the labels (#288)**: preflight
  classifies by TEXT LAYER first — a document whose pages carry no
  extractable text is a scan needing textification, **regardless of
  embedded PDF page labels** (production case Bartscher 2026-09-18: 658
  pages, 0 text chars, healthy labels — was green-lit into a projected
  10–16 h internal Marker/Surya OCR run while the stored object stayed
  textless and unannotatable). Labels only steer how the textification
  preserves them (the 2-in-1 folio heal above), never whether it is
  needed. **Internal OCR of scans is no longer a supported route**: the
  stored Zotero object must carry the text layer (highlights and quotes
  need text), internal OCR is transient — and the heal path is faster on
  top (bundled ocrmypdf, CPU, < 1 h for 658 pages in the pilot vs 10–16 h
  internal).
- **Manual escape hatch**: when the fixer is not applicable (no bundled
  OCR language, damaged original the rebuild chokes on, operator-curated
  OCR preferred), the operator route is the #279 custody tool with a
  pre-OCR'd file — same quarantine-first protocol, replaces the stored
  file (see `custody-repair-runbook.md`).
- **Manual full rebuilds of large books** (outside the invoker): fix.sh
  defaults to its own 30-min cap — for a hand-run Bartscher-class
  rebuild raise it explicitly:
  `AXIOM_FIX_SH_TIMEOUT=85500 scripts/fix.sh <KEY> --apply` (23h55m,
  matching the invoker's wedge-guard minus slack).
- **Full throttle (#293)**: the rebuild passes `--jobs` = all available
  cores, always, with no per-case tuning. Production reference: 658 pages
  in 10m14s with 12 workers on the host pilot. (The old "no --jobs"
  wording died with the Bartscher E2E: ocrmypdf's default ran ~3 workers.)
- **Language**: default `deu` (deu beat `deu+eng` in the pilot — combined
  mode produced errors like "Universitit"). Resolution order:
  per-case override (`analysis.ocr.lang`) → document metadata language
  (ISO 639-1/2 mapped to tesseract codes) → `deu`.
- **BEST models bundled (#293)**: deu+eng ship as the pinned
  `tessdata_best` variants (sha256-pinned in `build_fixer_artifact.sh`,
  download→tmp→mv per the #286 hardlink discipline) — quality parity with
  the owner reference runs; the FAST models of the conda package showed
  the „Universität→Universitit" error class on German text.
- **Dynamic duration (#293)**: the OCR process runs as long as it runs —
  nobody computes a budget up front, and the tool carries NO internal
  timeout kill. `AXIOM_FIXER_OCR_TIMEOUT` (default **24h**) bounds the
  invoker's backstop as a pure **wedge-guard** (a wedged process vs. a
  working one — orphan prevention, never tempo limitation); fix.sh
  receives the budget minus slack via `AXIOM_FIX_SH_TIMEOUT` so its
  `timeout` binary stays the primary killer. The stale-claim reaper
  honors the class bound — a live, merely slow rebuild is never requeued
  under a second claim.
- **Known toolchain penalty (accepted, #293)**: the bundled conda
  toolchain measures ~1.7× CPU-per-page against the nix host build
  (ghostscript rasterization is the prime suspect). Accepted for now —
  bundling buys autarky (no host tesseract/gs dependency); even with the
  penalty, 658 pages land well inside the wedge-guard window at full
  throttle. Upgrade path if it ever matters: a faster ghostscript via a
  conda-forge pin, re-measured against the documented baseline.
- **No deskew by default** (pilot pages were straight; unnecessary image
  processing costs quality). Available as a per-case option.
- **Post-OCR chaining (2-in-1)**: after the rebuild, the folio harvest runs
  on the NEW text layer and labels are written in the same repair — scans
  with unmeasurable folios become measurable, text + page labels healed
  together. If the folio mapping is not representable, the text layer
  heals alone (the document processes with `physical_only` locators).
- **Custody unchanged**: quarantine-first protocol — original to
  quarantine, OCR derivative uploaded as the healed attachment (#279).

## Command shape (the owner pilot, verbatim)

```text
ocrmypdf --language <lang> --oversample 300 --output-type pdf --optimize 1 [--force-ocr] in.pdf out.pdf
```

Verification gates (no silent lies): page count and page geometry must be
UNCHANGED, the new text layer must exist (aggregate mean chars/page —
real blank/front-matter pages are tolerated; the strict per-page gate of
the older `ocr_tool` does not fit real books). A failed gate rejects the
output; the original stays untouched.

## Flow through the system

1. Preflight rejects the scan → repair case (`pagination_state:
   needs_ocr` in the analysis). **Automated path today = textless scans**:
   the preflight does not yet detect broken word segmentation (follow-up),
   so a broken-text-layer case reaches the force mode through the operator
   surface: `POST /api/repair/cases/{id}/requeue` with
   `{"reason": "...", "analysis_patch": {"ocr": {"mode": "force",
   "lang": "eng"}}}` — the requeue route merges the override into the case
   analysis (shallow jsonb merge: a patched `ocr` object replaces any
   previous one — carry `mode` and `lang` together), which routes the
   invoker onto the OCR budget and `--ocr-mode force`. (Space-less scripts — CJK and kin — are explicitly recognized
   as INTACT and never force-classified.)
2. The case **auto-queues** (#284 lifted the historical never-queue
   refusal) — the pilot books already sitting rejected in the DB can be
   queued manually via the repair API.
3. The invoker claims the case, routes `--lang`/`--ocr-mode` from the case
   analysis + document metadata, runs fix.sh under the OCR budget.
4. The catalog rule rebuilds, chains the folio heal, leaves `work.pdf` +
   `report.json`; the invoker applies custody and marks the case healed.
5. The #282 post-heal auto-sync enqueues the healed attachment — the
   document processes end-to-end without operator action.

### Abgebrochener Create-Lauf (Orphan-Guard, #285)

Wenn der Upload nach dem Löschen des alten Items scheitert UND das
Best-Effort-Cleanup ebenfalls (Zotero-Mint des Items ✓, Upload ✗), auditiert
`repair.Apply` den Orphan-Key maschinenlesbar als `create_attachment_orphan`
— für ALLE Pfade (Auto-Verdict, Fixer-Invoker, manuelles Custody). Die
Requeue-Route verweigert dann den blinden Re-Run (409, nennt den Key): erst
 das LEERE Anhang-Item in Zotero löschen, dann mit Ack erneut requeued:

```bash
curl -sS -X POST http://<host>:<port>/api/repair/cases/<id>/requeue \
  -H 'content-type: application/json' \
  -d '{"reason": "orphan gelöscht", "orphan_resolved": "<KEY>"}'
```

Die Bestätigung landet als `create_attachment_orphan_resolved` in derselben
Audit-Tabelle — ein Re-Run mintet damit nie ein zweites leeres Geschwister.
Das manuelle Custody-Werkzeug trägt den Key stattdessen im Protokoll-Satz
(409-Guard, siehe Custody-Runbook).

**Residuum (bekannt):** stürzt der Lauf ZWISCHEN Orphan-Audit und
Failure-Mark ab, bleibt der Case `in_repair` und der Stale-Claim-Reaper
kann ihn ohne Ack-Guard zurück in `queued` setzen. Ein solcher Re-Run
stirbt aber am version-guarded DELETE (404 — das alte Item ist weg),
BEVOR es zu einem Create kommt: kein zweites Geschwister; der Case
landet `failed` und der Requeue-Guard übernimmt.

## Prerequisites — bundled (#286, #293)

The fixer artifact ships the entire OCR toolchain: `ocrmypdf` (venv),
`tesseract` + `ghostscript` and the `deu`/`eng` traineddata in `env/`
(as the pinned `tessdata_best` variants — quality parity with the owner
reference runs). The tools resolve everything env-relatively (no host
PATH contribution); the build proves it with a staged `--list-langs`
check and a sanitized-PATH rebuild smoke. Only pre-#286 fixer builds
rely on host binaries. **#293:** there is NO internal per-run bound
anymore — the rebuild runs as long as it runs; the only ceilings are the
outer wedge-guards (fix.sh's `AXIOM_FIX_SH_TIMEOUT`, the invoker's
`AXIOM_FIXER_OCR_TIMEOUT`, default 24h).

## Acceptance reference

The owner pilots (2026-09-17): Queckenberg 2025 (241 pages, 0 extractable
chars, ~96 dpi scan) → tesseract `deu`: 464k chars, 241/241 pages,
dimensions unchanged; Bartscher (658 pages) → 1.94M chars. Reference
outputs: `~/Downloads/queckenberg-ocr.pdf`, `~/Downloads/bartscher-ocr.pdf`.
