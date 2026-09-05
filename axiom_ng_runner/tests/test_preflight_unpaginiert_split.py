"""#254: the unpaginiert verdict splits by TEXT LAYER.

The old 🔴 unpaginiert skip conflated two very different cases:
  - born-digital PDFs without print pagination (text present) — the skip
    was WRONG: they process honestly with physical_only locators
    ("PDF-S. N", #173), searchable and citable (APA section form)
  - textless scans — the skip is right, surfaced as needs_ocr (#252)

Plus the Plantin sub-finding: +1-offset folios in a mid-page running head
(~75% height) the band harvester missed — repairable class, not
unpaginiert. The real file is pinned as testdata fixture (owner-verified:
print = pdf - 1).

Mutation bars:
  - text-layer split removed (🟡 -> 🔴)  -> born-digital test RED
  - bottom band back to 0.88             -> Plantin test RED
  - Go gate: dispatcher suite (scan still skips, 🟡 processes)
"""
from pathlib import Path

import pymupdf
from axiom_ng_runner.compute_core.pdf_health import analyze_pdf, preflight

PLANTIN = Path(__file__).parent / "testdata" / "plantin_2018_infrastructure_studies.pdf"


def _born_digital(path, n=8):
    """Text on every page, NO page labels, NO folios: the no_print_pagination shape."""
    doc = pymupdf.open()
    for i in range(n):
        page = doc.new_page()
        page.insert_text((72, 72), f"section {i} " + "born digital body text without any page folio markers " * 8)
    doc.save(path)
    doc.close()
    return path


def _true_scan(path, n=8):
    """No extractable text at all (blank image-only pages): the needs_ocr shape."""
    doc = pymupdf.open()
    for i in range(n):
        doc.new_page()  # no text, no folio, nothing
    doc.save(path)
    doc.close()
    return path


def test_born_digital_no_folio_processes_with_physical_only(tmp_path):
    """THE split: text present + no folios is NOT a reject — it processes
    with physical_only locators (preflight ok=True, new 🟡 verdict)."""
    pdf = _born_digital(tmp_path / "born.pdf")
    r = preflight(str(pdf))
    assert r.ok is True, f"born-digital must pass, got {r.finding} ({r.reason})"
    assert r.finding == "🟡 no_print_pagination"
    assert r.details["pagination_state"] == "physical_only"
    assert r.details["text_layer"] is True


def test_true_scan_stays_skipped_as_needs_ocr(tmp_path):
    """Gegen-Sonde: a textless scan still rejects — the skip was right for
    THIS class; the surface says needs_ocr (#252)."""
    pdf = _true_scan(tmp_path / "scan.pdf")
    r = preflight(str(pdf))
    assert r.ok is False, "a textless scan must NOT process"
    assert r.finding == "🔴 unpaginiert"
    assert r.details["pagination_state"] == "needs_ocr"
    assert r.details["text_layer"] is False


def test_plantin_fixture_is_repairable_not_unpaginiert():
    """Owner-verified +1-offset folios in a mid-page running head: the file
    belongs in the REPAIRABLE class (write_labels +1), never unpaginiert.
    The testdata copy pins the real layout (folio row at ~75-76% height,
    print = pdf - 1)."""
    assert PLANTIN.exists(), "plantin fixture must be pinned in testdata"
    d = analyze_pdf(str(PLANTIN))
    assert d["finding"] == "🔴 reparierbar", (
        f"Plantin must classify repairable (folio run present), got {d['finding']}"
    )
    assert d["folio_verifiziert"] >= 3
    assert d["versatz"] == -1  # print = pdf - 1 (owner-verified on two samples)


def test_healthy_book_pagination_state_absent(tmp_path):
    """A print-paginated healthy book carries NO pagination_state — the
    #252 derivation only needs the two honest edge states."""
    doc = pymupdf.open()
    for i in range(6):
        page = doc.new_page()
        page.insert_text((72, 30), str(i + 1))  # bare folio, top band
        page.insert_text((72, 72), "healthy body " * 10)
    doc.set_page_labels([{"startpage": 0, "prefix": "", "style": "D", "firstpagenum": 1}])
    doc.save(tmp_path / "healthy.pdf")
    doc.close()
    d = analyze_pdf(str(tmp_path / "healthy.pdf"))
    assert d["finding"] == "🟢 gesund", d["finding"]
    assert d.get("pagination_state") is None


def test_born_digital_page_span_locators_stamp_physical_only():
    """E2E link: a born-digital book has NO page_source_map (no verified
    folios, no labels) — every page_span locator stamps physical_only, the
    state the #173 Go rendering turns into "PDF-S. N" (pinned by the search
    suite). Searchable, citable, honest about what the page number is."""
    from axiom_ng_runner.compute_core import page_trust as pt
    from axiom_ng_runner.runner import _stamp_page_source

    locator = {"type": "page_span", "physical_page_start": 3, "page_label_start": "4"}
    _stamp_page_source(locator, None)  # no map — the born-digital reality
    assert locator["page_source"] == pt.PHYSICAL_ONLY
