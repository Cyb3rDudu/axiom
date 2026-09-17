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

- **Unthrottled**: no `--jobs`/OMP limits in normal processing (the pilot's
  throttling was wave-coexistence, not a product requirement).
- **Language**: default `deu` (deu beat `deu+eng` in the pilot — combined
  mode produced errors like "Universitit"). Resolution order:
  per-case override (`analysis.ocr.lang`) → document metadata language
  (ISO 639-1/2 mapped to tesseract codes) → `deu`.
- **Own time budget**: OCR-class repairs do not share the 35-minute fixer
  timeout (a 658-page rebuild does not fit). `AXIOM_FIXER_OCR_TIMEOUT`
  (default 90m) bounds the invoker's backstop; fix.sh receives the budget
  minus slack via `AXIOM_FIX_SH_TIMEOUT` so its `timeout` binary stays the
  primary killer. The stale-claim reaper honors the class bound — a live,
  merely slow rebuild is never requeued under a second claim.
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
   the preflight does not yet detect broken word segmentation, so a
   broken-text-layer case carries no marker until an operator seeds the
   per-case `analysis.ocr` override (`{"ocr": {"mode": "force"}}`, set on
   the repair case's analysis before queueing — the follow-up is teaching
   the preflight the segmentation heuristic).
2. The case **auto-queues** (#284 lifted the historical never-queue
   refusal) — the pilot books already sitting rejected in the DB can be
   queued manually via the repair API.
3. The invoker claims the case, routes `--lang`/`--ocr-mode` from the case
   analysis + document metadata, runs fix.sh under the OCR budget.
4. The catalog rule rebuilds, chains the folio heal, leaves `work.pdf` +
   `report.json`; the invoker applies custody and marks the case healed.
5. The #282 post-heal auto-sync enqueues the healed attachment — the
   document processes end-to-end without operator action.

## Host prerequisite

`tesseract` (with `deu` + `eng` traineddata) and `ghostscript` on PATH,
plus `ocrmypdf` in the fixer venv (bundled). The mothership nix config
carries the OCR stack (`nix-conf/hosts/mothership/home.nix`).

## Acceptance reference

The owner pilots (2026-09-17): Queckenberg 2025 (241 pages, 0 extractable
chars, ~96 dpi scan) → tesseract `deu`: 464k chars, 241/241 pages,
dimensions unchanged; Bartscher (658 pages) → 1.94M chars. Reference
outputs: `~/Downloads/queckenberg-ocr.pdf`, `~/Downloads/bartscher-ocr.pdf`.
