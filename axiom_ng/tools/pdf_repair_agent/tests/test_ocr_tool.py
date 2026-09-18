"""T2 ocr_tool — Fixture-Tests: Textschicht-Diagnose, Abhängigkeits-Ehrlichkeit,
Qualitätstor. Der echte OCR-Lauf hängt an tesseract/ghostscript/ocrmypdf
(fehlen auf diesem System) → hier wird der ehrliche Unfähigkeitszweig geprüft,
nicht geraten. Das Qualitätstor ist eine reine Funktion der Zeichenzahlen und
ist unabhängig von den Binaries testbar."""

from __future__ import annotations

import sys
from pathlib import Path

import pymupdf  # type: ignore[reportMissingImports]
import pytest

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

from tools import (  # noqa: E402  # type: ignore[reportAttributeAccessIssue]
    ocr_tool,
    pdf_kernel,
)

FIX = PKG / "fixtures"
# Verfügbarkeits-Sonde über das WERKZEUG selbst (venv-bewusste Auflösung —
# shutil.which allein wäre der alte Binary-Bug).
HAS_OCR_BINS = all(ocr_tool._bins_available().values())


def _ensure():
    if not (FIX / "ohne_textschicht.pdf").exists():
        from fixtures import generate_fixtures

        generate_fixtures.main()


def test_erkennt_textschicht_fehlend():
    _ensure()
    pl = ocr_tool.plan(FIX / "ohne_textschicht.pdf")
    assert pl["text_layer_ok"] is False
    assert pl["text_layer_missing_pages"] == list(range(8))
    assert pl["raster_scan_hypothesis"] is True


def test_gesund_hat_textschicht():
    _ensure()
    pl = ocr_tool.plan(FIX / "gesund.pdf")
    assert pl["text_layer_ok"] is True
    assert pl["ocr_verdict"] == "not_needed"


def test_qualitaetstor_schuetzt_vor_leerer_textschicht():
    # Leeres/zu dünnes OCR-Ergebnis darf das Tor NICHT passieren.
    assert (
        ocr_tool._quality_report(FIX / "ohne_textschicht.pdf")["quality_gate_pass"]
        is False
    )


def test_qualitaetstor_je_seite_nicht_aggregat():
    """C4-Regression: 1 dichte + mehrere leere Seiten. Das Aggregat-Tor
    (sum >= MIN*len) hätte bestanden — per Seite muss es FAILen."""
    p = FIX.parent / "runs" / "_ocr_gate.pdf"
    p.parent.mkdir(parents=True, exist_ok=True)
    d = pymupdf.open()
    dense = d.new_page(width=595, height=842)
    dense.insert_textbox(
        (40, 60, 555, 780),
        "Volltextseite. " * 200,  # weit über MIN_TEXT_CHARS
        fontsize=10,
    )
    for _ in range(3):
        d.new_page(width=595, height=842)  # leere Seiten (0 Zeichen)
    d.save(str(p))
    d.close()
    q = ocr_tool._quality_report(p)
    assert q["quality_gate_pass"] is False
    assert q["pages_below_min"] == [1, 2, 3]  # nur die leeren, Belegstellen
    assert q["textchars_pages"][0] >= ocr_tool.MIN_TEXT_CHARS
    p.unlink(missing_ok=True)


@pytest.mark.skipif(
    HAS_OCR_BINS, reason="OCR-Binaries vorhanden → echter Lauf, kein Unfähigkeitszweig"
)
def test_apply_ohne_binaries_luegt_nicht():
    _ensure()
    dst = FIX.parent / "runs" / "ocr_refuse_test.pdf"
    # Ohne tesseract/gs/ocrmypdf muss --apply ehrlich ablehnen (kein Raten,
    # keine leere Erfolgsmeldung) und nichts schreiben.
    res = ocr_tool.run_ocr(FIX / "ohne_textschicht.pdf", dst, "deu")
    assert res["applied"] is False
    assert "nicht baubar" in res["cause"]
    assert not dst.exists()


@pytest.mark.skipif(
    not HAS_OCR_BINS,
    reason="OCR-Binaries fehlen → Unfähigkeitszweig hat eigenen Test",
)
def test_ocr_live_lauf_und_qualitaetstor():
    """Echter ocrmypdf-Lauf auf der Text-Pixel-Fixture: Textschicht wird
    gebaut, das per-seite-Tor besteht, die Ausgabedatei trägt Text."""
    _ensure()
    dst = FIX.parent / "runs" / "ocr_live_test.pdf"
    res = ocr_tool.run_ocr(FIX / "ohne_textschicht.pdf", dst, "deu")
    assert res["applied"] is True, res.get("cause")
    assert res["quality"]["quality_gate_pass"] is True
    assert res["quality"]["pages_below_min"] == []
    counts = pdf_kernel.page_char_count(dst)
    assert all(c >= ocr_tool.MIN_TEXT_CHARS for c in counts), counts
    dst.unlink(missing_ok=True)


# ── #286: env-relative bundled OCR binaries (artifact standard) ──────────
# Der Fixer verschiebt tesseract/gs/tessdata ins Artifact-Env; die Auflöse-
# Logik muss ohne Host-PATH funktionieren (Carrier-Szenario). Die Sonde baut
# ein FAKES gebündeltes Env-Layout auf und fährt PATH-sanitiert.


def _fake_bundled_env(tmp_path, echo_mode: bool):
    """Baut bin/tesseract + bin/gs + share/tessdata/{deu,eng}.traineddata
    als Schein-Env; die Skripte protokollieren argv0 + TESSDATA_PREFIX, um
    die Übergabe an den Kindprozess beweisbar zu machen."""
    env = tmp_path / "fakeenv"
    (env / "bin").mkdir(parents=True)
    tdir = env / "share" / "tessdata"
    tdir.mkdir(parents=True)
    for lang in ("deu", "eng"):
        (tdir / f"{lang}.traineddata").write_bytes(b"fake-model")
    for name in ("tesseract", "gs"):
        script = env / "bin" / name
        if echo_mode:
            script.write_text(
                "#!/bin/sh\n"
                'echo "BIN=$0 TESSDATA=[$TESSDATA_PREFIX]"\n'
                "exit 0\n"
            )
        else:
            script.write_text("#!/bin/sh\nexit 0\n")
        script.chmod(0o755)
    return env


def test_bundled_bin_gewinnt_ueber_path(tmp_path, monkeypatch):
    """Env-relativ schlägt PATH: selbst wenn ein ANDERER tesseract auf dem
    PATH liegt, gewinnt sys.prefix/bin/tesseract (Mutationssonde: die
    sys.prefix-Zeile entfernt -> PATH-Fake gewinnt -> rot)."""
    from tools import bundled_env as be

    fake = _fake_bundled_env(tmp_path, echo_mode=False)
    other = tmp_path / "otherbin"
    other.mkdir()
    (other / "tesseract").write_text("#!/bin/sh\nexit 0\n")
    (other / "tesseract").chmod(0o755)
    monkeypatch.setattr(be.sys, "prefix", str(fake))
    monkeypatch.setattr(be.shutil, "which", lambda n: str(other / n))
    got = ocr_tool.bundled_bin("tesseract")
    assert got == str(fake / "bin" / "tesseract"), got


def test_bundled_aufloesung_ohne_host_path(tmp_path, monkeypatch):
    """Carrier-Szenario: PATH SANITIERT (kein Host-tesseract/gs) — die
    Binär-Bilanz bleibt grün, weil das Env bündelt."""
    from tools import bundled_env as be

    fake = _fake_bundled_env(tmp_path, echo_mode=False)
    monkeypatch.setattr(be.sys, "prefix", str(fake))
    monkeypatch.setattr(be.shutil, "which", lambda n: None)  # kein Host
    bins = ocr_tool._bins_available()
    assert bins["tesseract"] and bins["gs"], bins


def test_ocr_child_env_setzt_tessdata_und_path(tmp_path, monkeypatch):
    """Die Kind-Umgebung trägt TESSDATA_PREFIX (gebündelte Modelle) und
    stellt env/bin VORAN — ocrmypdf findet tesseract/gs auch ohne Host."""
    import os as _os

    from tools import bundled_env as be

    fake = _fake_bundled_env(tmp_path, echo_mode=False)
    monkeypatch.setattr(be.sys, "prefix", str(fake))
    monkeypatch.setenv("PATH", "/usr/bin:/bin")
    child = ocr_tool.ocr_child_env()
    assert child["TESSDATA_PREFIX"] == str(fake / "share" / "tessdata")
    assert child["PATH"].startswith(str(fake / "bin") + _os.pathsep), child["PATH"]


def test_dev_venv_ohne_buendel_bleibt_noop(monkeypatch):
    """Dev-Venv ohne gebündelte Binaries: keine PATH-Verfälschung, kein
    TESSDATA_PREFIX — transparenter Host-PATH-Fallback."""
    from tools import bundled_env as be

    monkeypatch.setattr(be, "tessdata_dir", lambda: None)
    monkeypatch.setattr(be.os.path, "exists", lambda p: False)
    child = ocr_tool.ocr_child_env()
    assert "TESSDATA_PREFIX" not in child
    assert not child["PATH"].startswith(str(be.Path(be.sys.prefix) / "bin"))


def test_rebuild_reicht_kind_env_durch(tmp_path, monkeypatch):
    """Prozessgrenzen-Sonde (#286): run_rebuild übergibt die gebündelte
    Kind-Umgebung (TESSDATA_PREFIX + env-bin-PATH) tatsächlich an den
    ocrmypdf-Kindprozess — nicht nur die pure Funktionslogik."""
    import tools.scan_ocr_rebuild as t

    from tools import bundled_env as be

    fake = _fake_bundled_env(tmp_path, echo_mode=True)
    monkeypatch.setattr(be.sys, "prefix", str(fake))
    monkeypatch.setattr(t, "ocrmypdf_bin", lambda: "/usr/bin/false")

    captured = {}

    class _FakeProc:
        returncode = 0
        stderr = ""
        stdout = ""

    def _fake_run(cmd, capture_output, text, timeout, env):
        captured["env"] = env
        return _FakeProc()

    import pymupdf

    monkeypatch.setattr(t.subprocess, "run", _fake_run)
    src = tmp_path / "s.pdf"
    d = pymupdf.open()
    p = d.new_page()
    p.insert_text((50, 50), "x" * 200)
    pix = pymupdf.Pixmap(pymupdf.csRGB, pymupdf.IRect(0, 0, 40, 40))
    pix.clear_with(120)
    p.insert_image(p.rect, pixmap=pix)
    d.save(src)
    d.close()

    # run_rebuild aufrufen; die Verifikation wird weggestubbt (die Sonde
    # misst die Prozessübergabe, nicht die OCR-Qualität)
    monkeypatch.setattr(t, "_page_dims", lambda pdf: [(100.0, 100.0)])
    monkeypatch.setattr(
        t, "_text_layer_metrics", lambda pdf: {"pages": 1, "total_chars": 99, "mean_chars_per_page": 99.0}
    )
    res = t.run_rebuild(src, tmp_path / "out.pdf", lang="deu", timeout_s=30)
    assert res.get("applied"), res
    child = captured["env"]
    assert child["TESSDATA_PREFIX"] == str(fake / "share" / "tessdata")
    assert child["PATH"].startswith(str(fake / "bin") + ":"), child["PATH"]


def test_bundled_env_drift_zwischen_den_baeumen():
    """#286: bundled_env existiert CODE-IDENTISCH in beiden Paketbäumen
    (Fixer = vendored Mirror des Runner-kanonikats — folio_harvest-Muster).
    Drift hier = Drift-Test im Runner ROT und umgekehrt; beide Builds cmp
    zusätzlich vor dem Staging."""
    from pathlib import Path

    repo = Path(__file__).resolve().parents[4]
    canonical = repo / "axiom_ng_runner" / "compute_core" / "bundled_env.py"
    mirror = repo / "axiom_ng" / "tools" / "pdf_repair_agent" / "tools" / "bundled_env.py"
    assert canonical.exists() and mirror.exists(), "beide Bäume müssen die Datei tragen"
    assert canonical.read_text() == mirror.read_text(), (
        "bundled_env drift: Runner-Kanonikat und Fixer-Mirror sind nicht "
        "mehr identisch — synchronisieren (beide Builds cmp-en das auch)"
    )
