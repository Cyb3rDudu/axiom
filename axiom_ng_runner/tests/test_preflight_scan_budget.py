"""#292 — fail-closed scan classification for huge scans + bounded pre-check.

The Bartscher E2E (658 p, 192 MB, textless) expired the preflight request at
the fixed ~15s budget; the advisory gate treated the timeout as
skip-and-proceed and the giant scan re-entered internal OCR — exactly the
books #288 classifies are the ones whose measurement dies first. These tests
pin the closure on the runner side:

  1. A huge textless scan (page count over the old budget's reach) is
     classified 🔴 scan-ohne-textlayer from a 5-page sampled pre-check in
     milliseconds — repair class, NOT internal processing. The routing half
     (this report → skipped job + repair case, zero /v1/process hits) is
     pinned Go-side by TestPreflightFailSkipsJobAndCreatesRepairCase.
  2. A large-but-texty PDF keeps the FULL measurement (no short-circuit) —
     the dispatcher's scaled budget (15s + 100ms/page, Go side) gives it
     time; here we prove the runner never trades the full pass away.
  3. The pre-check answers in bounded time regardless of file size.

Synthetic fixtures are generated in-test (rasterizing one page once, then
sharing the image xref — a 300-page scan builds in <1s). Mutation safety:
removing _scan_precheck turns test 1/3 red (scan_precheck marker absent,
sampled details gone); making it greedy turns test 2 red (per_page_density
truncated to the sample).

Run: PYTHONPATH=<repo-root> .venv/bin/python -m pytest tests/test_preflight_scan_budget.py
"""

from __future__ import annotations

import contextlib
import time
from pathlib import Path

import pymupdf  # type: ignore[reportMissingImports]
from axiom_ng_runner.app import app
from axiom_ng_runner.compute_core import pdf_health as ph
from axiom_ng_runner.config import Settings, settings
from fastapi.testclient import TestClient

W, H = pymupdf.paper_size("a4")
N_SCAN = 300  # > PRECHECK_MIN_PAGES — the pre-check's jurisdiction
N_TEXTY = 300


def _raster_page_jpg(dpi: int = 100) -> bytes:
    """One rasterized text page (pixels only — get_text stays empty)."""
    src = pymupdf.open()
    sp = src.new_page(width=W, height=H)
    sp.insert_text((60, 120), "Scankapitel", fontsize=24)
    sp.insert_textbox(
        (60, 160, W - 60, H - 80),
        "Synthetische Scanseite mit gedrucktem Text, der nur als Pixel existiert. " * 6,
        fontsize=14,
    )
    pix = sp.get_pixmap(dpi=dpi)
    jpg = pix.tobytes("jpg")
    src.close()
    return jpg


def _scan_bytes(n: int, dpi: int = 100) -> bytes:
    """Huge textless scan: n image-only pages sharing one raster xref
    (cheap to build — the pre-check never decodes image streams, so the
    shared xref exercises the same path as 658 distinct Bartscher pages)."""
    jpg = _raster_page_jpg(dpi)
    d = pymupdf.open()
    for _ in range(n):
        page = d.new_page(width=W, height=H)
        page.insert_image(page.rect, stream=jpg)
    b = d.tobytes(deflate=True)
    d.close()
    return b


def _texty_bytes(n: int) -> bytes:
    """Large born-digital text PDF — body text, no folio numbers, no labels
    → 🟡 no_print_pagination (processes normally with physical_only)."""
    d = pymupdf.open()
    for i in range(n):
        page = d.new_page(width=W, height=H)
        page.insert_textbox(
            (40, 130, W - 40, H - 60),
            f"Seite {i + 1} trägt gesetzten Fließtext ohne Foliozahl im Kopf. " * 6,
            fontsize=11,
        )
    b = d.tobytes(deflate=True)
    d.close()
    return b


@contextlib.contextmanager
def _client(tmp_path: Path):
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work"))
    try:
        with TestClient(app) as c:
            yield c
    finally:
        settings.set(old)


def test_huge_textless_scan_repair_class_via_precheck(tmp_path):
    """DoD 1: textless giant scan → repair class (ok=False, needs_ocr),
    answered by the sampled pre-check in bounded time — never a timed-out
    measurement that advisory-skips into internal OCR."""
    with _client(tmp_path) as c:
        t0 = time.perf_counter()
        r = c.post(
            "/v1/pdf/preflight",
            content=_scan_bytes(N_SCAN),
            headers={"Content-Type": "application/pdf"},
        )
        elapsed = time.perf_counter() - t0
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["ok"] is False
    assert body["finding"] == ph.SCAN_FINDING
    d = body["details"]
    # The verdict came from the #292 pre-check — removing it turns this red
    # (the full pass classifies the same scan, but only after measuring all
    # pages; the marker plus the sampled details are the contract here).
    assert d["scan_precheck"] is True
    assert d["text_layer"] is False
    assert d["pagination_state"] == "needs_ocr"
    assert d["pages"] == N_SCAN
    assert len(d["per_page_density"]) == ph.PRECHECK_SAMPLE
    # Bounded time: the sampled answer must not scale with the document.
    # Ceiling is generous (first-call pymupdf warm-up included) but far
    # below any full-measurement timeout regime.
    assert elapsed < 10.0, f"pre-check answered in {elapsed:.1f}s — not bounded"


def test_texty_large_pdf_gets_full_measurement(tmp_path):
    """DoD 2 (runner half): large-but-texty PDF → the pre-check declines
    (sample carries text) and the FULL measurement runs — normal processing
    with complete per-page details, not a sampled short-circuit."""
    with _client(tmp_path) as c:
        r = c.post(
            "/v1/pdf/preflight",
            content=_texty_bytes(N_TEXTY),
            headers={"Content-Type": "application/pdf"},
        )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["ok"] is True, body["finding"]
    assert body["details"]["text_layer"] is True
    assert "scan_precheck" not in body["details"]
    # Full pass ran: per-page density covers EVERY page.
    assert len(body["details"]["per_page_density"]) == N_TEXTY


def test_precheck_bounded_time_oversized_scan(tmp_path):
    """DoD 3: the pre-check answers in bounded time independent of file
    size — a fatter raster per page (4× dpi) multiplies bytes, not latency."""
    big = tmp_path / "oversized_scan.pdf"
    big.write_bytes(_scan_bytes(N_SCAN, dpi=200))
    doc = pymupdf.open(big)
    try:
        t0 = time.perf_counter()
        verdict = ph._scan_precheck(doc, doc.page_count, big.stat().st_size)
        elapsed = time.perf_counter() - t0
    finally:
        doc.close()
    assert verdict is not None and verdict["finding"] == ph.SCAN_FINDING
    assert elapsed < 1.0, f"pre-check took {elapsed:.2f}s — scales with size?"


def test_precheck_unit_arms(tmp_path):
    """The pre-check's decision arms, pinned directly:
    small docs → None (full path, fixture taxonomy unchanged);
    texty giants → None; image-arm and bytes-arm giants → scan verdict."""
    texty = tmp_path / "texty.pdf"
    texty.write_bytes(_texty_bytes(80))
    doc = pymupdf.open(texty)
    try:
        assert ph._scan_precheck(doc, doc.page_count, texty.stat().st_size) is None
    finally:
        doc.close()

    scan = tmp_path / "scan.pdf"
    scan.write_bytes(_scan_bytes(80))
    doc = pymupdf.open(scan)
    try:
        # image arm: sampled pages all image-only (real bytes/page is tiny
        # here, so only the ≥half-images arm can fire)
        v = ph._scan_precheck(doc, doc.page_count, scan.stat().st_size)
        assert v is not None and v["finding"] == ph.SCAN_FINDING
        # bytes arm alone: same doc, size forced above the ratio threshold
        v = ph._scan_precheck(doc, doc.page_count, ph.PRECHECK_BYTES_PER_PAGE * 80)
        assert v is not None and v["finding"] == ph.SCAN_FINDING
    finally:
        doc.close()

    # neither arm (no images, small size) → full pass: a blank-vector doc
    blank = tmp_path / "blank.pdf"
    d = pymupdf.open()
    for _ in range(80):
        d.new_page(width=W, height=H)
    blank.write_bytes(d.tobytes())
    d.close()
    doc = pymupdf.open(blank)
    try:
        assert ph._scan_precheck(doc, doc.page_count, blank.stat().st_size) is None
    finally:
        doc.close()

    # n ≤ PRECHECK_MIN_PAGES: never short-circuit (8-page fixture class …)
    small = tmp_path / "small.pdf"
    small.write_bytes(_scan_bytes(ph.PRECHECK_MIN_PAGES))
    doc = pymupdf.open(small)
    try:
        assert ph._scan_precheck(doc, doc.page_count, ph.PRECHECK_BYTES_PER_PAGE * 99) is None
    finally:
        doc.close()


def test_small_scan_fixture_still_fully_measured(tmp_path):
    """Guard: the committed fixture taxonomy (≤12 pages) keeps the full
    path — details carry all pages, no sampled short-circuit."""
    fixture = (
        Path(__file__).resolve().parents[2]
        / "axiom_ng"
        / "tools"
        / "pdf_repair_agent"
        / "fixtures"
        / "ohne_textschicht.pdf"
    )
    r = ph.preflight(str(fixture))
    assert r.ok is False
    assert r.finding == ph.SCAN_FINDING
    assert "scan_precheck" not in r.details
    assert len(r.details["per_page_density"]) == r.details["pages"]
