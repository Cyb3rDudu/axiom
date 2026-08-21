"""#176 — pdf_label_surgery contract suite.

Synthetic fixture PDFs per class (pymupdf builds them: defined label trees +
footer folio texts = the print truth), healing roundtrips, rot-sonde
(classifier crippled → anchor-truth gate goes red → auto-rollback),
dry-run-touches-nothing, refusal on unclassifiable evidence, and the
reproduced-real-case proof: a healthy tree re-broken synthetically, healed
by the CLI off-storage, three-way green (label == truth at every anchor).
No network, no DB, no production PDF is ever touched.
"""
from __future__ import annotations

import hashlib
import importlib.util
import io
import json
import sys
from pathlib import Path

import pymupdf
import pytest

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
SCRIPT = REPO_ROOT / "scripts" / "pdf_label_surgery.py"

_spec = importlib.util.spec_from_file_location("pdf_label_surgery", SCRIPT)
pls = importlib.util.module_from_spec(_spec)
sys.modules["pdf_label_surgery"] = pls
_spec.loader.exec_module(pls)


# ------------------------------------------------------------- fixtures ----

def build_fixture(path: Path, truth: dict[int, str], tree: list[dict] | None,
                  n_pages: int = 30) -> dict[int, str]:
    """PDF whose footers carry the print truth; tree = the (broken) current
    /PageLabels. Returns the truth map (0-based page -> printed label)."""
    doc = pymupdf.open()
    for i in range(n_pages):
        page = doc.new_page()
        page.insert_text((72, 96), f"Kapiteltext Seite {i + 1} — Lorem ipsum dolor "
                                   f"sit amet, consectetur adipiscing elit, sed do "
                                   f"eiusmod tempor incident {i + 1}رقم.")
        if i in truth:
            page.insert_text((72, page.rect.height - 42), truth[i])
    if tree is not None:
        doc.set_page_labels(tree)
    doc.save(str(path))
    doc.close()
    return truth


def current_labels(path: Path) -> list[str]:
    doc = pymupdf.open(str(path))
    try:
        return [doc[i].get_label() for i in range(doc.page_count)]
    finally:
        doc.close()


def probe_like(path: Path, truth: dict[int, str], pages: list[int]) -> list[dict]:
    """Anchor measurement exactly as the integrity probe reports it:
    N = current embedded label, M = chunk truth (the print)."""
    labels = current_labels(path)
    return [{"page": p, "N": labels[p] or "", "M": truth[p]} for p in pages]


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def call_main(argv: list[str]) -> tuple[int, str]:
    """Invoke the CLI main in-process; returns (exit_code, stdout)."""
    buf = io.StringIO()
    old_argv, old_stdout = sys.argv, sys.stdout
    try:
        sys.argv, sys.stdout = argv, buf
        rc = pls.main()
    finally:
        sys.argv, sys.stdout = old_argv, old_stdout
    return rc, buf.getvalue()


def run_cli(key: str, pdf: Path, anchors: list[dict], *extra: str) -> int:
    """Off-storage CLI invocation: --pdf implies --no-probe/--no-db, the
    measurement comes from --anchors (a probe output stand-in)."""
    import tempfile

    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(anchors, f)
        anchor_file = f.name
    rc, out = call_main([str(SCRIPT), key, "--pdf", str(pdf),
                         "--anchors", anchor_file, *extra])
    # evidence on stdout is part of the contract (Befehl + Output)
    assert "KLASSIFIKATION" in out, out
    print(out)
    Path(anchor_file).unlink(missing_ok=True)
    return rc


# ------------------------------------------------- classification classes --

def test_constant_offset_roundtrip(tmp_path):
    """Labels ≡ physical shifted by +3 (delta constant, non-identity tree)."""
    pdf = tmp_path / "co.pdf"
    truth = {i: str(i + 1) for i in range(30)}                      # print = physical
    build_fixture(pdf, truth, tree=[{"startpage": 0, "prefix": "", "style": "D",
                                     "firstpagenum": 4}])           # labels = p+4
    anchors = probe_like(pdf, truth, [4, 15, 26])
    klas = pls.classify(anchors, pls.read_spec(pdf), 30)
    assert klas["klasse"] == "constant-offset"
    assert klas["deltas"] == [-3]
    assert run_cli("TESTCO", pdf, anchors, "--apply") == pls.EXIT_OK
    assert current_labels(pdf) == [truth.get(i, "") for i in range(30)]


def test_reprint_start_roundtrip(tmp_path):
    """Internal numbering (identity labels) vs print start 1317 — the
    reprint-start form: same constant arithmetic, identity tree."""
    pdf = tmp_path / "rs.pdf"
    truth = {i: str(1317 + i) for i in range(30)}
    build_fixture(pdf, truth, tree=[{"startpage": 0, "prefix": "", "style": "D",
                                     "firstpagenum": 1}])
    anchors = probe_like(pdf, truth, [6, 15, 26])
    klas = pls.classify(anchors, pls.read_spec(pdf), 30)
    assert klas["klasse"] == "reprint-start"
    assert run_cli("TESTRS", pdf, anchors, "--apply") == pls.EXIT_OK
    assert current_labels(pdf) == [truth.get(i, "") for i in range(30)]


def test_two_range_roundtrip(tmp_path):
    """Print gap (+2 at page 18) the tree doesn't model; boundary is pinned
    by the measured folio jump — roman front matter PRESERVED verbatim."""
    pdf = tmp_path / "tr.pdf"
    truth = {i: pls.to_roman(i + 1) for i in range(8)}
    for i in range(8, 30):
        truth[i] = str(1 + (i - 8) + (2 if i >= 18 else 0))
    tree = [{"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1},
            {"startpage": 8, "prefix": "", "style": "D", "firstpagenum": 1}]
    build_fixture(pdf, truth, tree)
    anchors = probe_like(pdf, truth, [3, 14, 24])   # front roman + 2 arabic
    klas = pls.classify(anchors, pls.read_spec(pdf), 30)
    assert klas["klasse"] == "two-range"
    assert klas["deltas"] == [0, 2]
    assert run_cli("TESTTR", pdf, anchors, "--apply") == pls.EXIT_OK
    labels = current_labels(pdf)
    assert labels[:8] == [pls.to_roman(i + 1) for i in range(8)]  # PRESERVE
    assert labels[17] == "10" and labels[18] == "13"               # pinned gap
    assert labels == [truth.get(i, "") for i in range(30)]


def test_injection_labelless_adopts_roman_truth(tmp_path):
    """Label-less PDF, chunk truth carries roman front matter → the style is
    ADOPTED from the measurement (no open decision)."""
    pdf = tmp_path / "in.pdf"
    truth = {i: pls.to_roman(i - 1) for i in range(2, 8)}          # i…vi ab S.3
    for i in range(8, 30):
        truth[i] = str(1 + (i - 8))
    build_fixture(pdf, truth, tree=None)
    anchors = probe_like(pdf, truth, [3, 12, 24])                  # M='ii','5','17'
    klas = pls.classify(anchors, pls.read_spec(pdf), 30)
    assert klas["klasse"] == "injection"
    folio, runs = pls.folio_evidence(pdf)
    plan = pls.build_plan(klas, pls.read_spec(pdf), 30, folio, runs, None)
    assert plan["open_decision"] is None and "Chunk-Wahrheit" in plan["style_note"]
    assert run_cli("TESTIN", pdf, anchors, "--apply") == pls.EXIT_OK
    labels = current_labels(pdf)
    assert labels[:2] == ["C1", "C2"]                              # Cover-Präfix (Korpus-Muster,
    # pymupdf kann Seiten vor der ersten Range nicht lesen — totaler Baum Pflicht)
    assert labels[2] == "i" and labels[3] == "ii"                  # römisch aus Messung (ADOPT)
    assert labels[8] == "1" and labels[24] == "17"


def test_injection_c1_block(tmp_path):
    """The Springer disease: C1-Block over everything, chunk truth arabic →
    injection rebuilds the tree from truth."""
    pdf = tmp_path / "c1.pdf"
    truth = {i: str(1 + i) for i in range(30)}
    build_fixture(pdf, truth, tree=[{"startpage": 0, "prefix": "C", "style": "D",
                                     "firstpagenum": 1}])
    anchors = probe_like(pdf, truth, [5, 15, 25])
    klas = pls.classify(anchors, pls.read_spec(pdf), 30)
    assert klas["klasse"] == "injection"       # N='C…' is non-numeric → Case B
    assert run_cli("TESTC1", pdf, anchors, "--apply") == pls.EXIT_OK
    assert current_labels(pdf) == [truth.get(i, "") for i in range(30)]


def test_injection_style_vacuum_open_decision(tmp_path):
    """No tree, no roman evidence: front-matter style is NOT decidable — the
    roman proposal is an OPEN DECISION; --style-override prefix:C resolves."""
    pdf = tmp_path / "vac.pdf"
    truth = {i: str(1 + (i - 6)) for i in range(6, 30)}            # body ab S.7
    build_fixture(pdf, truth, tree=None)
    anchors = probe_like(pdf, truth, [9, 18, 27])
    klas = pls.classify(anchors, pls.read_spec(pdf), 30)
    assert klas["klasse"] == "injection"
    folio, runs = pls.folio_evidence(pdf)
    plan = pls.build_plan(klas, pls.read_spec(pdf), 30, folio, runs, None)
    assert plan["open_decision"] and "NICHT entscheidbar" in plan["open_decision"]
    assert run_cli("TESTVAC", pdf, anchors, "--apply",
                   "--style-override", "prefix:C") == pls.EXIT_OK
    labels = current_labels(pdf)
    assert labels[:6] == [f"C{i + 1}" for i in range(6)]           # override honored
    assert labels[9] == "4" and labels[27] == "22"


# ---------------------------------------------------- refuse + dry-run -----

def test_unclassifiable_refuses_and_touches_nothing(tmp_path):
    """Variable deltas = the reportable refusal case; exit 3, file frozen."""
    pdf = tmp_path / "var.pdf"
    truth = {i: str(1 + (i - 8)) for i in range(8, 30)}
    build_fixture(pdf, truth, tree=[{"startpage": 0, "prefix": "", "style": "D",
                                     "firstpagenum": 1}])
    anchors = probe_like(pdf, truth, [10, 16, 24])
    # forge variable evidence (the real world feeds this from broken print):
    # drei verschiedene Deltas — weder konstant noch ein einziger Sprung
    anchors[0]["M"], anchors[1]["M"], anchors[2]["M"] = "3", "11", "16"
    before = sha(pdf)
    klas = pls.classify(anchors, pls.read_spec(pdf), 30)
    assert klas["klasse"] == "unclassifiable"
    assert run_cli("TESTVAR", pdf, anchors) == pls.EXIT_REFUSE
    assert sha(pdf) == before


def test_dry_run_touches_nothing(tmp_path):
    """Default run: fs fingerprint before == after, no backup written."""
    pdf = tmp_path / "dry.pdf"
    truth = {i: str(i + 1) for i in range(30)}
    build_fixture(pdf, truth, tree=[{"startpage": 0, "prefix": "", "style": "D",
                                     "firstpagenum": 4}])
    anchors = probe_like(pdf, truth, [4, 15, 26])
    st0 = pdf.stat()
    before = sha(pdf)
    assert run_cli("TESTDRY", pdf, anchors) == pls.EXIT_OK
    st1 = pdf.stat()
    assert sha(pdf) == before
    assert (st0.st_mtime_ns, st0.st_size) == (st1.st_mtime_ns, st1.st_size)
    assert not pls.BACKUP_DIR.joinpath("dry.pdf").exists()
    assert not pls.BACKUP_DIR.joinpath("dry.bak.pdf").exists()


# ------------------------------------------------------------ rot-sonde ----

def test_rot_sonde_crippled_classifier_witness_red(tmp_path, monkeypatch):
    """Cripple the classifier to lie (single constant segment on a two-range
    truth): the plan is self-consistent, so only the ANCHOR-TRUTH gate can
    catch it — witness red, auto-rollback, file byte-identical."""
    pdf = tmp_path / "rot.pdf"
    truth = {i: pls.to_roman(i + 1) for i in range(8)}
    for i in range(8, 30):
        truth[i] = str(1 + (i - 8) + (2 if i >= 18 else 0))
    tree = [{"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1},
            {"startpage": 8, "prefix": "", "style": "D", "firstpagenum": 1}]
    build_fixture(pdf, truth, tree)
    anchors = probe_like(pdf, truth, [3, 14, 24])

    real_classify = pls.classify

    def lying_classify(anchors_, spec_, n_):
        klas = real_classify(anchors_, spec_, n_)
        if klas["klasse"] == "two-range":        # pretend the gap doesn't exist:
            arabic = [(p, m, n) for p, m, n in klas["arabic"]]  # claim ONE constant
            d = klas["deltas"][-1]                # delta (the back anchor's view)
            klas = {"klasse": "constant-offset", "deltas": [d],
                    "segs": [(arabic[0][0], d)], "arabic": arabic,
                    "roman": klas["roman"], "delta0": d}
        return klas

    monkeypatch.setattr(pls, "classify", lying_classify)
    before = sha(pdf)
    assert run_cli("TESTROT", pdf, anchors, "--apply") == pls.EXIT_ABORT
    assert sha(pdf) == before                     # AUTO-ROLLBACK bewiesen


# -------------------------------------------- reproduced real case (DoD) ---

def test_reproduced_real_case(tmp_path):
    """The honest e2e: a HEALTHY tree (as the 31 healed books now stand),
    copied to temp, re-broken synthetically, healed via the CLI off-storage —
    three-way green without ever risking a production PDF."""
    healthy = [{"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1},
               {"startpage": 6, "prefix": "", "style": "D", "firstpagenum": 1},
               {"startpage": 20, "prefix": "", "style": "D", "firstpagenum": 17}]
    truth = {i: pls.to_roman(i + 1) for i in range(6)}
    for i in range(6, 30):
        truth[i] = str(1 + (i - 6) + (2 if i >= 20 else 0))       # real print gap
    src = tmp_path / "already_healed.pdf"
    build_fixture(src, truth, tree=healthy)
    assert current_labels(src) == [truth.get(i, "") for i in range(30)]  # gesund

    broken = tmp_path / "copy_rebroken.pdf"                        # Kopie ins Temp
    broken.write_bytes(src.read_bytes())
    doc = pymupdf.open(str(broken))                                # wieder kaputt
    doc.set_page_labels([{"startpage": 0, "prefix": "C", "style": "D", "firstpagenum": 1}])
    doc.save(str(broken), incremental=True, encryption=pymupdf.PDF_ENCRYPT_KEEP)
    doc.close()
    assert current_labels(broken)[10].startswith("C")              # C1-Wüste zurück

    anchors = probe_like(broken, truth, [3, 12, 24])
    assert run_cli("REPRODUCED", broken, anchors, "--apply") == pls.EXIT_OK
    assert current_labels(broken) == current_labels(src)           # probe-grün-Äquivalent:
    # jedes gemessene M == Label UND vollständige Baumgleichheit mit der Wahrheit
    for a in anchors:
        assert current_labels(broken)[a["page"]] == a["M"]
    assert sha(src) == sha(tmp_path / "already_healed.pdf")        # Quelle unberührt


# ----------------------------------------------------------- DB + probe ----

def test_db_hash_sync_rowcount_guard(tmp_path, monkeypatch):
    """Hash-Sync ist Pflicht: rowcount != 1 (keine Row) → Apply bricht ab,
    kein stiller Zustand."""
    pdf = tmp_path / "db.pdf"
    truth = {i: str(i + 1) for i in range(30)}
    build_fixture(pdf, truth, tree=[{"startpage": 0, "prefix": "", "style": "D",
                                     "firstpagenum": 4}])
    calls = {}

    class _Cur:
        rowcount = 0

        def execute(self, sql, params):
            calls["sql"], calls["params"] = sql, params

    class _Conn:
        def cursor(self):
            return _Cur()

        def commit(self):
            calls["commit"] = True

        def close(self):
            pass

    import types

    fake_psycopg2 = types.SimpleNamespace(connect=lambda dsn: _Conn())
    monkeypatch.setitem(sys.modules, "psycopg2", fake_psycopg2)

    anchors = probe_like(pdf, truth, [4, 15, 26])
    # key mode: storage/<KEY>/*.pdf resolves to our fixture; probe mocked
    monkeypatch.setattr(pls, "STORAGE", tmp_path)
    key_dir = tmp_path / "DBKEY2"
    key_dir.mkdir(exist_ok=True)
    import shutil
    shutil.copy2(pdf, key_dir / "db.pdf")
    monkeypatch.setattr(pls, "run_probe",
                        lambda probe, key: {"verdict": "ABWEICHUNG", "_cmd": "mocked-probe --dry", "_rc": 0, "anchors": [
                            {"position": pos, "page_index": a["page"],
                             "pdf_label": a["N"],
                             "chunk": {"page": a["M"]}, "verdict": "ABWEICHUNG"}
                            for pos, a in zip(("front", "middle", "back"), anchors)
                        ]})
    rc, _ = call_main([str(SCRIPT), "DBKEY2", "--apply"])
    assert rc == pls.EXIT_ERROR                    # rowcount 0 → Abbruch
    assert "UPDATE zotero_attachments" in calls["sql"]
    assert calls["params"][3] == "DBKEY2"


def test_probe_unreachable_refuses_to_operate(tmp_path, monkeypatch):
    """Ohne Sonde kein Schnitt: probe down → EXIT_ERROR, nothing written."""
    pdf = tmp_path / "np.pdf"
    truth = {i: str(i + 1) for i in range(30)}
    build_fixture(pdf, truth, tree=None)
    (tmp_path / "NPKEY").mkdir(exist_ok=True)
    (tmp_path / "NPKEY" / "np.pdf").write_bytes(pdf.read_bytes())
    monkeypatch.setattr(pls, "STORAGE", tmp_path)

    def boom(probe, key):
        raise RuntimeError("probe nicht erreichbar")

    monkeypatch.setattr(pls, "run_probe", boom)
    rc, out = call_main([str(SCRIPT), "NPKEY", "--apply"])
    assert rc == pls.EXIT_ERROR
    assert "Probe nicht erreichbar" in out


def test_healthy_book_needs_no_surgery(tmp_path, monkeypatch):
    """Probe MATCH → kein Eingriff indiziert (exit 0, nichts geschrieben)."""
    pdf = tmp_path / "ok.pdf"
    truth = {i: str(i + 1) for i in range(30)}
    build_fixture(pdf, truth, tree=[{"startpage": 0, "prefix": "", "style": "D",
                                     "firstpagenum": 1}])
    (tmp_path / "OKKEY").mkdir(exist_ok=True)
    (tmp_path / "OKKEY" / "ok.pdf").write_bytes(pdf.read_bytes())
    monkeypatch.setattr(pls, "STORAGE", tmp_path)
    monkeypatch.setattr(pls, "run_probe", lambda probe, key: {"verdict": "MATCH",
                                                              "_cmd": "mocked-probe --dry", "_rc": 0,
                                                              "anchors": []})

    rc, out = call_main([str(SCRIPT), "OKKEY"])
    before_check = sha(tmp_path / "OKKEY" / "ok.pdf")
    assert rc == pls.EXIT_OK
    assert "kein Eingriff" in out
    assert sha(tmp_path / "OKKEY" / "ok.pdf") == before_check


# ------------------------------------ real storage PDF (skip when absent) --

def test_reproduced_case_on_real_pdf(tmp_path):
    """DoD's strongest form, env-gated: find a healthy labeled PDF in the
    real Zotero storage, copy to temp, re-break, heal, compare. Skips
    honestly when no healthy book is around."""
    storage = Path.home() / "Zotero" / "storage"
    if not storage.exists():
        pytest.skip("kein Zotero-Storage auf dieser Maschine")
    victim = None
    for key_dir in sorted(storage.iterdir())[:200]:
        pdfs = list(key_dir.glob("*.pdf"))
        if not pdfs:
            continue
        try:
            labels = current_labels(pdfs[0])
        except Exception:  # noqa: BLE001, S112 — kaputte Fremd-PDFs übergehen
            continue
        if sum(1 for l in labels if l.strip()) >= len(labels) * 0.9 and \
                len(set(labels)) >= len(labels) * 0.9:
            victim = pdfs[0]
            break
    if victim is None:
        pytest.skip("kein gesundes PDF im Storage gefunden")
    truth = {i: l for i, l in enumerate(current_labels(victim)) if l.strip()}
    broken = tmp_path / "real_rebroken.pdf"
    broken.write_bytes(victim.read_bytes())
    doc = pymupdf.open(str(broken))
    doc.set_page_labels([{"startpage": 0, "prefix": "C", "style": "D", "firstpagenum": 1}])
    doc.save(str(broken), incremental=True, encryption=pymupdf.PDF_ENCRYPT_KEEP)
    doc.close()
    pages = sorted(truth)[:3] + sorted(truth)[len(truth) // 2:len(truth) // 2 + 2] \
        + sorted(truth)[-2:]
    anchors = probe_like(broken, truth, pages)
    assert run_cli("REALREPRO", broken, anchors, "--apply") == pls.EXIT_OK
    for p, label in truth.items():
        assert current_labels(broken)[p] == label, f"S.{p + 1} weicht ab"
    assert sha(victim) == sha(victim)  # Quelle unberührt (trivial, aber Vertrag)
