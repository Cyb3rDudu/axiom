"""#175 — /v1/pdf/preflight quality gate before chunking.

Tests the read-only preflight endpoint against the pdf_repair_agent fixture
taxonomy (the exact classes the pipeline must flag before a bad scan becomes
junk chunks):
  gesund.pdf            text layer present + sane labels            -> ok
  falsche_labels.pdf     text layer present + label offset (Versatz) -> ok (🟡)
  ohne_textschicht.pdf   NO text layer (pure image scan)             -> not ok

Run: .venv/bin/python -m pytest tests/test_pdf_preflight.py
"""

from __future__ import annotations

from pathlib import Path

import pytest
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from fastapi.testclient import TestClient

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
FIXTURES = REPO_ROOT / "axiom_ng" / "tools" / "pdf_repair_agent" / "fixtures"


@pytest.fixture()
def client(tmp_path):
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work"))
    try:
        with TestClient(app) as c:
            yield c
    finally:
        settings.set(old)


def _read_fixture(name: str) -> bytes:
    p = FIXTURES / name
    assert p.is_file(), f"fixture missing: {p}"
    return p.read_bytes()


def test_preflight_gesund_passes(client):
    """Healthy book: text layer present, sane labels -> ok=True."""
    r = client.post(
        "/v1/pdf/preflight",
        content=_read_fixture("gesund.pdf"),
        headers={"Content-Type": "application/pdf"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["contract_version"] == "1.0"
    assert body["ok"] is True
    assert "gesund" in body["finding"]
    # Text-layer metrics surfaced (the #175 contract fields).
    d = body["details"]
    assert d["text_layer"] is True
    assert d["pages"] == 12
    assert d["mean_chars_per_page"] > 0
    assert d["suspicious_patterns"] == []


def test_preflight_falsche_labels_ok_but_yellow(client):
    """Label offset (Versatz) with an intact text layer is a repair/🟡 signal,
    not a hard failure — ok stays True so the doc can still be chunked."""
    r = client.post(
        "/v1/pdf/preflight",
        content=_read_fixture("falsche_labels.pdf"),
        headers={"Content-Type": "application/pdf"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["ok"] is True
    assert "Versatz" in body["finding"] or "gesund" in body["finding"]
    assert body["details"]["text_layer"] is True


def test_preflight_ohne_textschicht_rejects(client):
    """Pure image scan without an OCR text layer -> ok=False; the detected
    blank/image-only series is reported as a suspicious pattern."""
    r = client.post(
        "/v1/pdf/preflight",
        content=_read_fixture("ohne_textschicht.pdf"),
        headers={"Content-Type": "application/pdf"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["ok"] is False
    d = body["details"]
    assert d["text_layer"] is False
    assert d["pages"] == 8
    assert d["mean_chars_per_page"] == 0.0
    # suspicious patterns include the blank-series and image-only scan
    assert any("Bildseiten" in p for p in d["suspicious_patterns"])


def test_preflight_empty_body_rejected(client):
    r = client.post(
        "/v1/pdf/preflight", content=b"", headers={"Content-Type": "application/pdf"}
    )
    assert r.status_code == 422
    assert r.json()["detail"]["code"] == "PREFLIGHT_EMPTY"


def test_preflight_invalid_pdf_500(client):
    """Garbage bytes are not a valid PDF -> a loud 500 (the caller policy will
    treat it as un-assessable and fall back to normal processing), never a
    silent ok."""
    r = client.post(
        "/v1/pdf/preflight",
        content=b"not a pdf at all",
        headers={"Content-Type": "application/pdf"},
    )
    assert r.status_code == 500


def test_preflight_capability_reported(client):
    caps = client.get("/v1/capabilities").json()
    assert caps["features"]["pdf_preflight"] is True


def test_preflight_textless_with_sane_labels_is_red(client):
    """Blocker class (review): a textless scan whose page labels happen to be
    sane (unique, monotone, covering) must still be ok=False — the label/folio
    verdict alone is not enough; without a text layer the pipeline would chunk
    junk. ohne_textschicht.pdf is only caught because its labels are ALSO
    broken; this pins the textless+labeled gap the ok=label AND text_layer
    rule closes."""
    import pymupdf

    doc = pymupdf.open()
    for i in range(4):
        page = doc.new_page()
        # A colored rectangle (no text) so get_text is empty but the page has
        # content — the "image-only scan" shape.
        page.draw_rect(pymupdf.Rect(0, 0, 200, 200), color=(0, 0, 0), fill=(1, 1, 1))
    buf = doc.tobytes()
    doc.close()
    # Now apply SANE monotone labels 1..4 to the same bytes.
    d2 = pymupdf.open(stream=buf, filetype="pdf")
    d2.set_page_labels([{"startpage": 0, "style": "D", "firstpagenum": 1}])
    sane_bytes = d2.tobytes()
    d2.close()

    r = client.post(
        "/v1/pdf/preflight",
        content=sane_bytes,
        headers={"Content-Type": "application/pdf"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["ok"] is False, (
        "a textless PDF with sane labels must fail the gate — did you drop "
        "the text_layer requirement?"
    )
    assert body["details"]["text_layer"] is False
    assert body["details"]["pages"] == 4


def test_preflight_result_field_names_english():
    """#219 pin: PreflightResult's dataclass fields are finding/reason.

    The HTTP surface is asserted elsewhere via response keys; this pins the
    internal struct a rename-revert of the dataclass fields (missed by
    dict-based analyzer tests) would break.
    """
    import dataclasses

    from axiom_ng_runner.compute_core import pdf_health

    names = [f.name for f in dataclasses.fields(pdf_health.PreflightResult)]
    assert names == ["ok", "finding", "reason", "details"]
