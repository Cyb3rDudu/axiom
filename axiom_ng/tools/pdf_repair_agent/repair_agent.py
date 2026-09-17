"""repair_agent — Einstiegspunkt des agentischen PDF-Repair-Services (Stufe 2).

    .venv/bin/python repair_agent.py --key <ZOTERO-KEY> [--apply]

Autarker Kasten: eigene Config (config.env / Env, Sandbox-Default),
eigene Schleife (agent_loop), eigener Client (deepseek_client), echte
Handler auf dem Toolbelt T1–T4. Kein Modell schreibt PDF-Bytes — die
Handler rufen ausschließlich die deterministischen Werkzeuge.

Disziplin:
  · Ohne --apply liefern ALLE Handler nur Dry-Run-Evidenz (kein Byte).
  · Schreibzugriffe laufen NUR auf die Arbeitskopie unter WORK_ROOT
    (Backup-Pflicht erfüllt surgery_exec selbst: backup → write →
    read-back → rollback).
  · Der Endbericht (inkl. „was unbewiesen blieb") landet als Audit-Spur
    unter WORK_ROOT/<key>/report.json.
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

from agent_loop import ToolRegistry, run_loop, system_header  # noqa: E402
from config import config_status, load_config_envfile  # noqa: E402
from deepseek_client import DeepSeekClient  # noqa: E402

# ------------------------------------------------------------- Arbeitskopie --


def pdf_for_key(storage_root: Path, key: str) -> Path | None:
    """Erstes PDF im Attachment-Ordner (Zotero-Konvention) oder None."""
    att = storage_root / key
    if not att.is_dir():
        return None
    pdfs = sorted(att.glob("*.pdf"))
    return pdfs[0] if pdfs else None


def ensure_work_copy(cfg, key: str) -> tuple[Path, bool] | None:
    """Arbeitskopie unter WORK_ROOT/<key>/work.pdf — Schreibzugriffe gehen
    NUR hierher, das Storage-Original bleibt unberührt. Rückgabe
    (Pfad, work_reused): work_reused=True heißt, die Kopie stammt aus einem
    FRÜHEREN Lauf (evtl. bereits repariert) — niemals als Storage-Zustand
    deuten."""
    src = pdf_for_key(cfg.zotero_storage_root, key)
    if src is None:
        return None
    run_dir = cfg.work_root / key
    run_dir.mkdir(parents=True, exist_ok=True)
    work = run_dir / "work.pdf"
    reused = work.exists()
    if not reused:
        shutil.copy2(src, work)
    return work, reused


# ----------------------------------------------------------------- Handler --


def _ctx(cfg, key: str, allow_apply: bool) -> dict:
    return {"cfg": cfg, "key": key, "allow_apply": allow_apply}


def _heal_readback_proof(work: Path) -> dict | None:
    """#258: Readback-Beweis am (ggf. geheilten) Exemplar — Katalog-Tree
    EXISTIERT und die Labels sind NICHT-leer. Ohne diesen Beweis ist eine
    Heilung ein ehrliches FAIL: kein healed-Verdict, kein hochladbares
    Artefakt (Pfad-Enforcement, nicht Konvention — der Invoker lädt nur
    bei Exit 0 + Artefakt hoch, und ein unbewiesenes work.pdf wird als
    falsches Artefakt ENTFERNT). Das Prädikat selbst lebt EINMAL in
    labeltree_heal.readback_proof (geteilt mit surgery_exec)."""
    from tools import labeltree_heal  # type: ignore[reportAttributeAccessIssue]

    return labeltree_heal.readback_proof(work)


# #284: Sprache des OCR-Rebuilds — Aufrufer (Invoker) übergibt den
# Metadaten-Default; hier gilt der Owner-Default (deu schlug deu+eng im
# Pilot: „Universitit"-Fehler im Kombimodus).
def _ocr_lang_default() -> str:
    import os

    return os.environ.get("AXIOM_OCR_LANG", "deu")


def _scan_ocr_rebuild_rule(
    cfg, key: str, apply: bool, lang: str = "", force: bool = False
) -> dict | None:
    """#284 Katalog-Regel scan_ocr_rebuild — deterministisch, kein Modell.

    Trigger (Diagnose-Schwellen im Werkzeug selbst):
      - reiner Bildscan (keine Textschicht) → plain-Modus, ODER
      - kaputte Textschicht (defekte Worttrennung, Reder-Klasse: Wörter
        ohne Leerzeichen konkateniert) → force-Modus (--force-ocr
        rasterisiert die kaputte Vektorschicht weg), ODER
      - expliziter force-Modus am Aufruf (Case-Override).

    Heilung (2-in-1, Owner-Ruling): OCRmyPDF-Rebuild (ungedrosselt,
    Kommandoform aus dem Pilot) BAUT die neue Textschicht; DANACH läuft
    die Folio-Verifikation auf der NEUEN Schicht (labeltree_heal —
    Scan + Labels in EINER Heilung). Ist das Folio-Mapping nicht
    darstellbar, heilt die Textschicht allein (physical_only-verarbeitbar);
    die Folio-Frage fällt dann an die normale Klassifikation zurück.

    Rückgabe None = nicht die Klasse (Aufrufer fällt weiter durch).
    """
    import shutil as _shutil

    from tools import (
        labeltree_heal,  # type: ignore[reportAttributeAccessIssue]
        scan_ocr_rebuild,  # type: ignore[reportAttributeAccessIssue]
        surgery_exec,  # type: ignore[reportAttributeAccessIssue]
    )

    wc = ensure_work_copy(cfg, key)
    if wc is None:
        return None
    work, _reused = wc
    diag = scan_ocr_rebuild.diagnose(work)
    mode_force = force or diag.get("mode") == "force"
    # nicht die Klasse: intakte Textschicht ohne force-Override, oder ein
    # degenerates text-/bildfreies Vektor-PDF (kein Scan — OCR könnte
    # nichts liefern) — beides fällt an Label-Regel/Agent zurück.
    if diag.get("mode") is None and not mode_force:
        return None

    run_dir = cfg.work_root / key
    ocr_lang = lang or _ocr_lang_default()
    if not apply:
        report = {
            "key": key,
            "verdict": "report",
            "catalog_rule": "scan_ocr_rebuild",
            "final_step": {
                "action": "report",
                "plan_class": "scan_ocr_rebuild",
                "reason": (
                    f"Diagnose: {diag['class']} · Modus {'force' if mode_force else 'plain'} · "
                    f"Sprache {ocr_lang} — Schreibfreigabe nicht erteilt (Dry-Run)."
                ),
            },
            "evidence": [diag],
            "apply": False,
        }
        run_dir.mkdir(parents=True, exist_ok=True)
        (run_dir / "report.json").write_text(
            json.dumps(report, ensure_ascii=False, indent=1, default=str)
        )
        return report

    # Ein HALT ohne verifizierte Heilung ENTFERNT die Workcopy (#258-
    # Pforte, physisch): der Invoker lädt bei Exit 0 ALLES hoch, was unter
    # work.pdf liegt — ein unverändertes Original dort wäre eine
    # Schein-Heilung mit Upload des kaputten Bytes.
    def _halt(reason: str, evidence: list) -> dict:
        work.unlink(missing_ok=True)
        report = {
            "key": key,
            "verdict": "halt",
            "catalog_rule": "scan_ocr_rebuild",
            "final_step": {
                "action": "rollback",
                "plan_class": "scan_ocr_rebuild",
                "reason": reason,
            },
            "evidence": evidence,
            "apply": True,
        }
        run_dir.mkdir(parents=True, exist_ok=True)
        (run_dir / "report.json").write_text(
            json.dumps(report, ensure_ascii=False, indent=1, default=str)
        )
        return report

    if not diag.get("ocr_available"):
        return _halt(
            "OCR-Werkzeuge fehlen (tesseract/gs/ocrmypdf) — Textschicht "
            "nicht baubar, keine stille Lüge: needs-evidence, nichts "
            "geschrieben.",
            [diag],
        )

    rebuilt = run_dir / "ocr_rebuild.pdf"
    res = scan_ocr_rebuild.run_rebuild(work, rebuilt, lang=ocr_lang, force=mode_force)
    if not res.get("applied"):
        return _halt(
            f"OCR-Rebuild abgelehnt/fehlgeschlagen: {res.get('cause', '?')}",
            [diag, res],
        )

    # Neue Schicht wird die Workcopy (das Storage-Original bleibt unberührt;
    # der Invoker lädt genau work.pdf nach Exit 0 hoch).
    _shutil.copy2(rebuilt, work)

    # 2-in-1: Folio-Verifikation auf der NEUEN Textschicht — Labels
    # aus den jetzt messbaren Folios (dieselbe #258-Ernte-Diziplin wie
    # die Preflight-Klassifikation). Nicht darstellbar → Textschicht-
    # Alleinheilung (physical_only), Folio-Frage fällt an die
    # Normalverarbeitung zurück.
    folio_evidence: dict = {"plan_class": "scan_ocr_rebuild+labeltree"}
    labels = labeltree_heal.heal_labels(work)
    labels_applied = False
    if labels is not None:
        plan = {
            "class": "scan_ocr_rebuild",
            "operations": [
                {
                    "op": "write_labels",
                    "source": str(work),
                    "backup": str(run_dir / "backup.pdf"),
                    "labels": labels,
                    "expected_after": labels,
                }
            ],
        }
        surg = surgery_exec.run_plan(plan, apply=True)
        labels_applied = bool(surg.get("applied"))
        folio_evidence["surgery"] = surg
    proof = _heal_readback_proof(work)
    # #258-Disziplin auch für den 2-in-1-Pfad: ein ANGEWENDETER Label-Write
    # OHNE Readback-Beweis darf nicht im Upload landen (Exit 0 + work.pdf =
    # Upload-Pfad des Invokers). Rollback auf den Backup-Stand — die
    # verifizierte TEXT-Schicht-Heilung bleibt, die Labels fallen ehrlich
    # weg (physical_only).
    labels_rolled_back = False
    if labels_applied and proof is None:
        backup = run_dir / "backup.pdf"
        if backup.exists():
            _shutil.copy2(backup, work)
            labels_applied = False
            labels_rolled_back = True
    folio_evidence["readback"] = proof
    folio_evidence["labels_applied"] = labels_applied
    folio_evidence["labels_rolled_back"] = labels_rolled_back

    # healed gilt, sobald die TEXT-Schicht verifiziert ist (Verifikat im
    # Werkzeug: Seitenzahl/Geometrie unverändert, Schicht vorhanden); die
    # Label-Heilung ist der 2-in-1-Bonus — ihr Ausbleiben (Mapping nicht
    # darstellbar) macht die Textschicht-Heilung nicht ungeschehen.
    report = {
        "key": key,
        "verdict": "healed",
        "catalog_rule": "scan_ocr_rebuild",
        "final_step": {
            "action": "heal",
            "plan_class": "scan_ocr_rebuild",
            "reason": (
                f"OCR-Rebuild ({'force' if mode_force else 'plain'}, {ocr_lang}): "
                f"{res['quality']} · Labels: "
                + (
                    "geheilt (Readback bestätigt)"
                    if proof
                    else (
                        "Label-Write zurückgerollt — KEIN Readback-Beweis "
                        "(#258-Pforte), Textschicht-Heilung allein (physical_only)"
                        if labels_rolled_back
                        else "nicht darstellbar — Textschicht-Heilung allein"
                    )
                )
            ),
        },
        "evidence": [diag, res, folio_evidence],
        "heal_readback": proof,
        "apply": True,
    }
    run_dir.mkdir(parents=True, exist_ok=True)
    (run_dir / "report.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=1, default=str)
    )
    return report


def h_probe(step: dict, ctx: dict) -> dict:
    """Stellen-Sonde (3-Stellen-Beweis): misst hier die RAG-Erreichbarkeit
    (Vorbedingung von Stelle 2). Was NICHT gemessen wurde, steht unter
    `unproven`; fehlende Stellen unter `offen` — nie still behauptet.
    #278: Stelle 3 (Zitat-Beweis über Zotero-Annotation) ist in diesem Build
    NOT IMPLEMENTED — kein Codepfad liest Annotationen. Die Sonde führt das
    explizit (nicht als runtime-Befund „nicht prüfbar"), damit Operatoren
    aufhören, Annotationen für einen Slot zu liefern, den nichts liest."""
    import httpx  # type: ignore[reportMissingImports]

    cfg = ctx["cfg"]
    base = cfg.rag_api_base
    _STELLE3_NOT_IMPLEMENTED = (
        "stelle3_zitat: NOT IMPLEMENTED — kein Codepfad liest "
        "Zotero-Annotationen in diesem Build (nicht liefern; "
        "Upgrade-Pfad #278: Annotation via Zotero-API holen, Zitat an "
        "Position validieren)"
    )
    _ANNOTATION_LABEL_NOT_IMPLEMENTED = (
        "annotation-label: NOT IMPLEMENTED (kein Annotations-Lesepfad)"
    )
    # Wahrheits-Ordnung (Owner-Ruling 23.08.): fehlende Stellen 2/3 sind
    # OFFEN, kein Misserfolg — „unvollständige Sonde" ist KEIN Eskalations-
    # grund; nur UNMESSBARES Signal (Stelle 1) eskaliert. Der Lauf kann
    # deshalb mit forensischer M-Quelle (Stelle 1) weiterarbeiten.
    try:
        r = httpx.get(f"{base}/api/zotero/documents", timeout=5.0)
        reachable = r.status_code == 200
        detail = f"HTTP {r.status_code}"
    except Exception as exc:  # noqa: BLE001 — Beweis, kein Crash
        return {
            "action": "probe",
            "ok": True,
            "base": base,
            "measured": [],
            "offen": [
                "stelle2_chunk: RAG nicht erreichbar "
                f"({type(exc).__name__}) — offene Stelle, heilbar über "
                "Stelle 1 (Druckseite)",
                _STELLE3_NOT_IMPLEMENTED,
            ],
            "unproven": [
                _ANNOTATION_LABEL_NOT_IMPLEMENTED,
                "chunk-page-exakt",
            ],
        }
    if not reachable:
        return {
            "action": "probe",
            "ok": True,
            "base": base,
            "measured": [],
            "offen": [
                f"stelle2_chunk: RAG antwortet nicht 200 ({detail})",
                _STELLE3_NOT_IMPLEMENTED,
            ],
            "unproven": [
                _ANNOTATION_LABEL_NOT_IMPLEMENTED,
                "chunk-page-exakt",
            ],
        }
    return {
        "action": "probe",
        "ok": True,
        "base": base,
        "detail": detail,
        "measured": ["rag-reachability"],
        "offen": [_STELLE3_NOT_IMPLEMENTED],
        "unproven": [
            _ANNOTATION_LABEL_NOT_IMPLEMENTED,
            "chunk-page-exakt (benötigt Zotero-"
            "Annotationen + chunk-id; nur mit Produktiv-Config)",
        ],
    }


# #278: Chat-Budget pro forensics-Fenster (Zeichen). Ein Fenster trägt
# ~350 Seiten Kompakt-Digest; 547-Seiten-Bücher brauchen 2 Aufrufe statt
# unendlich vieler Raten aus einem gekürzten Ein-Blick.
FORENSICS_RENDER_BUDGET = 15000


def h_forensics(step: dict, ctx: dict) -> dict:
    from tools import forensics_tool  # type: ignore[reportAttributeAccessIssue]

    wc = ensure_work_copy(ctx["cfg"], ctx["key"])
    if wc is None:
        return {
            "action": "forensics",
            "ok": False,
            "cause": f"kein PDF für Key '{ctx['key']}' im Storage",
        }
    work, reused = wc
    m = forensics_tool.build_map(work)
    anchors = forensics_tool.anchor_folio_run(m)
    # #278 Fensterung: `page_start` (1-basiert) im step wählt das Fenster;
    # ohne Angabe startet die Karte bei p1. Das Fenster endet am Budget
    # (oder am expliziten `page_end`/Dokumentende) — `next_page_start`
    # führt zum nächsten Fenster, null/fehlt = Karte vollständig beim
    # Agenten. Der KÖRPER wird nie wieder an einer Zeichengrenze beerdigt.
    n = m["page_count"]
    try:
        page_start = int(step.get("page_start") or 1)
    except (TypeError, ValueError):
        page_start = 1
    page_start = max(1, min(page_start, n))
    try:
        page_end = int(step.get("page_end") or 0)
    except (TypeError, ValueError):
        page_end = 0
    # Zero-Progress-Guard: page_end < page_start wäre ein leeres Fenster
    # mit next_page_start == page_start — der Agent drehte sich bis zum
    # Ops-Budget. Unsinniges page_end wird ignoriert (Fenster bis n).
    if page_end and page_end < page_start:
        page_end = 0
    cand = forensics_tool.compact_page_lines(m, page_start, page_end or n)
    lines: list[str] = []
    budget = 0
    end = page_start - 1
    for ln in cand:
        if lines and budget + len(ln) + 1 > FORENSICS_RENDER_BUDGET:
            break
        lines.append(ln)
        budget += len(ln) + 1
        end += 1
    next_page_start = end + 1 if end < n else None
    anchor_desc = (
        f"p{anchors[0]['page'] + 1}..p{anchors[-1]['page'] + 1} "
        f"(folio {anchors[0]['folio']}..{anchors[-1]['folio']})"
        if anchors
        else "kein Lauf über dem Qualitäts-Tor"
    )
    render = "\n".join(
        [
            "forensics T3 · Druck-Struktur-Karte (Kompaktansicht, #278)",
            f"seiten: {n} · folio_monoton: {m['folio_sequence_monotonic']} · "
            f"anker_lauf: {anchor_desc}",
            f"folio_spruenge: {len(m['folio_anomalies'])} · "
            f"titelei: {m['titelei_pages']} · iv: {m['toc_pages']}",
            f"fenster: p{page_start}..p{end}"
            + (
                f" · next_page_start: {next_page_start} "
                "(weiter mit page_start im nächsten forensics-step)"
                if next_page_start
                else " · KARTE VOLLSTÄNDIG (letztes Fenster)"
            ),
            *lines,
        ]
    )
    return {
        "action": "forensics",
        "ok": True,
        "pdf": str(work),
        "work_reused": reused,
        # Volle Karte — unverändert die Berichts-Evidenz (Audit-Spur).
        "map": m,
        # Qualitäts-Tor als CODE-Evidenz (nicht nur Prompt-Regel): die
        # rauschgefilterten Stelle-1-Anker stehen direkt im Bericht.
        "anchors": anchors,
        "next_page_start": next_page_start,
        "render": render,
    }


def h_spread(step: dict, ctx: dict) -> dict:
    from tools import spread_tool  # type: ignore[reportAttributeAccessIssue]

    wc = ensure_work_copy(ctx["cfg"], ctx["key"])
    if wc is None:
        return {"action": "spread", "ok": False, "cause": "kein PDF"}
    work, _ = wc
    want_apply = bool(step.get("apply") and ctx["allow_apply"])
    if not want_apply:
        return {
            "action": "spread",
            "ok": True,
            "applied": False,
            "plan": spread_tool._plan(work, spread_tool.DEFAULT_OFFSET),
        }
    dst = work.parent / "spread_split.pdf"
    return {
        "action": "spread",
        "ok": True,
        "applied": True,
        "result": spread_tool.split_and_write(work, dst, spread_tool.DEFAULT_OFFSET),
    }


def h_ocr(step: dict, ctx: dict) -> dict:
    from tools import ocr_tool  # type: ignore[reportAttributeAccessIssue]

    wc = ensure_work_copy(ctx["cfg"], ctx["key"])
    if wc is None:
        return {"action": "ocr", "ok": False, "cause": "kein PDF"}
    work, _ = wc
    pl = ocr_tool.plan(work)
    if not (step.get("apply") and ctx["allow_apply"]):
        return {"action": "ocr", "ok": True, "applied": False, "plan": pl}
    dst = work.parent / "ocr.pdf"
    return {
        "action": "ocr",
        "ok": True,
        "applied": True,
        "plan": pl,
        "result": ocr_tool.run_ocr(work, dst),
    }


def h_surgery(step: dict, ctx: dict) -> dict:
    from tools import surgery_exec  # type: ignore[reportAttributeAccessIssue]

    wc = ensure_work_copy(ctx["cfg"], ctx["key"])
    if wc is None:
        return {"action": "surgery", "ok": False, "cause": "kein PDF"}
    work, _ = wc
    plan_doc = {
        "operations": [
            {
                "op": "write_labels",
                "source": str(work),
                "backup": str(work.parent / "backup.pdf"),
                "labels": op.get("labels"),
                "expected_after": op.get("expected_after"),
            }
            for op in step.get("operations", [])
            if isinstance(op, dict)
        ]
    }
    want_apply = bool(step.get("apply") and ctx["allow_apply"])
    res = surgery_exec.run_plan(plan_doc, apply=want_apply)
    ok = res.get("valid") and not any(
        o.get("rolled_back") or o.get("applied") == False  # noqa: E712
        for o in res.get("operations", [])
    )
    return {
        "action": "surgery",
        "ok": bool(ok),
        "plan_class": step.get("plan_class"),
        "result": res,
    }


def build_registry() -> ToolRegistry:
    return ToolRegistry(
        handlers={
            "probe": h_probe,
            "forensics": h_forensics,
            "spread": h_spread,
            "ocr": h_ocr,
            "surgery": h_surgery,
        }
    )


# -------------------------------------------------------------------- main --


def make_client(cfg):
    if not cfg.deepseek_api_key:
        return None
    return DeepSeekClient(cfg.deepseek_api_key, cfg.deepseek_base_url, cfg.model)


def run_agent(
    key: str,
    *,
    apply: bool = False,
    client=None,
    cfg=None,
    task_extra: str = "",
    ocr_lang: str = "",
    ocr_force: bool = False,
) -> dict:
    """Vollständiger Agenten-Lauf für einen Key. Liefert den Endbericht als
    dict (identisch zur Audit-Spur unter WORK_ROOT/<key>/report.json)."""
    # #251: Default-Pfad ist der bewegliche Operator-Ort (Env/~/config),
    # fehlende Datei ist OK (Env-/Sandbox-Betrieb) — nie das read-only
    # Artifact als hartes Default-Todesurteil.
    cfg = cfg or load_config_envfile(None)
    cfg.ensure_dirs()
    status = config_status(cfg)

    # #284: Katalog-Regel scan_ocr_rebuild VOR der Label-Regel — ein
    # kaputter Textlayer (force-Modus) muss wegrastert werden, BEVOR die
    # Label-Heilung auf der (kaputten) Schicht Folios lesen würde; ein
    # reiner Scan hat keine Schicht und fällt hier wie dort durch.
    scan_report = _scan_ocr_rebuild_rule(
        cfg, key, apply, lang=ocr_lang, force=ocr_force
    )
    if scan_report is not None:
        return scan_report

    # #253: deterministische Stelle-1-Katalog-Regel VOR dem Agentenlauf —
    # "Label-Tree fehlt/leerer Strunk + Textschicht vorhanden" ist
    # beweisbar-sicher (write_labels aus den Folios der Textschicht),
    # braucht kein Modell und keine Stelle-2/3-Vorbedingung. Auch ohne
    # DEEPSEEK-Key heilbar.
    wc = ensure_work_copy(cfg, key)
    work: Path | None = None
    if wc is not None:
        work, _reused = wc  # reuse flag irrelevant here: fast path re-plans
        from tools import labeltree_heal  # type: ignore[reportAttributeAccessIssue]

        verdict = labeltree_heal.would_heal(work)
        if verdict.get("would_heal"):
            labels = verdict["labels"]
            run_dir = cfg.work_root / key
            plan = {
                "class": "labeltree-missing",
                "operations": [
                    {
                        "op": "write_labels",
                        "source": str(work),
                        "backup": str(run_dir / "backup.pdf"),
                        "labels": labels,
                        "expected_after": labels,
                    }
                ],
            }
            if not apply:
                report = {
                    "key": key,
                    "verdict": "report",
                    "catalog_rule": "labeltree-missing+textlayer -> write_labels",
                    "final_step": {
                        "action": "report",
                        "plan_class": "labeltree-missing",
                        "reason": "Katalog-Regel (#253): Tree fehlt/leerer "
                        "Strunk + Textschicht vorhanden — "
                        "beweisbar-sichere write_labels-Operation, "
                        "Schreibfreigabe nicht erteilt (Dry-Run).",
                    },
                    "evidence": [verdict],
                    "config": status,
                    "apply": False,
                }
                run_dir.mkdir(parents=True, exist_ok=True)
                (run_dir / "report.json").write_text(
                    json.dumps(report, ensure_ascii=False, indent=1, default=str)
                )
                return report
            from tools import surgery_exec  # type: ignore[reportAttributeAccessIssue]

            res = surgery_exec.run_plan(plan, apply=True)
            applied = bool(res.get("applied"))
            # #258: healed-Verdict NUR mit Readback-Beweis am geheilten
            # Exemplar — ohne Beweis ehrliches FAIL + Artefakt weg.
            proof = _heal_readback_proof(work)
            # cause liegt bei Operationsebene (res["cause"] nur bei
            # Validierungsfehler) — ehrlich lesen, nicht None zeigen.
            op_cause = res.get("cause") or next(
                (o.get("cause") for o in res.get("operations", []) if o.get("cause")),
                None,
            )
            report = {
                "key": key,
                "verdict": "healed" if applied and proof else "halt",
                "catalog_rule": "labeltree-missing+textlayer -> write_labels",
                "heal_readback": proof,
                "final_step": {
                    "action": "heal" if applied and proof else "rollback",
                    "plan_class": "labeltree-missing",
                    "reason": (
                        "Katalog-Regel (#253): write_labels ausgeführt, "
                        "Read-Back bestätigt."
                        if applied and proof
                        else (
                            f"write_labels abgelehnt: {op_cause}"
                            if not applied
                            else "write_labels angewendet, aber KEIN Readback-"
                            "Beweis (Tree/Labels) — ehrliches FAIL, kein "
                            "Upload (#258)"
                        )
                    ),
                },
                "evidence": [verdict, res],
                "config": status,
                "apply": True,
            }
            if proof is None:
                # Unbewiesenes work.pdf ist ein falsches Artefakt: der
                # Invoker liest genau diesen Pfad nach Exit 0 — weg damit.
                work.unlink(missing_ok=True)
            run_dir.mkdir(parents=True, exist_ok=True)
            (run_dir / "report.json").write_text(
                json.dumps(report, ensure_ascii=False, indent=1, default=str)
            )
            return report

    if client is None:
        client = make_client(cfg)
    if client is None:
        report = {
            "key": key,
            "verdict": "NO-MODEL",
            "cause": "DEEPSEEK_API_KEY nicht gesetzt — kein agentischer Lauf. "
            "Config setzen (config.env) und erneut starten.",
            "config": status,
        }
        # Auch der Abbruch schreibt die Audit-Spur — ein STALER Bericht
        # eines früheren Laufs darf nie vom Rückgabewert abweichen.
        run_dir = cfg.work_root / key
        run_dir.mkdir(parents=True, exist_ok=True)
        (run_dir / "report.json").write_text(
            json.dumps(report, ensure_ascii=False, indent=1, default=str)
        )
        return report

    system_prompt = (HERE / "prompts" / "system.txt").read_text() + system_header()
    freigabe = "erteilt (--apply)" if apply else "NICHT erteilt (nur Dry-Run)"
    zeit_budget = (
        f"{cfg.budget_max_seconds}s" if cfg.budget_max_seconds > 0 else "keines"
    )
    task = (
        f"Repariere das PDF des Zotero-Keys '{key}'.\n"
        f"Konfigurationslage: {json.dumps(status, ensure_ascii=False)}\n"
        f"Arbeitskopie: {cfg.work_root / key / 'work.pdf'} "
        f"(alle Schreibzugriffe NUR dort).\n"
        f"Schreibfreigabe: {freigabe}\n"
        f"Zeit-Budget: {zeit_budget} pro Case; "
        f"Operationen-Budget: {cfg.budget_max_ops}.\n"
        f"{task_extra}"
    )
    res = run_loop(
        client=client,
        system_prompt=system_prompt,
        task=task,
        registry=build_registry(),
        cfg=_ctx(cfg, key, allow_apply=apply),
        budget_max_ops=cfg.budget_max_ops,
        budget_max_seconds=cfg.budget_max_seconds,
    )
    report = {
        "key": key,
        "verdict": res.reason,
        "final_step": res.final_step,
        "ops_used": res.ops_used,
        "steps": res.history,
        "evidence": res.results,
        "truth_source": _truth_source(res.results),
        "unproven": _unproven_collect(res.results),
        "config": status,
        "apply": apply,
    }
    # #258 finale Pforte auch für den Agentenpfad: Exit 0 + work.pdf beim
    # Invoker bedeutet Upload — das Artefakt darf NUR mit Readback-Beweis
    # bleiben (Agenten-Heilungen über h_surgery tragen den Beweis in der
    # Op-Evidenz; die Pforte misst ihn unabhängig am Exemplar selbst).
    if apply and work is not None:
        proof = _heal_readback_proof(work)
        report["heal_readback"] = proof
        if proof is None:
            work.unlink(missing_ok=True)
    run_dir = cfg.work_root / key
    run_dir.mkdir(parents=True, exist_ok=True)
    (run_dir / "report.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=1, default=str)
    )
    return report


def _truth_source(results: list) -> dict:
    """Wahrheits-Ordnung (Owner-Ruling 23.08.): Transparenz über die
    genutzten Stellen. Stelle 1 (Druckseite) ist der Standardweg; Stelle 2
    (Chunk) und Stelle 3 (Zitat) gelten nur als GEMESSEN, wenn ein
    Chunk-Seiten-Vergleich bzw. Annotation-Check in der Evidenz liegt —
    beides existiert (noch) auf keinem Codepfad, beide bleiben daher
    offen; RAG-Erreichbarkeit ist eine Notiz, kein Beweis. Offene Stellen
    werden benannt — Information, keine Warnung.
    #278: Stelle 3 wird als NOT IMPLEMENTED geführt — ein deklarierter,
    aber unimplementierter Slot ist stille Falsheit, wenn Ausgaben ihn
    wie einen prüfbaren Behelf behandeln."""
    used: dict[str, str | list[str] | None] = {
        "stelle1_druckseite": None,
        "stelle2_chunk": None,
        "stelle3_zitat": None,
    }
    notizen: list[str] = []
    for r in results or []:
        if not isinstance(r, dict):
            continue
        if r.get("action") == "forensics" and r.get("ok"):
            used["stelle1_druckseite"] = "forensics_tool (Druckstruktur-Karte)"
        if r.get("action") == "probe" and "rag-reachability" in (
            r.get("measured") or []
        ):
            notizen.append(
                "rag_erreichbar (reachability gemessen, kein Chunk-Seiten-Vergleich)"
            )
    if used["stelle1_druckseite"] is None:
        used["stelle1_druckseite"] = "nicht gemessen"
    offene = [k for k, v in used.items() if v is None]
    used["offene_stellen"] = offene
    if notizen:
        used["notizen"] = notizen
    if used["stelle3_zitat"] is None:
        used["stelle3_zitat"] = "NOT IMPLEMENTED (kein Annotations-Lesepfad)"
    return used


def _unproven_collect(results: list) -> list[str]:
    """Sammelt jede `unproven`/`cause`-Angabe der Evidenz — Pflicht-Abschnitt
    „was unbewiesen blieb" speist sich hieraus."""
    out = []
    for r in results or []:
        if not isinstance(r, dict):
            continue
        out.extend(r.get("unproven") or [])
        out.extend(r.get("offen") or [])
        if r.get("ok") == False and r.get("cause"):  # noqa: E712
            out.append(f"{r.get('action', '?')}: {r['cause']}")
    return out


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="repair_agent",
        description="agentischer PDF-Repair-Service (Stufe 2, #203)",
    )
    p.add_argument("--key", required=True, help="Zotero attachment KEY")
    p.add_argument(
        "--apply", action="store_true", help="Schreibfreigabe (Default: Dry-Run)"
    )
    p.add_argument(
        "--config",
        default=None,
        help="Pfad zur config.env (Default: AXIOM_FIXER_CONFIG → "
        "~/.config/axiom/fixer.config.env → Repo-config.env; ein fehlender "
        "DEFAULT ist Sandbox/Env-Betrieb, ein fehlender EXPLIZITER Pfad "
        "stirbt laut)",
    )
    p.add_argument(
        "--lang",
        default="",
        help="OCR-Sprache (#284; Default: Metadaten-Default des Aufrufers "
        "bzw. deu — deu schlug deu+eng im Pilot)",
    )
    p.add_argument(
        "--ocr-mode",
        choices=["auto", "force"],
        default="auto",
        help="OCR-Modus (#284): auto = Diagnose entscheidet (reiner Scan "
        "plain, kaputte Worttrennung force); force = Textschicht immer "
        "wegrastern (--force-ocr, Reder-Klasse)",
    )
    a = p.parse_args(argv)
    cfg = load_config_envfile(a.config)
    report = run_agent(
        a.key,
        apply=a.apply,
        cfg=cfg,
        ocr_lang=a.lang,
        ocr_force=(a.ocr_mode == "force"),
    )
    print(json.dumps(report, ensure_ascii=False, indent=1, default=str))
    return 0 if report.get("verdict") not in ("NO-MODEL",) else 1


if __name__ == "__main__":
    sys.exit(main())
