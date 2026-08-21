"""Runner-Härtung Z3: EPUB-Pfadnormalisierung vor pandoc.

Beweislage (Jobs CVM26KLA/FFMTJA3S): OPF referenziert Ziele unnormalisiert
('../NL$…/OEBPS/Text/Cover.xhtml'); pandoc bricht ab, obwohl ALLE
Manifest-Ziele nach POSIX-Normalisierung existieren (105/105 am echten
Buch bewiesen). Die normalisierte Kopie rewritet href/src nur auf Ziele,
die real im Archiv liegen (nie Kaputtes erfinden).
"""
import glob
import unittest
import zipfile
from pathlib import Path

from axiom_ng_runner.compute_core.epub_worker.__main__ import _normalized_epub_copy


def _synthetic_bad_epub(path: Path) -> None:
    """Mini-EPUB mit der echten Fehlerklasse: OPF in OEBPS/, Ziele ../-relativ."""
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml",
                   '<container><rootfiles><rootfile full-path="OEBPS/content.opf"/></rootfiles></container>')
        z.writestr("OEBPS/content.opf",
                   '<manifest><item href="../Pkg/OEBPS/Text/Cover.xhtml" id="c"/>'
                   '<item href="../Pkg/OEBPS/Text/Ch1.xhtml" id="c1"/></manifest>')
        z.writestr("Pkg/OEBPS/Text/Cover.xhtml", "<html><body>Cover</body></html>")
        z.writestr("Pkg/OEBPS/Text/Ch1.xhtml", "<html><body>Kapitel</body></html>")


class EpubNormalizationTests(unittest.TestCase):
    def test_synthetic_bad_epub_gets_normalized(self):
        import tempfile
        with tempfile.TemporaryDirectory() as td:
            src = Path(td) / "bad.epub"
            _synthetic_bad_epub(src)
            out = _normalized_epub_copy(src, Path(td))
            self.assertNotEqual(out, src, "unnormalisierte OPF muss eine Kopie erzeugen")
            with zipfile.ZipFile(out) as z:
                names = set(z.namelist())
                self.assertIn("axiom_content.opf", names, "OPF liegt an der Wurzel")
                cont = z.read("META-INF/container.xml").decode()
                self.assertIn("axiom_content.opf", cont, "container.xml zeigt auf das Wurzel-OPF")
                opf = z.read("axiom_content.opf").decode()
                self.assertNotIn("../", opf, "die Kopie darf keine ..-Referenzen mehr tragen")
                self.assertIn("Pkg/OEBPS/Text/Cover.xhtml", opf)
                for name in names:
                    if name.endswith(".xhtml"):
                        z.read(name)  # alle Einträge lesbar

    def test_clean_epub_fast_path(self):
        import tempfile
        with tempfile.TemporaryDirectory() as td:
            src = Path(td) / "clean.epub"
            with zipfile.ZipFile(src, "w") as z:
                z.writestr("OEBPS/content.opf", '<manifest><item href="Text/A.xhtml"/></manifest>')
                z.writestr("OEBPS/Text/A.xhtml", "<html/>")
            self.assertEqual(_normalized_epub_copy(src, Path(td)), src,
                             "bereits normalisiert: Original-Pfad, keine Kopie")

    def test_real_epub_all_targets_resolve(self):
        """Das echte Buch (Zotero-Storage) als Regression-Fixture, falls lokal vorhanden."""
        candidates = glob.glob("/Users/dudu/Zotero/storage/FFMTJA3S/*.epub")
        if not candidates:
            self.skipTest("echtes EPUB nicht lokal vorhanden (Carrier-Pfad)")
        import tempfile
        with tempfile.TemporaryDirectory() as td:
            out = _normalized_epub_copy(Path(candidates[0]), Path(td))
            with zipfile.ZipFile(out) as z:
                names = set(z.namelist())
                opf = next(n for n in names if n.lower().endswith(".opf"))
                src = z.read(opf).decode("utf-8", "replace")
                import re
                hrefs = re.findall(r'href="([^"]+)"', src)
                self.assertTrue(hrefs)
                for h in hrefs:
                    # pandoc-Semantik am Wurzel-OPF: LITERALER Lookup — der
                    # href muss OHNE Normalisierung existieren
                    self.assertIn(h, names, f"pandoc-literal fehlgeschlagen: {h}")
                self.assertNotIn("../", src, "die Kopie trägt keine unnormalisierten Referenzen")


if __name__ == "__main__":
    unittest.main()
