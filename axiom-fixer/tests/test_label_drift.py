"""#395: positions-/dominanzbasierte Folio-Messung + Label-Klassifikation.

Fixtures: tests/fixtures/fixer395/measurements/ — die Band-Messungen der
9 christine-Produktionsbücher (2026-11-25/26), anonymisiert nach Klassen:
A = konstanter Versatz, B = zwei Segmente (two-range), C = Drift-Treppe
(+1-Schritte), D = Referenzfall der Produktions-Sonde. Die Zuordnung
steht in EXPECTED; die echten Titel bleiben außen vor.

E2E und Mutations-Sonden laufen auf synthetischen PDFs (deterministisch,
kein echtes Buch wird berührt).
"""

import json
import sys
from pathlib import Path

import pytest
import pymupdf  # type: ignore[reportMissingImports]

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

from tools import label_drift, pdf_kernel  # noqa: E402  # type: ignore[reportAttributeAccessIssue]

FIX = HERE / "fixtures" / "fixer395"

# Erwartete Klassifikation je Fixture. `ranges` = exakter Soll-Baum
# (aus ALL_TREES.json der Produktion, Front-Matter-Konvention inklusive);
# `min_anchors` = Qualitäts-Tor der Produktionsevidenz.
EXPECTED = {
    "A1": {"class": "constant-offset",
           "ranges": [{"startpage": 0, "prefix": "", "style": "R", "firstpagenum": 1},
                      {"startpage": 16, "prefix": "", "style": "D", "firstpagenum": 1}]},
    "A2": {"class": "drift",
           "ranges": [{"startpage": 0, "prefix": "", "style": "R", "firstpagenum": 2},
                      {"startpage": 16, "prefix": "", "style": "D", "firstpagenum": 1},
                      {"startpage": 421, "prefix": "", "style": "D", "firstpagenum": 407}]},
    "A3": {"class": "constant-offset",
           "ranges": [{"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1},
                      {"startpage": 11, "prefix": "", "style": "D", "firstpagenum": 1}]},
    "A6": {"class": "drift", "exact": False,
           # Referenzbaum (ALL_TREES): D@12 f2 + D@28 f19. Produktion
           # dokumentiert Grenzmehrdeutigkeit + ignorierten Tail. Der
           # Klassifikator heilt messegetreu: Front-Back-Extension eine
           # Seite früher (Anker-Folio − 1) und das Tail-Segment (0b61,
           # Sprung 1+1 über die unnummerierte Seite 0b60). JEDE gemessene
           # Folio stimmt mit dem emittierten Baum (siehe
           # test_interior_grenzen_exakt_und_anker_aequivalent).
           "anchor_ranges": [{"startpage": 28, "prefix": "", "style": "D", "firstpagenum": 19}]},
}
# Klasse B ist durch A2 (IHNEI6E7: zwei Segmente) und A6 (DFI2CY65)
# belegt — fixture_B1.json ist dieselbe Messung wie A2 und dient nur
# der Klassen-Namenskonvention des Korpus.
# A4/A5 (Fenster-Bücher): Interior-Grenzen exakt, Front-Konvention abweichend
EXPECTED["A4"] = {"class": "drift", "exact": False,
                  "anchor_ranges": [{"startpage": 74, "prefix": "", "style": "D", "firstpagenum": 55},
                                    {"startpage": 297, "prefix": "", "style": "D", "firstpagenum": 279},
                                    {"startpage": 332, "prefix": "", "style": "D", "firstpagenum": 315}]}
EXPECTED["A5"] = {"class": "drift", "exact": False,
                  "anchor_ranges": [{"startpage": 30, "prefix": "", "style": "D", "firstpagenum": 13},
                                    {"startpage": 71, "prefix": "", "style": "D", "firstpagenum": 55},
                                    {"startpage": 194, "prefix": "", "style": "D", "firstpagenum": 179}]}
EXPECTED["C1"] = {"class": "drift", "segments": 8,
                  "ranges": [{"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1},
                             {"startpage": 27, "prefix": "", "style": "D", "firstpagenum": 3},
                             {"startpage": 120, "prefix": "", "style": "D", "firstpagenum": 97},
                             {"startpage": 143, "prefix": "", "style": "D", "firstpagenum": 121},
                             {"startpage": 256, "prefix": "", "style": "D", "firstpagenum": 235},
                             {"startpage": 291, "prefix": "", "style": "D", "firstpagenum": 271},
                             {"startpage": 326, "prefix": "", "style": "D", "firstpagenum": 307},
                             {"startpage": 335, "prefix": "", "style": "D", "firstpagenum": 317},
                             {"startpage": 384, "prefix": "", "style": "D", "firstpagenum": 367}]}
EXPECTED["C2"] = {"class": "drift", "segments": 11,
                  "ranges": [{"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1},
                             {"startpage": 8, "prefix": "", "style": "D", "firstpagenum": 1},
                             {"startpage": 21, "prefix": "", "style": "D", "firstpagenum": 15},
                             {"startpage": 38, "prefix": "", "style": "D", "firstpagenum": 33},
                             {"startpage": 89, "prefix": "", "style": "D", "firstpagenum": 85},
                             {"startpage": 140, "prefix": "", "style": "D", "firstpagenum": 137},
                             {"startpage": 161, "prefix": "", "style": "D", "firstpagenum": 159},
                             {"startpage": 184, "prefix": "", "style": "D", "firstpagenum": 183},
                             {"startpage": 245, "prefix": "", "style": "D", "firstpagenum": 245},
                             {"startpage": 274, "prefix": "", "style": "D", "firstpagenum": 275},
                             {"startpage": 303, "prefix": "", "style": "D", "firstpagenum": 305},
                             {"startpage": 350, "prefix": "", "style": "D", "firstpagenum": 353}]}


def _load_fixture(name: str) -> tuple[dict[int, dict], int]:
    data = json.loads((FIX / "measurements" / f"fixture_{name}.json").read_text())
    meas = {int(i): m for i, m in data["bands"].items()}
    return meas, data["pages"]


def _load_fixture_pdf(path: Path) -> tuple[dict[int, dict], int]:
    live = label_drift.measure_bands(path)
    return live, pdf_kernel.doc_page_count(path)


# ------------------------------------------------------- Klassifikation --


@pytest.mark.parametrize("name", ["A1", "A2", "A3", "A4", "A5", "A6", "C1", "C2"])
def test_klassen_a_b_c_werden_heilbar_klassifiziert(name):
    """Die 8 christine-Bücher (Klassen A/B/C) klassifizieren heilbar —
    die Karte war nie chaotisch, nur die Messung war es."""
    meas, n = _load_fixture(name)
    v = label_drift.classify(meas, n)
    assert v["class"] == EXPECTED[name]["class"], v.get("reason", "")
    assert v.get("would_heal", True)


@pytest.mark.parametrize("name,expected", [("A1", EXPECTED["A1"]), ("A2", EXPECTED["A2"]),
                                           ("A3", EXPECTED["A3"]), ("C1", EXPECTED["C1"]),
                                           ("C2", EXPECTED["C2"])])
def test_referenzbaeume_werden_exakt_reproduziert(name, expected):
    """A1/A2/A3 + beide DoD-kritischen C-Bücher: der emittierte Baum ist
    EXAKT der Referenzbaum aus ALL_TREES.json (Range-Grenzen, Folio-
    Anker, römischer Vorspann mit adoptierter Startzahl/Case)."""
    meas, n = _load_fixture(name)
    v = label_drift.classify(meas, n)
    assert v["ranges"] == expected["ranges"]


@pytest.mark.parametrize("name", ["A4", "A5", "A6"])
def test_interior_grenzen_exakt_und_anker_aequivalent(name):
    """Fenster-/Grenzbücher: jede INTERIOR-Grenze exakt wie der Referenz-
    baum (Grenzlabel = next_start − 1); Front-Matter-Konvention ist ein
    offener Vorschlag — aber JEDE gemessene Folio stimmt mit dem
    emittierten Baum."""
    meas, n = _load_fixture(name)
    v = label_drift.classify(meas, n)
    assert v["class"] == "drift"
    ranges = v["ranges"]
    for want in EXPECTED[name]["anchor_ranges"]:
        assert want in ranges, f"{name}: Interior-Grenze fehlt: {want}"
    labels = label_drift.labels_from_ranges(ranges, n)
    for i, m in meas.items():
        if not m["digits"]:
            continue
        assert labels[i] in {str(d) for d in m["digits"]}, (
            f"{name} Seite {i}: Label {labels[i]!r} gegen {m['digits']}"
        )


def test_klasse_d_wird_zur_neuen_messung_gesund_geheilt():
    """Produktionsfall D1 (Tourismuspsychologie-Referenz): Der Auftrag
    meldete 'gar kein messbares Kopf/Fuß-Folio' — das war ein Artefakt
    der zeilenbasierten Sonde. Die Wort-Ebene misst 374 saubere Anker
    (Delta 0, folio == Seite), der Label-Tree FEHLT komplett. Die
    ehrliche Klassifikation ist daher heilbar, nicht 'no healable
    defect' — Abweichung vom Auftrag dokumentiert (Owner-Entscheid
    möglich: ein Gate 'Tree fehlt → nie #395' stellt den alten Verdict
    wieder her, verliert aber dieselbe Heilqualität wie A–C)."""
    meas, n = _load_fixture("D1")
    v = label_drift.classify(meas, n)
    assert v["class"] == "constant-offset"
    assert v["anchors"] >= 300
    labels = label_drift.labels_from_ranges(v["ranges"], n)
    hits = sum(1 for i, m in meas.items() if m["digits"] and labels[i] in {str(d) for d in m["digits"]})
    assert hits >= 300


# ------------------------------------------------- Mutations-Sonden ------


def test_mutation_a_jahreszahlen_drehen_die_klasse_nicht():
    """(a) Jahre in die Kopfzeile eines A-Fixtures injizieren: Dominanz
    trägt weiter — die Klasse bleibt constant-offset, der Baum identisch."""
    meas, n = _load_fixture("A1")
    # Jede vierte Seite bekommt ein Jahr als zusätzliches Band-Token
    for i in range(0, n, 4):
        meas.setdefault(i, {"digits": [], "roman": []})
        meas[i]["digits"] = sorted(set(meas[i]["digits"]) | {2013})
    v = label_drift.classify(meas, n)
    assert v["class"] == "constant-offset"
    assert v["ranges"] == EXPECTED["A1"]["ranges"]


def test_mutation_b_ohne_bandfilter_liefert_falschen_baum():
    """(b) Alter Modus: Jedes Zahlen-Token der GANZEN Seite ist Kandidat.
    Eine kohärente Fließtext-Familie (z. B. die physische Seitenzahl im
    Body) überstimmt dann das echte Folio-Signal — der Klassifikator
    heilt FALSCH. Mit Positionsfilter (±9 %-Band) bleibt der Baum exakt:
    die Positionsfilterung ist der wirksame Teil der Messung."""
    meas, n = _load_fixture("A1")
    # ALTER MODUS: die physische Seitenzahl steht (als Fließtext-Zahl)
    # auf JEDER Seite im Kandidatensatz — Familie Delta +1 mit n Seiten
    # überstimmt das echte Folio-Signal (A1: 447 Anker auf Delta −15).
    unfiltered: dict[int, dict] = {}
    for i in range(n):
        digits = set(meas.get(i, {}).get("digits", [])) | {i + 1}
        unfiltered[i] = {"digits": sorted(digits), "roman": []}
    v = label_drift.classify(unfiltered, n)
    assert v["class"] in ("constant-offset", "drift"), v
    assert v["ranges"] != EXPECTED["A1"]["ranges"], (
        "ohne Positionsfilter müsste die Fließtext-Familie den Baum drehen"
    )
    # Mit Positionsfilter (die Fixture IST die Band-Messung): exakter Baum.
    v2 = label_drift.classify(meas, n)
    assert v2["ranges"] == EXPECTED["A1"]["ranges"]


# ----------------------------------------------------- Verweigerung -----


def _drift_pdf(path: Path, dropped: int) -> Path:
    """Synthetisches Drift-Buch: an den unnummerierten Grenzseiten (10, 20)
    konsumiert die Print-Nummerierung die Zahl der Grenzseite SELBST plus
    `dropped` zusätzliche Nummern (gedroppte Beilage). dropped=1 → echte
    +1-Drift-Treppe; dropped=2 → Schritt +2 → Verweigerung.
    Folgen: p0..9 = 1..10 (Delta +1); dropped=1: p11 = 13 (Delta +2),
    p21 = 24 (Delta +3)."""
    doc = pymupdf.open()
    W, H = pymupdf.paper_size("a4")
    folio = 1
    for i in range(30):
        pg = doc.new_page(width=W, height=H)
        if i in (10, 20):
            folio += dropped  # Grenzseite konsumiert ihre Zahl still
        if i in (10, 20):
            pass  # unnummerierter Opener
        else:
            pg.insert_text((300, 30), str(folio), fontsize=12)
        folio += 1
        pg.insert_text((40, 150), f"Kapiteltext {i} " + "lorem ipsum " * 10, fontsize=11)
    doc.save(path)
    doc.close()
    return path


def test_drift_treppe_mit_schritt_1_heilt():
    p = _drift_pdf(FIX / "_drift_ok.pdf", dropped=1)
    try:
        meas, n = _load_fixture_pdf(p)
        v = label_drift.classify(meas, n)
        assert v["class"] == "drift"
        assert v["segments"] == 3
        r = v["ranges"]
        # Grenzlabel = next_start − 1: unnummerierte Grenzseite gehört zur
        # Folgerange und trägt deren erste Zahl minus eins.
        assert [x["firstpagenum"] for x in r if x["style"] == "D"] == [1, 12, 23]
    finally:
        p.unlink(missing_ok=True)


def test_schritt_2_an_grenze_wird_verweigert():
    """'Sprung 1+2' (zwei Nummern über eine Lückenseite): kein +1-Stufen-
    profil — konsistenter Restlauf → REFUSE, Owner-Anker statt Ratebasis."""
    p = _drift_pdf(FIX / "_drift_bad.pdf", dropped=2)
    try:
        meas, n = _load_fixture_pdf(p)
        v = label_drift.classify(meas, n)
        assert v["class"] == "refused", v
        assert v["reason"]  # ehrlicher Park-Grund (Rausch- oder Restlauf-Gate)
    finally:
        p.unlink(missing_ok=True)


def test_ohne_ziffer_signal_keine_heilung():
    """Kein Ziffer-Token im Band → Klasse 'none' (fällt an den Agenten,
    ehrlicher No-Op wie im Produktionsverdict-Pfad)."""
    doc = pymupdf.open()
    W, H = pymupdf.paper_size("a4")
    for i in range(6):
        pg = doc.new_page(width=W, height=H)
        pg.insert_text((40, 200), f"Prosa ohne Zahlen {i}", fontsize=11)
    p = FIX / "_nosignal.pdf"
    doc.save(p)
    doc.close()
    try:
        meas, n = _load_fixture_pdf(p)
        v = label_drift.classify(meas, n)
        assert v["class"] == "none"
    finally:
        p.unlink(missing_ok=True)


def test_labels_bereits_korrekt_kein_heal():
    """would_heal False, wenn jedes gemessene Folio bereits dem Label
    entspricht (No-Defect-Beweis aus der Messung, nicht aus Konvention)."""
    p = _drift_pdf(FIX / "_correct.pdf", dropped=0)
    try:
        # Labels exakt auf die Messung setzen: delta 0 …
        meas, n = _load_fixture_pdf(p)
        v = label_drift.classify(meas, n)
        assert v["class"] in ("constant-offset", "drift")
        doc = pymupdf.open(p)
        doc.set_page_labels([{"startpage": 0, "prefix": "", "style": "D",
                              "firstpagenum": v["ranges"][-1]["firstpagenum"]
                              - v["ranges"][-1]["startpage"]}])
        doc.saveIncr()
        doc.close()
        wh = label_drift.would_heal(p)
        assert wh["would_heal"] is False
        assert "already" in wh["reason"]
    finally:
        p.unlink(missing_ok=True)


# --------------------------------------------------- E2E Katalog-Heil --


def _cfg(tmp_path):
    from config import Config

    return Config(
        zotero_storage_root=tmp_path / "storage",
        rag_api_base="http://127.0.0.1:9",
        database_url="",
        deepseek_api_key="",
        deepseek_base_url="",
        model="",
        backup_root=tmp_path / "backups",
        work_root=tmp_path / "runs",
        budget_max_ops=10,
        budget_max_seconds=0,
        lang_profiles=["de"],
        probe_write=False,
        provenance={},
    )


@pytest.mark.parametrize("dropped,cls,segs", [(0, "constant-offset", 1), (1, "drift", 3)])
def test_e2e_katalogregel_heilt_ohne_modell_mit_readback(tmp_path, dropped, cls, segs):
    """E2E über run_agent: ein Klassen-A/C-Buch läuft ohne Modell durch
    Messung → Klassifikation → write_label_ranges → Readback-Beweis.
    Der Storage-Original bleibt byte-identisch (Arbeitskopie-Disziplin)."""
    import repair_agent as ra

    cfg = _cfg(tmp_path)
    key = f"E2E{dropped}{cls[:2].upper()}"
    att = cfg.zotero_storage_root / key
    att.mkdir(parents=True)
    p = _drift_pdf(att / "book.pdf", dropped=dropped)
    vorher = p.read_bytes()
    rep = ra.run_agent(key, apply=True, client=None, cfg=cfg)
    assert rep["verdict"] == "healed", json.dumps(rep, default=str)[:500]
    assert rep["final_step"]["plan_class"] == cls
    assert rep["heal_readback"]["tree"] is True
    work = cfg.work_root / key / "work.pdf"
    labels = pdf_kernel.read_page_labels(work)
    # Jede gedruckte Folio stimmt nach der Heilung mit dem Label überein
    meas, _ = _load_fixture_pdf(p)
    for i, m in meas.items():
        if m["digits"]:
            assert labels[i] in {str(d) for d in m["digits"]}, (i, labels[i], m["digits"])
    # Arbeitskopie-Disziplin: Storage-Original byte-identisch
    assert p.read_bytes() == vorher
    spur = json.loads((cfg.work_root / key / "report.json").read_text())
    assert spur["catalog_rule"].endswith("(#395)")


def test_e2e_refuse_faellt_an_agenten_zurueck(tmp_path):
    """Ein widersprüchliches Buch (Schritt 2) parkt die Katalog-Regel
    bewusst (None) — der Agentenlauf übernimmt (ohne Modell: NO-MODEL),
    kein Byte wird geschrieben."""
    import repair_agent as ra

    cfg = _cfg(tmp_path)
    key = "E2EREFUSE"
    att = cfg.zotero_storage_root / key
    att.mkdir(parents=True)
    _drift_pdf(att / "book.pdf", dropped=2)
    vorher = (att / "book.pdf").read_bytes()
    rep = ra.run_agent(key, apply=True, client=None, cfg=cfg)
    assert rep["verdict"] == "NO-MODEL"
    # kein Heilversuch: Storage-Original byte-identisch, kein
    # heal_readback-Beweis im Bericht (der Katalog hat bewusst geparkt).
    assert (att / "book.pdf").read_bytes() == vorher
    assert "heal_readback" not in rep
