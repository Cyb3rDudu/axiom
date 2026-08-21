r"""Runner-Härtung Z2: Hyperlink-Fehlklassifikation als Bildref.

Beweislage (Satellit 2026-08-21, Job 81d39c05, 3 Kollisionen): die
Ligatur-Fehllesung 'ft'->'!' macht aus Inline-Links Markdown-Bild-Syntax —
'Wirtscha!spsychologie aktuell (http://www.wirtscha![spsychologie-
aktuell.de\)](http://www.wirtschaftspsychologie-aktuell.de)' — und der
Chunker-Regex übernimmt die externe URL als image_ref. Der Persist-Gate
blockiert zu Recht (CHUNK_IMAGE_REF_UNRESOLVED), das Buch stirbt an der
Klassifikation, nicht am Gate.

Fix-Klasse: http(s)-Refs sind LINKS, keine Artefakt-Refs — sie fallen an
der Contract-Grenze aus den image_refs; lokale Refs bleiben strikt ge-gated.
"""
import unittest

from axiom_ng_runner.runner import _drop_link_refs

WIRTSCHA = ("- y Wirtscha!spsychologie aktuell (http://www.wirtscha![spsychologie-aktuell.de\\)]"
            "(http://www.wirtschaftspsychologie-aktuell.de),")
WELT = (
    "**Krenz, H. P.** (2016): Wann Arbeitnehmern eine Vertragsstrafe droht. In: Die Weilt, Teil "
    "Wirtscha!/Karriere, v. 27.04.2016. Online: https://www.welt.de/wirtscha![/karriere](https://www.welt.de/karriere),")
FAZ = (
    "**o.V.** (2008): Fachkrä!emangel kann 4,6 Billionen Euro kosten. In: Frankfurter Allgemeine "
    "Zeitung (FAZ) v. 8.10.2008, Nr. 235, S. 13. Online: ![emangel](http://example.com/faz.jpg),"
    " abgerufen.")


class LinkRefClassificationTests(unittest.TestCase):
    def test_real_collisions_drop_urls(self):
        """Die drei echten Kollisionen: externe URLs raus, nichts Übriges verfälscht."""
        from axiom_ng_runner.compute_core.chunker import Chunker
        for name, line in (("wirtscha", WIRTSCHA), ("welt", WELT), ("faz", FAZ)):
            ch = Chunker(max_chunk_tokens=1200)
            chunks = ch.chunk(line, doc_metadata={"doc_id": "d"})
            meta = chunks[0]["metadata"]
            refs = _drop_link_refs(meta.get("image_refs", []))
            for r in refs:
                self.assertFalse(
                    str(r).startswith(("http://", "https://")),
                    f"{name}: externer Link als image_ref übrig: {r!r}")

    def test_local_refs_survive(self):
        """Lokale Marker-Bildrefs (media/…, Bildnamen) bleiben — das Gate bleibt strikt."""
        refs = ["_page_6_Figure_5.jpeg", "media/cover.png"]
        self.assertEqual(_drop_link_refs(refs), refs)

    def test_http_and_https_both_drop(self):
        self.assertEqual(_drop_link_refs(["http://a.de/x.png", "https://b.de/y.png", "img_0001.png"]),
                         ["img_0001.png"])


if __name__ == "__main__":
    unittest.main()
