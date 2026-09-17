"""#278 — Forensik-Vollständigkeit: Fensterung, Render-Vertrag, stelle3-Ehrlichkeit.

Produktionsfall DAM2Q443 (547 Seiten, Geursen 2022): die Druck-Struktur-Karte
kam nur bis p24 durch die 4000-Zeichen-Kanalgrenze des Agenten-Chats; der
Körper war in der Agenten-Sicht 'null Bytes', Stelle 1 unmessbar, drei Läufe
geparkt. Diese Zeugen pinnen die Reparatur:

  · Kompakt-Digest + Fensterung: die LETZTE Seite erscheint in der
    Agenten-Sicht (render), egal wie groß das Buch — Fenster werden
    lückenlos und überschneidungsfrei durchlaufen
  · Berichts-Evidenz trägt weiterhin die VOLLSTÄNDIGE Karte (map)
  · Render-Konvention im agent_loop: `render` ersetzt den truncierten
    JSON-Dump im Chat; der JSON-Pfad behält seine Grenze
  · stelle3 ehrlich NOT IMPLEMENTED in Probe und truth_source

Mutations-Sonden: Fensterbildung entfernen → last-page-Test rot (der
letzte Fenstercursor läuft nie vorbei); render-Konvention entfernen →
loop-Test rot (Chat erhält wieder 'gekürzt'); stelle3-Ehrlichkeit
entfernen → probe/truth_source-Tests rot.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

pymupdf = pytest.importorskip("pymupdf")

import agent_loop  # type: ignore[import-not-found]  # noqa: E402
import repair_agent  # type: ignore[import-not-found]  # noqa: E402
from config import load_config  # type: ignore[import-not-found]  # noqa: E402
from deepseek_client import MockClient  # type: ignore[import-not-found]  # noqa: E402


def _big_pdf(tmp_path: Path, pages: int = 120) -> Path:
    """>100-Seiten-Buch mit systematischem Offset −1 (Druckfolio = phys−1):
    der Produktionsfall-Klasse nachempfunden (Geursen: Druckseite 1 auf
    PDF 29 — hier schärfer, aber kompakt)."""
    doc = pymupdf.open()
    for i in range(pages):
        page = doc.new_page()
        # Druckfolio in der Kopfzone (bare Zahl), Offset −1 ab p2
        folio = "" if i == 0 else str(i)
        if folio:
            page.insert_text((40, 40), folio, fontsize=12)
        page.insert_text((40, 120), f"KÖRPER-SEITE-{i + 1}", fontsize=16)
    f = tmp_path / "big.pdf"
    doc.save(str(f))
    doc.close()
    return f


def _ctx_for(tmp_path: Path, pdf: Path) -> dict:
    storage = tmp_path / "storage"
    (storage / "BIGKEY1").mkdir(parents=True)
    (storage / "BIGKEY1" / "book.pdf").write_bytes(pdf.read_bytes())
    cfg = load_config({"ZOTERO_STORAGE_ROOT": str(storage)})
    cfg.ensure_dirs()
    return repair_agent._ctx(cfg, "BIGKEY1", allow_apply=False)


def test_big_book_reaches_agent_completely(tmp_path, monkeypatch):
    """DoD: >100 Seiten → die letzte physische Seite erscheint in der
    Evidenz. Beide Ebenen: Agenten-Sicht (render) UND Berichts-Evidenz
    (map ist vollständig — wie im Produktionsreport, nur sah der Agent
    davon nichts)."""
    pdf = _big_pdf(tmp_path, 120)
    ctx = _ctx_for(tmp_path, pdf)
    # Kleines Fenster-Budget erzwingt Mehrfenster-Betrieb (Produktion:
    # 547 Seiten / 15k Budget = 2 Fenster; hier 120 / 900 = mehrere).
    monkeypatch.setattr(repair_agent, "FORENSICS_RENDER_BUDGET", 900)
    seen: list[int] = []
    page_start = None
    renders = 0
    for _ in range(50):  # harte Obergrenze; Abbruch über next_page_start
        step = {"action": "forensics"}
        if page_start:
            step["page_start"] = page_start
        res = repair_agent.h_forensics(step, ctx)
        assert res["ok"], res
        renders += 1
        for ln in res["render"].splitlines():
            if ln.startswith("p") and ln.split()[1].startswith("folio"):
                seen.append(int(ln[1:].split()[0]))
        # Berichts-Evidenz bleibt VOLLSTÄNDIG in JEDEM Aufruf:
        assert res["map"]["page_count"] == 120
        assert len(res["map"]["pages"]) == 120
        nps = res["next_page_start"]
        if nps is None:
            break
        page_start = nps
    assert renders > 1, "kleines Budget muss mehrere Fenster erzwingen"
    # Lückenlos, überschneidungsfrei, KOMPLETT bis zur letzten Seite:
    assert seen == list(range(1, 121)), (
        f"Agenten-Sicht unvollständig/überlappend: {seen[:5]}…{seen[-5:]}"
    )
    assert "KARTE VOLLSTÄNDIG" in res["render"]


def test_single_window_contains_last_page(tmp_path):
    """Ohne Budget-Zwang (Default 15k): ein 120-Seiten-Buch passt in EIN
    Fenster — die letzte Seite ist direkt sichtbar, kein Folgeschritt."""
    pdf = _big_pdf(tmp_path, 120)
    ctx = _ctx_for(tmp_path, pdf)
    res = repair_agent.h_forensics({"action": "forensics"}, ctx)
    assert res["next_page_start"] is None
    assert "p120 " in res["render"] + " " or "\np120" in res["render"]
    assert "p120 folio=119" in res["render"]  # Offset −1 sichtbar


def test_loop_render_convention(monkeypatch):
    """agent_loop: ein Handler-Ergebnis mit `render` geht UNGEKÜRZT in den
    Chat; ohne render bleibt der JSON-Pfad mit seiner Grenze. Sonde: die
    render-Konvention entfernen → der erste Assert wird rot (Chat zeigt
    'gekürzt' bzw. den JSON-Dump)."""
    calls: list[str] = []

    class _Cap(MockClient):
        def complete(self, messages, temperature=0.0):
            calls.append(messages[-1].content)
            return super().complete(messages, temperature)

    big_render = "RENDER-ZEILE\n" * 900  # ~10k Zeichen > JSON-Grenze 4000

    def _handler(_step, _cfg):
        return {"action": "forensics", "ok": True, "render": big_render}

    def _handler_json(_step, _cfg):
        return {"action": "ocr", "ok": True, "payload": "x" * 9000}

    reg = agent_loop.ToolRegistry(handlers={"forensics": _handler})
    client = _Cap(
        ['{"action":"forensics"}', '{"action":"stop","reason":"fertig"}'])
    agent_loop.run_loop(client=client, system_prompt="s", task="t", registry=reg)
    assert len(calls) == 2  # task + evidenz-feedback
    assert "RENDER-ZEILE" in calls[1]
    assert "gekürzt" not in calls[1]

    calls.clear()
    reg2 = agent_loop.ToolRegistry(handlers={"ocr": _handler_json})
    client2 = _Cap(
        ['{"action":"ocr"}', '{"action":"stop","reason":"fertig"}'])
    agent_loop.run_loop(client=client2, system_prompt="s", task="t", registry=reg2)
    assert "gekürzt" in calls[1]  # JSON-Pfad unverändert begrenzt


def test_probe_stelle3_not_implemented(tmp_path, monkeypatch):
    """stelle3 ehrlich als NOT IMPLEMENTED — Operatoren sollen aufhören,
    Annotationen für einen Slot zu liefern, den nichts liest."""
    cfg = load_config({"ZOTERO_STORAGE_ROOT": str(tmp_path / "storage")})
    cfg.ensure_dirs()
    ctx = repair_agent._ctx(cfg, "X1", allow_apply=False)

    # Toter RAG-Port (Sandbox): Ausnahmepfad
    res = repair_agent.h_probe({}, ctx)
    joined = json.dumps(res["offen"], ensure_ascii=False)
    assert "NOT IMPLEMENTED" in joined
    assert "nicht prüfbar" not in joined  # der alte Schein-Befund ist weg

    # Erreichbarer RAG: gleiche Ehrlichkeit im Erfolgsfall
    class _Resp:
        status_code = 200

    import httpx  # type: ignore[import-not-found]

    monkeypatch.setattr(httpx, "get", lambda *a, **k: _Resp())
    res2 = repair_agent.h_probe({}, ctx)
    assert any("NOT IMPLEMENTED" in o for o in res2["offen"])


def test_truth_source_stelle3_not_implemented():
    ts = repair_agent._truth_source([])
    assert "NOT IMPLEMENTED" in str(ts["stelle3_zitat"])
    assert "stelle3_zitat" in ts["offene_stellen"]  # Slot bleibt offen benannt
