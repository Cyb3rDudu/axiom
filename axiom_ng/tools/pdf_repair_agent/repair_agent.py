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


def h_probe(step: dict, ctx: dict) -> dict:
    """Stellen-Sonde (3-Stellen-Beweis): misst hier die RAG-Erreichbarkeit
    (Vorbedingung von Stelle 2). Was NICHT gemessen wurde, steht unter
    `unproven`; fehlende Stellen unter `offen` — nie still behauptet."""
    import httpx  # type: ignore[reportMissingImports]

    cfg = ctx["cfg"]
    base = cfg.rag_api_base
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
                "stelle3_zitat: ohne Zotero-Annotation nicht prüfbar "
                "(nachgelagerter Produktiv-Beweis)",
            ],
            "unproven": ["annotation-label", "chunk-page-exakt"],
        }
    if not reachable:
        return {
            "action": "probe",
            "ok": True,
            "base": base,
            "measured": [],
            "offen": [
                f"stelle2_chunk: RAG antwortet nicht 200 ({detail})",
                "stelle3_zitat: ohne Zotero-Annotation nicht prüfbar",
            ],
            "unproven": ["annotation-label", "chunk-page-exakt"],
        }
    return {
        "action": "probe",
        "ok": True,
        "base": base,
        "detail": detail,
        "measured": ["rag-reachability"],
        "offen": [
            "stelle3_zitat: ohne Zotero-Annotation nicht prüfbar "
            "(nachgelagerter Produktiv-Beweis)"
        ],
        "unproven": [
            "annotation-label",
            "chunk-page-exakt (benötigt Zotero-"
            "Annotationen + chunk-id; nur mit Produktiv-Config)",
        ],
    }


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
    return {
        "action": "forensics",
        "ok": True,
        "pdf": str(work),
        "work_reused": reused,
        "map": m,
        # Qualitäts-Tor als CODE-Evidenz (nicht nur Prompt-Regel): die
        # rauschgefilterten Stelle-1-Anker stehen direkt im Bericht.
        "anchors": forensics_tool.anchor_folio_run(m),
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
    key: str, *, apply: bool = False, client=None, cfg=None, task_extra: str = ""
) -> dict:
    """Vollständiger Agenten-Lauf für einen Key. Liefert den Endbericht als
    dict (identisch zur Audit-Spur unter WORK_ROOT/<key>/report.json)."""
    cfg = cfg or load_config_envfile(HERE / "config.env")
    cfg.ensure_dirs()
    status = config_status(cfg)
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
    task = (
        f"Repariere das PDF des Zotero-Keys '{key}'.\n"
        f"Konfigurationslage: {json.dumps(status, ensure_ascii=False)}\n"
        f"Arbeitskopie: {cfg.work_root / key / 'work.pdf'} "
        f"(alle Schreibzugriffe NUR dort).\n"
        f"Schreibfreigabe: {freigabe}\n"
        f"{task_extra}"
    )
    res = run_loop(
        client=client,
        system_prompt=system_prompt,
        task=task,
        registry=build_registry(),
        cfg=_ctx(cfg, key, allow_apply=apply),
        budget_max_ops=cfg.budget_max_ops,
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
    werden benannt — Information, keine Warnung."""
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
        if (
            r.get("action") == "probe"
            and "rag-reachability" in (r.get("measured") or [])
        ):
            notizen.append(
                "rag_erreichbar (reachability gemessen, kein "
                "Chunk-Seiten-Vergleich)"
            )
    if used["stelle1_druckseite"] is None:
        used["stelle1_druckseite"] = "nicht gemessen"
    offene = [k for k, v in used.items() if v is None]
    used["offene_stellen"] = offene
    if notizen:
        used["notizen"] = notizen
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
        "--config", default=str(HERE / "config.env"), help="Pfad zur config.env"
    )
    a = p.parse_args(argv)
    cfg = load_config_envfile(a.config)
    report = run_agent(a.key, apply=a.apply, cfg=cfg)
    print(json.dumps(report, ensure_ascii=False, indent=1, default=str))
    return 0 if report.get("verdict") not in ("NO-MODEL",) else 1


if __name__ == "__main__":
    sys.exit(main())
