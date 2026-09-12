"""#268 figure-caption matcher: full-axis positive/negative matrix, mutation
security, and the checked-in inventory coverage gate.

Kept in a TORCH-FREE module on purpose: test_image_captions.py skips its
whole module when torch is absent (the CI light stack has no heavy deps),
which would leave this regression gate unexecuted in CI. The matcher imports
no heavy dependency.
"""

from __future__ import annotations

import pytest

# ── #268: full-axis caption matcher rewrite ────────────────────────────────
#
# Inventory over the production corpus (103,420 chunks; 18,883 with
# image_refs) exposed axes beyond the lead-word list: decoration anchors/
# hashes/table cells, range and roman ordinals, abbreviations without a
# space, alnum-prefixed ordinals — and 428-class prose false positives the
# old pattern actively paired. Each axis below carries one real (anonymized)
# excerpt; the negatives pin the prose verb class, link anchors and TOC
# lines. The mutation tests remove one axis at a time from the module and
# prove its fixture turns red (mutation security per the DoD).


def _captions_of(line: str, ref: str = "image-0001") -> dict[str, str]:
    """Run the real extractor over a single-line chunk and return the
    paired caption map (or {} when nothing was recognized)."""
    from axiom_ng_runner.runner import _extract_figure_captions

    chunk = {"text": line, "metadata": {"image_refs": [ref]}}
    _extract_figure_captions([chunk])
    return chunk["metadata"].get("figure_captions", {})


@pytest.mark.parametrize(
    "line, expected",
    [
        # decoration anchor — <span id="page-7-1"></span>Figure 7: …
        ('<span id="page-7-1"></span>Figure 7: Lifecycle of a commit', "Figure 7: Lifecycle of a commit"),
        # decoration hash — #### Exhibit 14.11 / # Exhibit 1.3
        ("#### Exhibit 14.11 Borrowers, Lenders, and Social Capital", "Exhibit 14.11 Borrowers, Lenders, and Social Capital"),
        # decoration table cell — | Fig. 7.1 | Distribution … |
        ("| Fig. 7.1  | Distribution of 2019 policy blog posts (n=238) |", "Fig. 7.1  | Distribution of 2019 policy blog posts (n=238)"),
        # decoration emphasis — **Abb. 19.1** … balanced wrapper
        ("**Abb. 19.1** Lernkreislauf und Problemlösekreislauf", "Abb. 19.1 Lernkreislauf und Problemlösekreislauf"),
        # range ordinal — Figure 1-1. / Table 8-1. / Abb. 3-5
        ("*Figure 1-1. Data warehouse versus data lake versus Data Lakehouse*", "Figure 1-1. Data warehouse versus data lake versus Data Lakehouse"),
        ("Table 8-1. Short-term vs. long-term memory", "Table 8-1. Short-term vs. long-term memory"),
        # roman numeral — Abbildung II.10 / TABLE III / Abb. II.1.1
        ("Abbildung II.10: Effektivität und Effizienz", "Abbildung II.10: Effektivität und Effizienz"),
        ("TABLE III THE RESULTS OF THE SPEARMAN CORRELATION", "TABLE III THE RESULTS OF THE SPEARMAN CORRELATION"),
        ("| Abb. II.1.1: Effektivität und Effizienz (vgl. Wimmer/Neuberger 1998) |", "Abb. II.1.1: Effektivität und Effizienz (vgl. Wimmer/Neuberger 1998)"),
        # abbrev without space — Abb.1.10: …
        ("Abb.1.10: Abgrenzung der Geschäftsbereiche", "Abb.1.10: Abgrenzung der Geschäftsbereiche"),
        # decimal ordinal (the common case, pinned against regression)
        ("Abbildung 5.3: Kostenverlauf bei steigender Auslastung", "Abbildung 5.3: Kostenverlauf bei steigender Auslastung"),
        # alnum-prefixed ordinal — FIGURE B1.2.1 (inventory `?` bucket)
        ("FIGURE B1.2.1 Regional outlooks", "FIGURE B1.2.1 Regional outlooks"),
        # bare roman with no title suffix (still a caption)
        ("Abbildung I.2", "Abbildung I.2"),
    ],
)
def test_caption_positive_axes(line, expected):
    """Every inventory axis pairs its line as a caption."""
    caps = _captions_of(line)
    assert caps == {"image-0001": expected}, f"positive axis not matched: {line!r}"


@pytest.mark.parametrize(
    "line",
    [
        # German verb continuations (rule i/ii): direkt nach dem Ordinal
        "Abb. 4.62 zeigt den Zusammenhang zwischen Aufwand und Ertrag",
        "Abb. 2.39 veranschaulicht das Zielbild mit SAP S/4 HANA",
        "Abbildung 6 zeigt die sechs Kondratieff-Zyklen im Überblick",
        "Tabelle 5 stellt die Unterscheidungsmerkmale einander gegenüber",
        "Abbildung 24 fasst die methodischen Anforderungen grafisch zusammen",
        "Abb. 6.4 zeigt den Verlauf (Quelle: eigene Darstellung)",
        # English verb continuations
        "Figure 1 shows the layout of a typical repository",
        "Table 2 shows that in-toto takes up about 19% of the repository",
        "Fig. 4 shows the updated posteriori distributions",
        "Table 7 presents initial estimates of the relationship",
        "Figure 2 also illustrates the dynamic landscape of tags",
        # subfigure letter + verb (Fig. 1-b shows …)
        "Fig. 1-b shows a dataset with somewhat imbalanced data",
        # not line-initial → a reference inside prose, never a caption
        "As shown in Figure 3-14, the agent response is validated.",
        # ordinal as a link anchor (rule ii b)
        "Abbildung [2](#page-143-0) zeigt den BASF-Innovationsprozess",
    ],
)
def test_caption_prose_negatives(line):
    """A verb continuation (or a non-line-initial / link-anchor ordinal) is
    a prose reference and must never pair as a caption (the 428-class FP)."""
    assert _captions_of(line) == {}, f"prose paired as caption: {line!r}"


def test_caption_toc_line_negative():
    """A dot-leader TOC line (`Abb. 6.1 Titel …… 133`) is a table of
    contents entry, not a caption — the page-number leader must not turn it
    into a figure caption (DoD negative)."""
    assert _captions_of("Abb. 6.1 Aufbau der Bilanz ……… 133") == {}


def test_caption_line_after_decoration_pairs_positionally():
    """End-to-end: a decorated caption line inside a real chunk still pairs
    with ITS image, and the paired text carries the decoration removed
    while the chunk text (and thus the locator) stays verbatim."""
    from axiom_ng_runner.runner import _extract_figure_captions

    text = (
        "Intro prose.\n\n"
        "![chart](media/chart.png)\n\n"
        '<span id="page-7-1"></span>**Figure 7: Lifecycle of a commit**\n\n'
        "More prose.\n\n"
        "![tree](media/tree.png)\n\n"
        "| Fig. 7.2 | Repository object graph |\n"
    )
    chunk = {
        "text": text,
        "metadata": {"image_refs": ["image-0001", "image-0002"]},
    }
    _extract_figure_captions([chunk], {"image-0001": "chart.png", "image-0002": "tree.png"})
    assert chunk["metadata"]["figure_captions"] == {
        "image-0001": "Figure 7: Lifecycle of a commit",
        "image-0002": "Fig. 7.2 | Repository object graph",
    }
    # the caption remains part of the document text verbatim
    assert '<span id="page-7-1"></span>**Figure 7: Lifecycle of a commit**' in chunk["text"]


def test_mutation_strip_stage_required(monkeypatch):
    """Removing the decoration-strip stage turns the span/hash/cell fixtures
    red: with `_strip_caption_deco` reduced to identity, the decorated
    positive axes no longer match."""
    import axiom_ng_runner.runner as runner_mod

    monkeypatch.setattr(runner_mod, "_strip_caption_deco", lambda line: line)
    assert _captions_of('#### Exhibit 14.11 Borrowers, Lenders') == {}
    assert _captions_of('| Fig. 7.1 | Distribution |') == {}


def _rebuild_caption_re(runner_mod, monkeypatch) -> None:
    """Recompile the anchored caption regex from the module's current
    pattern pieces (used by the mutation tests after swapping one axis).
    The rebuilt regex is registered with monkeypatch so it is restored too."""
    import re as _re

    monkeypatch.setattr(
        runner_mod, "_FIGURE_CAPTION_RE",
        _re.compile(
            rf"(?i)^{runner_mod._CAPTION_LEAD}\.?\s*{runner_mod._CAPTION_ORDINAL}\b"
            rf"(?![\s\-–—]{{0,3}}(?:{runner_mod._CAPTION_CONNECTOR}\s+)?(?:{runner_mod._CAPTION_VERB})\b)"
        ),
    )


def test_mutation_ordinal_range_required(monkeypatch):
    """The range arm is load-bearing exactly where a range meets a verb:
    `Figure 1-3 zeigt` must be prose. Without the range arm the engine sees
    `Figure 1` (guard blind to `-3 zeigt`) and falsely pairs it; with the
    range arm the guard sees the full `1-3` and blocks."""
    import axiom_ng_runner.runner as runner_mod

    assert _captions_of("Figure 1-3 zeigt die Plattform") == {}
    # drop `(?:\s*[-–—]\s*\d+…)` (the range arm) from the ordinal token
    monkeypatch.setattr(
        runner_mod, "_CAPTION_ORDINAL",
        r"(?>\d+(?:[.,]\d+)*|[IVXLC]+(?:\.\d+)*|[A-Z]\d+(?:\.\d+)*)",
    )
    _rebuild_caption_re(runner_mod, monkeypatch)
    assert _captions_of("Figure 1-3 zeigt die Plattform") != {}, (
        "without the range arm the guard is blind to a range+verb prose line"
    )


def test_mutation_ordinal_roman_required(monkeypatch):
    """Removing the roman alternative turns the roman fixture red."""
    import axiom_ng_runner.runner as runner_mod

    monkeypatch.setattr(
        runner_mod, "_CAPTION_ORDINAL",
        r"(?>\d+(?:[.,]\d+)*(?:\s*[-–—]\s*\d+(?:[.,]\d+)*)?|[A-Z]\d+(?:\.\d+)*)",
    )
    _rebuild_caption_re(runner_mod, monkeypatch)
    assert _captions_of("Abbildung II.10: Effektivität und Effizienz") == {}
    assert _captions_of("Abbildung 5.3: Kostenverlauf") != {}


def test_mutation_alnum_ordinal_required(monkeypatch):
    r"""Removing the alnum-prefixed alternative (`[A-Z]\d+…`) turns the
    FIGURE B1.2.1 fixture red — that inventory shape is not reachable via
    the numeric or roman arms."""
    import axiom_ng_runner.runner as runner_mod

    monkeypatch.setattr(
        runner_mod, "_CAPTION_ORDINAL",
        r"(?>\d+(?:[.,]\d+)*(?:\s*[-–—]\s*\d+(?:[.,]\d+)*)?|[IVXLC]+(?:\.\d+)*)",
    )
    _rebuild_caption_re(runner_mod, monkeypatch)
    assert _captions_of("FIGURE B1.2.1 Regional outlooks") == {}
    assert _captions_of("Abbildung 5.3: Kostenverlauf") != {}


def test_mutation_abbrev_dot_optional_required(monkeypatch):
    """The abbreviation dot must be optional so `Abb.1.10` (no space)
    matches; forcing a space after the lead word turns that fixture red."""
    import re as _re

    import axiom_ng_runner.runner as runner_mod

    monkeypatch.setattr(
        runner_mod, "_FIGURE_CAPTION_RE",
        _re.compile(
            rf"(?i)^{runner_mod._CAPTION_LEAD}\.\s+{runner_mod._CAPTION_ORDINAL}\b"
            rf"(?![\s\-–—]{{0,3}}(?:{runner_mod._CAPTION_CONNECTOR}\s+)?(?:{runner_mod._CAPTION_VERB})\b)"
        ),
    )
    assert _captions_of("Abb.1.10: Abgrenzung der Geschäftsbereiche") == {}
    assert _captions_of("Abb. 1.10: Abgrenzung der Geschäftsbereiche") != {}


def test_mutation_prose_guard_required(monkeypatch):
    """Removing the negative prose guard re-pairs the verb continuations —
    the exact false-positive class #268 exists to purge."""
    import re as _re

    import axiom_ng_runner.runner as runner_mod

    monkeypatch.setattr(
        runner_mod, "_FIGURE_CAPTION_RE",
        _re.compile(
            rf"(?i)^{runner_mod._CAPTION_LEAD}\.?\s*{runner_mod._CAPTION_ORDINAL}\b"
        ),
    )
    assert _captions_of("Abb. 4.62 zeigt den Zusammenhang") != {}
    assert _captions_of("Figure 1 shows the layout") != {}


def test_mutation_atomic_ordinal_required(monkeypatch):
    """Without the atomic group the engine backtracks to a shorter ordinal
    and slips past the prose guard on a range (`Figure 1-3 shows`)."""
    import axiom_ng_runner.runner as runner_mod

    monkeypatch.setattr(
        runner_mod, "_CAPTION_ORDINAL",
        r"(?:\d+(?:[.,]\d+)*(?:\s*[-–—]\s*\d+(?:[.,]\d+)*)?|[IVXLC]+(?:\.\d+)*|[A-Z]\d+(?:\.\d+)*)",
    )
    _rebuild_caption_re(runner_mod, monkeypatch)
    assert _captions_of("Figure 1-3 shows the Databricks lakehouse platform") != {}, (
        "the backtracking hole must be observable without the atomic group"
    )


def test_caption_inventory_coverage_claim():
    """DoD gate: the checked-in inventory of real corpus forms must be fully
    covered (N of N captions) with zero prose false positives (0 of M).
    Reproducible standalone via scripts/caption_inventory.py."""
    import importlib.util
    from pathlib import Path

    script = Path(__file__).resolve().parent.parent / "scripts" / "caption_inventory.py"
    spec = importlib.util.spec_from_file_location("caption_inventory", script)
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    forms = mod.load()
    captions = [f for f in forms if f["label"] == "caption"]
    prose = [f for f in forms if f["label"] == "prose"]
    missed = [f for f in captions if not mod.matched(f["form"])]
    fp = [f for f in prose if mod.matched(f["form"])]
    assert not missed, f"inventoried caption forms not matched: {missed}"
    assert not fp, f"prose forms falsely paired: {fp}"
    assert len(captions) >= 25 and len(prose) >= 10


# ── #268 figcap backfill: positional reconstruction ────────────────────────


def test_reextract_reconstructs_multi_image_pairing_positionally():
    """The stored ingest ref_to_orig is not persisted; the backfill rebuilds
    it from the markdown occurrence order. A multi-image chunk whose captions
    were paired at ingest must reproduce the SAME pairing."""
    from axiom_ng_runner.runner import reextract_figure_captions

    text = (
        "Intro.\n\n"
        "![a](media/_page_36_Figure_2.jpeg)\n\n"
        "Figure 1: Lifecycle of a commit\n\n"
        "More prose.\n\n"
        "![b](media/_page_90_Figure_2.jpeg)\n\n"
        "Fig. 2: Repository object graph\n"
    )
    chunk = {
        "text": text,
        "metadata": {"image_refs": ["image-0031", "image-0034"]},
    }
    assert reextract_figure_captions(chunk) == {
        "image-0031": "Figure 1: Lifecycle of a commit",
        "image-0034": "Fig. 2: Repository object graph",
    }


def test_reextract_refuses_multi_image_count_mismatch():
    """A Z2 ligature misread leaves an http image in the text that
    _drop_link_refs removed from the refs: refs=1, occurrences=2. The
    reconstruction cannot be trusted for a multi-image chunk, so the backfill
    must refuse (None) and leave the stored captions alone — never purge a
    caption that may still be correct."""
    from axiom_ng_runner.runner import reextract_figure_captions

    text = (
        "Prose with a misread inline link ![x](http://example.com/a.png) "
        "and another ![z](http://example.com/b.png) plus the real figure.\n\n"
        "![y](media/_page_36_Figure_2.jpeg)\n\n"
        "Figure 1: Lifecycle of a commit\n"
    )
    # the two http images were dropped from the refs, but they still appear
    # in the text: 2 refs vs 3 occurrences — the mapping cannot be trusted
    chunk = {"text": text, "metadata": {"image_refs": ["image-0031", "image-0032"]}}
    assert reextract_figure_captions(chunk) is None


def test_reextract_single_image_count_mismatch_still_safe():
    """A single-image chunk keeps the ingest fallback even on a count
    mismatch: the caption belongs to the one image."""
    from axiom_ng_runner.runner import reextract_figure_captions

    text = "Prose ![x](http://e/a.png) ![y](media/page_1_Figure_0.jpeg)\n\nFigure 1: X\n"
    chunk = {"text": text, "metadata": {"image_refs": ["image-0001"]}}
    assert reextract_figure_captions(chunk) == {"image-0001": "Figure 1: X"}


def test_backfill_plan_leaves_mismatch_chunk_unchanged():
    """The engine treats the refused reconstruction as unchanged: the stored
    map is preserved and the chunk is not re-embedded."""
    from axiom_ng_runner.compute_core.figcap_backfill_cli import plan

    text = (
        "![x](http://e/a.png) ![z](http://e/b.png) "
        "![y](media/page_1_Figure_0.jpeg)\n\n"
        "Figure 1: X\n"
    )
    stored = {"image-0002": "Figure 1: X"}
    p = plan(
        [
            {
                "chunk_id": "c1",
                "text": text,
                "image_refs": ["image-0001", "image-0002"],
                "image_captions": {},
                "figure_captions": stored,
            }
        ]
    )[0]
    assert p["changed"] is False
    assert p["figure_captions"] == stored


# ── #268 additional negative classes (English verbs, span/emphasis stages) ──


@pytest.mark.parametrize(
    "line",
    [
        "Table 3 provides an overview of the failure modes",
        "Figure 5 compares the two architectures",
        "Figure 6 summarises the experimental setup",
        "Table 4 lists the evaluated models",
        "Figure 7 outlines the proposed pipeline",
    ],
)
def test_caption_english_verb_class_negative(line):
    """Common English reference verbs join the prose guard (the corpus is
    bilingual; these are the same 428-class false positives)."""
    assert _captions_of(line) == {}


def test_mutation_toc_guard_required(monkeypatch):
    """Removing the TOC dot-leader guard re-admits a contents entry as a
    caption."""
    import re as _re

    import axiom_ng_runner.runner as runner_mod

    monkeypatch.setattr(runner_mod, "_CAPTION_TOC_RE", _re.compile(r"(?!x)x"))
    assert _captions_of("Abb. 6.1 Aufbau der Bilanz ……… 133") != {}


def test_mutation_span_and_hash_strip_required(monkeypatch):
    """Removing the decoration-strip stage turns the span ANCHOR and hash
    fixtures red (the strip stage sub-cases are individually load-bearing)."""
    import axiom_ng_runner.runner as runner_mod

    monkeypatch.setattr(runner_mod, "_strip_caption_deco", lambda line: line)
    assert _captions_of('<span id="page-7-1"></span>Figure 7: Lifecycle') == {}
    assert _captions_of("#### Exhibit 14.11 Borrowers, Lenders") == {}
