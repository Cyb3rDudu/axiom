"""#251 r4: spec-legal PageLabels trees whose FIRST entry starts at page
> 0 must not crash the PREFLIGHT path (pdf_health.analyze_pdf /
pdf_processing.extract_page_labels) — the guard previously lived only in
the chunker, the preflight crashed with IndexError (reproduced on
d28d241 with a synthetic PDF).

Mutation-bar: safe_page_label replaced by a direct page.get_label() ->
both tests RED."""
import pymupdf
import pytest
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from fastapi.testclient import TestClient


def _uncovered_pdf(path, n=6):
    doc = pymupdf.open()
    for i in range(n):
        page = doc.new_page()
        page.insert_text((72, 72), f"page body {i} " + "lorem ipsum " * 20)
    # first entry starts at page 1 — page 0 uncovered (spec-legal)
    doc.set_page_labels(
        [{"startpage": 1, "prefix": "", "style": "D", "firstpagenum": 2}]
    )
    doc.save(path)
    doc.close()
    return path


@pytest.fixture()
def client(tmp_path):
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work"))
    try:
        with TestClient(app) as c:
            yield c
    finally:
        settings.set(old)


def test_preflight_survives_uncovered_label_tree(client, tmp_path):
    """THE regression: /v1/pdf/preflight on the synthetic PDF must return
    a report, never a 500 IndexError."""
    import io

    buf = io.BytesIO()
    doc = pymupdf.open()
    for i in range(6):
        page = doc.new_page()
        page.insert_text((72, 72), f"page body {i} " + "lorem ipsum " * 20)
    doc.set_page_labels(
        [{"startpage": 1, "prefix": "", "style": "D", "firstpagenum": 2}]
    )
    doc.save(buf)
    doc.close()
    r = client.post(
        "/v1/pdf/preflight",
        content=buf.getvalue(),
        headers={"Content-Type": "application/pdf"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert "ok" in body, body


def test_pdf_health_analyze_survives_uncovered_tree(tmp_path):
    from axiom_ng_runner.compute_core.pdf_health import analyze_pdf

    p = _uncovered_pdf(tmp_path / "uncovered.pdf")
    res = analyze_pdf(str(p))  # must NOT raise
    assert res is not None


def test_pdf_processing_extract_survives_uncovered_tree(tmp_path):
    from axiom_ng_runner.compute_core.pdf_processing import extract_page_labels

    p = _uncovered_pdf(tmp_path / "uncovered2.pdf")
    labels = extract_page_labels(str(p))  # must NOT raise
    # tier-1 for the covered pages
    assert labels.get(1) or "2" in (labels.get(1) or ""), labels
