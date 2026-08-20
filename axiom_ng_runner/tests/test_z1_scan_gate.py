"""Runner-Härtung Z1: Scan-Gate, Batch-Planung, Signal-Klassifikation.

Rot-Sonden: ein 10-Seiten-Raster-Scan-Fixture muss durchgehen (kein Gate),
ein 658-Seiten-Fake (kleine Seiten, hohes Dekodier-Volumen) muss sauber
OOM-gated werden statt still zu sterben — und SIGKILL als eigene
Fehlerklasse sichtbar sein.
"""
import unittest

from axiom_ng_runner.compute_core.pdf_worker.__main__ import (
    _batch_bounds,
    _classify_child_failure,
    _is_raster_scan,
    _scan_profile,
)


class _FakeImg:
    def __init__(self, w, h):
        self.w, self.h = w, h


class _FakePage:
    def __init__(self, text, imgs):
        self._text, self._imgs = text, imgs

    def get_text(self):
        return self._text

    def get_images(self, full=False):
        return [("xref", None, None, None, None, None, None, self._imgs.w, self._imgs.h)]


class _FakePixInfo(dict):
    pass


class _FakeDoc:
    """Baut get_text/get_images/extract_image aus (text, w, h)-Tripeln."""

    def __init__(self, pages):
        self._pages = pages
        self.page_count = len(pages)

    def __getitem__(self, i):
        text, w, h = self._pages[i]
        return _FakePage(text, _FakeImg(w, h))

    def extract_image(self, xref):
        # Breite/Höhe über den Fake-Kontext: der Worker fragt xref 0 an —
        # wir legen die Maße der Seite in die letzte FakeImg
        page = self._pages[0]
        return _FakePixInfo({"width": page[1], "height": page[2]})


def _fake_doc(n_pages, chars, w, h):
    return _FakeDoc([("" if chars == 0 else str(chars), w, h)] * n_pages)


class ScanGateTests(unittest.TestCase):
    def test_small_scan_passes_ungated(self):
        """Rot-Sonde A: 10-Seiten-Raster muss DURCHGEHEN (kein Gate)."""
        doc = _fake_doc(10, 0, 2000, 3000)  # ~18 MPix ×10 ≈ 540 MB
        prof = _scan_profile(doc)
        self.assertFalse(_is_raster_scan(prof),
                         f"10-Seiten-Raster ist harmlos: {dict(prof)}")

    def test_bartscher_fake_is_gated(self):
        """Rot-Sonde B: 658 Seiten Vollseitenraster ≈7,4 GB → OOM-Gate."""
        doc = _fake_doc(658, 0, 1944, 1944)  # 3,8 MPix ×658 ≈ 7,4 GB RGB
        prof = _scan_profile(doc)
        self.assertEqual(prof["pages"], 658)
        self.assertEqual(prof["chars_per_page"], 0.0)
        self.assertEqual(prof["raster_pages"], 658)
        self.assertGreater(prof["decoded_bytes"], 7_000_000_000)
        self.assertTrue(_is_raster_scan(prof), "Bartscher-Klasse MUSS erkennen")

    def test_textbook_not_gated(self):
        """Normale Bücher (Text vorhanden) nie scalieren."""
        doc = _fake_doc(400, 2000, 500, 400)
        self.assertFalse(_is_raster_scan(_scan_profile(doc)))

    def test_batches_stay_in_budget(self):
        prof = _scan_profile(_fake_doc(658, 0, 1944, 1944))
        bounds = _batch_bounds(prof)
        self.assertTrue(len(bounds) >= 2, "7,4 GB muss in mehrere Batches zerfallen")
        self.assertEqual(bounds[0][0], 0)
        self.assertEqual(bounds[-1][1], 658)
        per_page = prof["decoded_bytes"] // 658
        for start, end in bounds:
            self.assertLessEqual((end - start) * per_page, 1_400_000_000,
                                 "Batch überschreitet Budget")

    def test_sigkill_gets_own_class(self):
        """Z1c: oom_killtes Kind sichtbar — kein stilles 'failed: '."""
        msg = _classify_child_failure(-9)
        self.assertIn("CHILD_OOM_SIGKILL", msg)
        self.assertIn("oom", msg.lower())
        self.assertEqual(_classify_child_failure(0), "")
        self.assertIn("SIGNAL_15", _classify_child_failure(-15))
        self.assertEqual(_classify_child_failure(1), "")


if __name__ == "__main__":
    unittest.main()
