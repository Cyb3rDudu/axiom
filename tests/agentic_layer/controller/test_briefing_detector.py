"""Tests for structured-briefing detection + Leitfragen extraction."""

# Prime axiom_backend's fragile import graph — see
# tests/agentic_layer/test_source_quality.py for rationale.
import api as _api_primer  # noqa: F401  # isort: skip

import pytest

from ai_researcher.agentic_layer.controller.utils.briefing_detector import (
    classify_assignment,
    detect_structured_briefing,
    extract_leitfragen,
    extract_primary_leitfrage,
)


KMU_BRIEFING = """Erstelle eine wissenschaftliche Hausarbeit-Recherche zum Thema:

**Welche Rolle nimmt China in der Weltwirtschaft ein und welche Chancen, Risiken
und Herausforderungen ergeben sich daraus für die DACH-Region?**

## Kontext
- Modul: Volkswirtschaftslehre, KMU Akademie
- Prüfungsform: Hausarbeit, 5 ECTS, 50 Punkte, Einzelarbeit
- Zielumfang: 3.000 Wörter ± 10 %

## Pflicht-Leitfragen
1. Welche außenwirtschaftstheoretischen Konzepte sind einschlägig, um Chinas Weltwirtschaftsrolle zu erklären?
2. Wie hat sich Chinas Position seit dem WTO-Beitritt 2001 verschoben?
3. Was sind Chinas strukturelle Probleme und Treiber 2024 bis 2026?
4. Welche empirisch belegbaren Chancen und Risiken ergeben sich für Deutschland?

## Quellenstrategie
- 10 bis 20 Quellen, mindestens 50 % wissenschaftlich und facheinschlägig
- APA 7, keine Wikipedia als Primärquelle
"""

CASUAL_SHORT = "Fasse mir die VWL-Bücher zusammen."

CASUAL_LONGISH = (
    "Hallo zusammen, ich wollte mal wissen welche Rolle China in der Weltwirtschaft "
    "spielt und was sich daraus für Deutschland ergibt. Kannst du mir dazu ein paar "
    "Gedanken liefern und vielleicht auf ein paar Aspekte eingehen, die besonders "
    "wichtig sind? Wäre super wenn du das etwas strukturierter aufbauen könntest."
)


# ---------------------------------------------------------------------------
# detect_structured_briefing
# ---------------------------------------------------------------------------


def test_detects_full_kmu_briefing():
    assert detect_structured_briefing(KMU_BRIEFING) is True


def test_rejects_casual_short():
    assert detect_structured_briefing(CASUAL_SHORT) is False


def test_rejects_casual_longish_without_structure():
    assert detect_structured_briefing(CASUAL_LONGISH) is False


def test_rejects_empty():
    assert detect_structured_briefing("") is False
    assert detect_structured_briefing(None) is False  # type: ignore[arg-type]


def test_detects_minimal_structured_mix():
    """Three signals: two headings, numbered list, academic-task keyword."""
    msg = """## Aufgabe
Eine Seminararbeit zum Thema Nachhaltigkeit in KMU.

## Leitfragen
1. Wie definieren KMU nachhaltige Praktiken in ihren Geschäftsprozessen?
2. Welche empirischen Studien zu Nachhaltigkeit in KMU sind verfügbar?
3. Welche regulatorischen Rahmenbedingungen existieren für KMU in DE?
""" + ("filler " * 30)
    assert detect_structured_briefing(msg) is True


def test_two_signals_not_enough():
    """Only 2 signals (headings + academic keyword) — should not trigger."""
    msg = """## Einleitung
Ich schreibe eine Hausarbeit.

## Frage
Wie funktioniert Keynesianismus?
""" + ("filler " * 30)
    assert detect_structured_briefing(msg) is False


# ---------------------------------------------------------------------------
# extract_leitfragen
# ---------------------------------------------------------------------------


def test_extracts_four_leitfragen_from_kmu():
    fragen = extract_leitfragen(KMU_BRIEFING)
    assert len(fragen) == 4
    assert "WTO-Beitritt 2001" in fragen[1]
    assert fragen[3].startswith("Welche empirisch belegbaren Chancen und Risiken")


def test_prefers_leitfragen_over_gliederung_when_both_present():
    """Regression: a briefing with both ## Pflicht-Leitfragen and ## Gliederung
    should return the Leitfragen, not the (often longer) Gliederung."""
    msg = """## Kontext
Eine Hausarbeit zum Thema X.

## Pflicht-Leitfragen
1. Welches sind die theoretischen Grundlagen des Themas X konkret?
2. Wie hat sich Phänomen X historisch entwickelt seit 2000?
3. Was sind die aktuellen Treiber und Hindernisse von X in DACH?

## Ziel-Gliederung
1. Einleitung (content_based, 250 Wörter)
2. Theoretische Grundlagen (research_based, 600 Wörter)
3. Entwicklung und Status quo (research_based, 700 Wörter)
4. Analyse der Fallstudie (research_based, 900 Wörter)
5. Kritische Diskussion und Ausblick (research_based, 400 Wörter)

## Quellenstrategie
APA 7, 10 bis 20 Quellen
""" + ("filler " * 40)

    fragen = extract_leitfragen(msg)
    # Must be the 3 Leitfragen, NOT the 5 Gliederungs-items.
    assert len(fragen) == 3
    assert "theoretischen Grundlagen" in fragen[0]
    assert "historisch entwickelt" in fragen[1]
    # The Einleitung line from the outline must NOT appear.
    assert not any("Einleitung (content_based" in f for f in fragen)


def test_falls_back_to_longest_when_no_leitfragen_header():
    msg = """## Aufgabe
Ein konkretes Forschungsprojekt zu Thema Y.

1. Was sind die zentralen Begriffe und ihre Operationalisierung?
2. Welche empirischen Studien existieren bereits zum Thema Y?
3. Welche methodischen Herausforderungen sind zu adressieren?
""" + ("filler " * 40)

    fragen = extract_leitfragen(msg)
    assert len(fragen) == 3
    assert "zentralen Begriffe" in fragen[0]


def test_extracts_empty_on_casual():
    assert extract_leitfragen(CASUAL_SHORT) == []
    assert extract_leitfragen(CASUAL_LONGISH) == []


def test_extracts_empty_on_two_items_only():
    msg = """Hallo. Ein paar Punkte:
1. Das ist der erste Punkt mit ausreichender Länge für die Heuristik.
2. Das ist der zweite Punkt mit genügend Zeichen ebenfalls dabei.
"""
    assert extract_leitfragen(msg) == []


def test_filters_out_short_items():
    """Items shorter than 30 chars are stripped; if too few remain, returns []."""
    msg = """Aufgabe.
1. kurz
2. ebenfalls kurz
3. Wirklich ein langer Leitfragen-Item mit Inhalt zum Thema.
4. Ein weiterer ausreichend langer Leitfragen-Satz zum Thema X.
5. Und ein dritter langer Leitfragen-Item als Abschluss der Liste.
"""
    fragen = extract_leitfragen(msg)
    assert len(fragen) == 3
    assert all(len(f) >= 30 for f in fragen)


# ---------------------------------------------------------------------------
# extract_primary_leitfrage (P0)
# ---------------------------------------------------------------------------


BERGTECH_BRIEFING = """Du unterst\u00fctzt mich bei der Konzeption einer wissenschaftlichen Hausarbeit.

Die Hausarbeit umfasst ungef\u00e4hr 3.000 W\u00f6rter.

## Zentrale Leitfrage

Verwende folgende Leitfrage:

\u201eWie beeinflussen die Makroumwelt, die Branchenumwelt und zentrale Anspruchsgruppen die Bergtech Maschinenbau GmbH als mittelst\u00e4ndisches Sondermaschinenbauunternehmen, wie k\u00f6nnen diese Einfl\u00fcsse systematisch analysiert werden und welche Konsequenzen ergeben sich daraus?\u201c

M\u00f6gliche Unterfragen:

1. Wie l\u00e4sst sich Bergtech als offene marktwirtschaftliche Unternehmung und soziotechnisches System einordnen?
2. \u00dcber welche Mechanismen wirken Makroumwelt, Branchenumwelt und Stakeholder auf das Unternehmen ein?
3. Mit welchen Instrumenten kann Bergtech relevante Umweltentwicklungen analysieren?
4. Welche drei bis f\u00fcnf Umweltfaktoren besitzen f\u00fcr Bergtech die h\u00f6chste strategische Relevanz?
5. Wie kann Bergtech auf diese Einfl\u00fcsse reagieren und Teile ihrer Umwelt aktiv mitgestalten?

## Empfohlene Gliederung

# 1. Einleitung
# 2. Theoretischer Bezugsrahmen
# 3. Darstellung und Analyse der Unternehmensumwelt
# 4. Umweltanalyse der Bergtech Maschinenbau GmbH
# 5. Zentrale Umwelteinfl\u00fcsse und Managementimplikationen
# 6. Fazit

## Literaturanforderungen
Verwende insgesamt 10 bis 20 Quellen. Schrey\u00f6gg und Koch.
"""


def test_primary_leitfrage_under_singular_header():
    """Regression: `## Zentrale Leitfrage` (singular) must be recognised and the
    quoted Leitfrage extracted — not the numbered sub-questions."""
    q = extract_primary_leitfrage(BERGTECH_BRIEFING)
    assert q is not None
    assert q.startswith("Wie beeinflussen die Makroumwelt")
    assert "Bergtech" in q


def test_primary_leitfrage_none_for_casual():
    assert extract_primary_leitfrage(CASUAL_SHORT) is None


def test_primary_leitfrage_ascii_quotes():
    msg = ("## Forschungsfrage\n\n" "\"Wie wirkt sich der Fachkr\u00e4ftemangel auf KMU aus?\"\n"
           + ("filler " * 40))
    q = extract_primary_leitfrage(msg)
    assert q is not None and "Fachkr\u00e4ftemangel" in q


# ---------------------------------------------------------------------------
# classify_assignment (P2)
# ---------------------------------------------------------------------------


def test_classify_complete_briefing():
    """The Bergtech-style prompt has outline + scope + deliverable → complete."""
    c = classify_assignment(BERGTECH_BRIEFING)
    assert c["specificity"] == "complete"
    assert c["briefing_style"] == "structured"
    assert c["has_outline"] is True
    assert c["has_scope"] is True
    assert c["has_deliverable"] is True
    assert c["deliverable"] == "Hausarbeit"
    assert c["primary_question"].startswith("Wie beeinflussen die Makroumwelt")
    assert len(c["questions"]) == 5  # the numbered sub-questions


def test_classify_kmu_briefing_is_structured_or_complete():
    """The KMU fixture is structured; with its scope + deliverable it is complete."""
    c = classify_assignment(KMU_BRIEFING)
    assert c["specificity"] in ("structured", "complete")
    assert c["briefing_style"] == "structured"


def test_classify_open_research():
    """A vague 'go research this' prompt must stay 'open' (no false positive)."""
    c = classify_assignment(CASUAL_LONGISH)
    assert c["specificity"] == "open"
    assert c["briefing_style"] == "open"
    assert c["primary_question"] is None


def test_classify_empty():
    c = classify_assignment("")
    assert c["specificity"] == "open"
    assert c["questions"] == []


# ---------------------------------------------------------------------------
# Finding 1: structured outline extraction (deterministic Gliederung)
# ---------------------------------------------------------------------------

from ai_researcher.agentic_layer.controller.utils.briefing_detector import (
    extract_outline,
)


NEXMACH_BRIEFING = """Du unterst\u00fctzt mich bei der Konzeption einer wissenschaftlichen Hausarbeit.
Umfang ca. 3.000 W\u00f6rter.

## Zentrale Leitfrage

Verwende folgende Leitfrage:

\u201eWie beeinflussen die Makroumwelt, die Branchenumwelt und zentrale Anspruchsgruppen die NexMach Systems GmbH und welche Konsequenzen ergeben sich daraus?\u201c

M\u00f6gliche Unterfragen:

1. Wie l\u00e4sst sich NexMach als marktwirtschaftliche Unternehmung einordnen?
2. \u00dcber welche Mechanismen wirken Makroumwelt und Branchenumwelt auf das Unternehmen ein?
3. Mit welchen Instrumenten kann NexMach relevante Umweltentwicklungen analysieren?
4. Welche drei bis f\u00fcnf Umweltfaktoren besitzen die h\u00f6chste strategische Relevanz?
5. Wie kann NexMach auf diese Einfl\u00fcsse reagieren?

## Empfohlene Gliederung

# 1. Einleitung

Umfang: ungef\u00e4hr 250 bis 300 W\u00f6rter

# 2. Theoretischer Bezugsrahmen

## 2.1 NexMach als marktwirtschaftliche Unternehmung

## 2.2 NexMach als offenes soziales System

# 3. Darstellung und Analyse der Unternehmensumwelt

## 3.1 Makroumwelt

## 3.2 Branchen- und Wettbewerbsumwelt

# 4. Umweltanalyse der NexMach Systems GmbH

# 5. Zentrale Umwelteinfl\u00fcsse und Managementimplikationen

# 6. Fazit

## Literaturanforderungen

Verwende insgesamt 10 bis 20 Quellen.
"""


def test_extract_outline_captures_all_sections_including_nested():
    """Regression (Finding 1): the production NexMach run was missing section
    3.2 (Branchen- und Wettbewerbsumwelt) and produced duplicate headings like
    '# 1. 1. Einleitung'. The structured outline must capture every section,
    keep nesting via the number, and yield number-free titles."""
    outline = extract_outline(NEXMACH_BRIEFING)
    titles = [s.title for s in outline]
    numbers = [s.number for s in outline]

    # All 8 top-level + 4 nested sections present (10 total).
    assert len(outline) >= 10
    assert "Branchen- und Wettbewerbsumwelt" in titles  # the one that was missing
    assert "Einleitung" in titles
    assert "Fazit" in titles

    # Titles are number-free (no double numbering).
    assert not any(t.startswith("1.") or t.startswith("#") for t in titles)

    # Nesting preserved through the number.
    assert "2.1" in numbers and "3.2" in numbers
    two_one = [s for s in outline if s.number == "2.1"][0]
    assert two_one.level == 2
    one = [s for s in outline if s.number == "1"][0]
    assert one.level == 1


def test_classify_complete_with_nested_outline():
    c = classify_assignment(NEXMACH_BRIEFING)
    assert c["specificity"] == "complete"
    assert c["has_outline"] is True
    assert len(c["outline"]) >= 10
    assert c["primary_question"].startswith("Wie beeinflussen")
    assert len(c["questions"]) == 5


# ---------------------------------------------------------------------------
# Finding 2: classify_assignment must not be over-broad
# ---------------------------------------------------------------------------


def test_classify_not_complete_without_real_outline():
    """Regression (Finding 2): Hausarbeit + 3.000 words + 3 numbered Leitfragen,
    but NO Gliederung region, must be 'structured' (not 'complete') and must not
    count the Leitfragen as outline sections."""
    msg = """## Aufgabenstellung
Hausarbeit f\u00fcr das Modul Organisation.

Umfang: ca. 3.000 W\u00f6rter.

## Leitfragen
1. Welche Rolle spielt die Makroumwelt f\u00fcr Unternehmen im Bereich industrieller Software?
2. Wie lassen sich Umwelteinfl\u00fcsse systematisch analysieren und priorisieren?
3. Welche strategischen Konsequenzen ergeben sich daraus f\u00fcr ein mittelst\u00e4ndisches Unternehmen?
"""
    c = classify_assignment(msg)
    assert c["specificity"] == "structured"  # NOT complete
    assert c["has_outline"] is False  # numbered Leitfragen are not outline sections
    assert c["outline"] == []
    # No primary question under a plural "## Leitfragen" header.
    assert c["primary_question"] is None


def test_primary_leitfrage_not_taken_from_plural_fragen_header():
    """Regression (Finding 2): a numbered sub-question under ## Pflicht-Leitfragen
    (plural) must NOT be returned as the primary Leitfrage."""
    msg = """Hausarbeit, ca. 3.000 W\u00f6rter.

## Pflicht-Leitfragen
1. Welche au\u00dfenwirtschaftstheoretischen Konzepte sind einschl\u00e4gig, um etwas zu erkl\u00e4ren?
2. Wie hat sich die Position seit dem Beitritt verschoben?
3. Was sind die strukturellen Probleme und Treiber?
4. Welche empirisch belegbaren Chancen und Risiken ergeben sich f\u00fcr Deutschland?

## Gliederung
# 1. Einleitung
# 2. Theorie
# 3. Analyse
""" + ("filler " * 30)
    q = extract_primary_leitfrage(msg)
    assert q is None


# ---------------------------------------------------------------------------
# Regression: production mission c00de8dd generated 47.000 words with a 17-
# section scrambled outline because extract_outline() pulled in BARE numbered
# lists nested inside outline subsections (the per-factor analysis structure
# "1. Beschreibung ... 2. Beleg ...") and numbered lists from post-outline
# single-# categories (# Quellenanforderungen) whose single hash did not end
# the outline region. The real Gliederung uses ##/### markdown headings; bare
# numbered lines are instruction text, not sections.
# ---------------------------------------------------------------------------

_OUTLINE_NOISE_BRIEFING = (
    "Hausarbeit, ca. 3.000 W\u00f6rter.\n\n"
    "# Verbindliche Gliederung\n\n"
    "## 1. Einleitung\n\n"
    "## 2. Theoretischer Bezugsrahmen\n\n"
    "### 2.1 NexMach als Unternehmung\n\n"
    "### 2.2 NexMach als System\n\n"
    "## 3. Darstellung und Analyse\n\n"
    "### 3.1 Makroumweltanalyse\n\n"
    "### 3.2 Branchenstrukturanalyse\n\n"
    "## 4. Umweltanalyse der NexMach\n\n"
    "### 4.2 Makroumwelt von NexMach\n\n"
    "F\u00fcr jeden Faktor ist folgende Struktur einzuhalten:\n\n"
    "1. Beschreibung der externen Entwicklung\n"
    "2. Beleg durch eine aktuelle Quelle\n"
    "3. konkreter Wirkungsmechanismus\n"
    "4. Betroffenheit des Gesch\u00e4ftsmodells\n"
    "5. Chance oder Risiko\n"
    "6. m\u00f6gliche Reaktion von NexMach\n\n"
    "## 5. Zentrale Umwelteinfl\u00fcsse\n\n"
    "Vorl\u00e4ufige Auswahl:\n\n"
    "1. Zugang zu Industrie- und Maschinendaten\n"
    "2. KI-, Daten- und Cybersecurity-Regulierung\n"
    "3. technologische Dynamik und Plattformabh\u00e4ngigkeit\n\n"
    "## 6. Fazit\n\n"
    "# Quellenanforderungen\n\n"
    "Verwende insgesamt 13 bis 16 Quellen.\n\n"
    "1. facheinschl\u00e4gige wissenschaftliche Quellen\n"
    "2. Praxisquellen\n"
)


def test_extract_outline_ignores_bare_numbered_lists_inside_outline():
    """Bare numbered instruction lists nested under a markdown outline
    subsection must NOT become top-level outline sections."""
    outline = extract_outline(_OUTLINE_NOISE_BRIEFING)
    titles = [s.title for s in outline]
    numbers = [s.number for s in outline]

    # Exactly the 11 real Gliederung sections.
    assert len(outline) == 11
    assert numbers == [
        "1", "2", "2.1", "2.2", "3", "3.1", "3.2", "4", "4.2", "5", "6",
    ]
    # None of the bare-list instruction items leaked in.
    for noise in [
        "Beschreibung der externen Entwicklung",
        "Beleg durch eine aktuelle Quelle",
        "m\u00f6gliche Reaktion von NexMach",
        "Zugang zu Industrie- und Maschinendaten",
        "KI-, Daten- und Cybersecurity-Regulierung",
    ]:
        assert noise not in titles, f"bogus section leaked in: {noise!r}"
    # Every surviving section carries a markdown heading marker.
    assert all(s.heading_marker for s in outline)


def test_outline_region_ends_at_single_hash_post_outline_category():
    """A single-# category after the Gliederung (e.g. # Quellenanforderungen)
    must terminate the outline region so its numbered lists are not collected."""
    import ai_researcher.agentic_layer.controller.utils.briefing_detector as bd
    region = bd._outline_region(_OUTLINE_NOISE_BRIEFING)
    assert region is not None
    start, end = region
    region_text = _OUTLINE_NOISE_BRIEFING[start:end]
    # Region must contain Fazit but NOT the post-outline Quellenanforderungen.
    assert "## 6. Fazit" in region_text
    assert "Quellenanforderungen" not in region_text
    assert "facheinschl\u00e4gige" not in region_text


def test_classify_complete_outline_not_inflated_by_noise():
    """classify_assignment must report the real section count, not the inflated
    one that caused the planner to generate 17 sections for a 6-chapter thesis."""
    c = classify_assignment(_OUTLINE_NOISE_BRIEFING)
    assert c["has_outline"] is True
    assert len(c["outline"]) == 11
    assert c["outline"][0] == {"number": "1", "title": "Einleitung", "level": 1}


# ---------------------------------------------------------------------------
# Word-budget extraction (deterministic). Must extract total + per-section
# budgets, and MUST NOT treat "470.000 Euro", "85 Mitarbeitende" or
# "13 bis 16 Quellen" as word counts.
# ---------------------------------------------------------------------------

from ai_researcher.agentic_layer.controller.utils.briefing_detector import (
    extract_word_budget as _extract_word_budget,
)


_BUDGET_BRIEFING = (
    "Die Hausarbeit umfasst ca. 3.000 W\u00f6rter.\n\n"
    "# Fallunternehmen\n"
    "Jahresumsatz: rund 19 Mio. Euro.\n"
    "Unternehmensgr\u00f6\u00dfe: 85 Mitarbeitende.\n"
    "Umsatzleistung: rund 470.000 Euro pro Mitarbeitendem.\n\n"
    "# Verbindliche Gliederung\n\n"
    "## 1. Einleitung\n"
    "Umfang: ungef\u00e4hr 230 bis 270 W\u00f6rter\n\n"
    "## 2. Theorie\n\n"
    "### 2.1 Unternehmung\n"
    "Umfang: ca. 180 bis 220 W\u00f6rter\n\n"
    "## 3. Analyse\n"
    "Umfang: 1.100 bis 1.200 W\u00f6rter\n\n"
    "## 4. Fazit\n\n"
    "# Quellenanforderungen\n"
    "Verwende 13 bis 16 Quellen.\n"
)


def test_extract_word_budget_total():
    wb = _extract_word_budget(_BUDGET_BRIEFING)
    # Total = 3000 with +/-10% window, target 3000.
    assert wb.total == (2700, 3300, 3000)


def test_extract_word_budget_sections_keyed_by_number():
    wb = _extract_word_budget(_BUDGET_BRIEFING)
    assert wb.sections == {
        "1": (230, 270),
        "2.1": (180, 220),
        "3": (1100, 1200),
    }


def test_extract_word_budget_no_false_positives_from_decoy_numbers():
    """Euro amounts, headcounts and source counts must never be budgets."""
    wb = _extract_word_budget(_BUDGET_BRIEFING)
    all_nums = set()
    if wb.total:
        all_nums.update(wb.total[:2])
    all_nums.update(v for rng in wb.sections.values() for v in rng)
    for bad in (19, 470000, 85, 13, 16):
        assert bad not in all_nums, f"decoy number {bad} leaked into budget"


def test_extract_word_budget_single_total_gets_window():
    wb = _extract_word_budget("ca. 2500 words total.\n# Gliederung\n## 1. A\n")
    assert wb.total == (2250, 2750, 2500)


def test_extract_word_budget_total_not_confused_with_section_scope():
    """The total must come from the document-level statement, not the first
    per-section scope line (regression: previously total was 250 = sec 1)."""
    wb = _extract_word_budget(_BUDGET_BRIEFING)
    assert wb.total[2] == 3000  # not 250


def test_extract_word_budget_english_range():
    wb = _extract_word_budget(
        "About 3,000 words.\n# Outline\n## 1. Intro\nScope: 200 to 250 words\n"
    )
    assert wb.total == (2700, 3300, 3000)
    assert wb.sections == {"1": (200, 250)}


def test_classify_includes_word_budget():
    c = classify_assignment(_BUDGET_BRIEFING)
    assert "word_budget" in c
    assert c["word_budget"]["total_word_budget"]["target"] == 3000
    assert c["word_budget"]["section_word_budgets"]["1"] == [230, 270]
    assert c["word_budget"]["budget_source"]  # non-empty provenance


# ---------------------------------------------------------------------------
# Priority 5: staged-output detection ("Gib zunächst noch keinen Fließtext aus")
# ---------------------------------------------------------------------------

from ai_researcher.agentic_layer.controller.utils.briefing_detector import (
    detect_staged_output as _detect_staged_output,
)


def test_detect_staged_output_german():
    msg = ("Gib zunächst noch keinen vollständigen Fließtext aus. Liefere als "
           "erste Ausgabe die Gliederung, These und Quellenmatrix.")
    assert _detect_staged_output(msg) is True


def test_detect_staged_output_english():
    msg = "Do NOT write the full text yet. First deliverable: outline + thesis."
    assert _detect_staged_output(msg) is True


def test_detect_staged_output_absent_for_normal_briefing():
    msg = "Schreibe eine vollständige Hausarbeit zum Thema NexMach."
    assert _detect_staged_output(msg) is False


def test_classify_staged_downgrades_specificity_to_structured():
    """A complete briefing that ALSO asks for a staged output must NOT be
    'complete' (which would direct-start a full draft); it stays 'structured'
    and is flagged output_stage='planning_only'."""
    msg = (
        "Hausarbeit, ca. 3.000 W\u00f6rter.\n\n"
        "Gib zun\u00e4chst noch keinen vollst\u00e4ndigen Flie\u00dftext aus. "
        "Liefere als erste Ausgabe die kommentierte Gliederung.\n\n"
        "# Verbindliche Gliederung\n"
        "## 1. Einleitung\n"
        "## 2. Theorie\n"
        "## 3. Analyse\n"
        "## 4. Fazit\n"
    )
    c = classify_assignment(msg)
    assert c["specificity"] == "structured"  # NOT complete -> no direct full start
    assert c["output_stage"] == "planning_only"


def test_classify_normal_complete_briefing_not_staged():
    """Sanity: a complete briefing without a staged directive (and without
    contradictory case assumptions) is still complete."""
    msg = (
        "Hausarbeit im Modul Organisation und Management, ca. 3.000 W\u00f6rter. "
        "Analysiere die Unternehmensumwelt eines fiktiven Unternehmens.\n"
        "Verwende APA-7 als Zitierstil und facheinschl\u00e4gige wissenschaftliche Literatur.\n\n"
        "# Verbindliche Gliederung\n"
        "## 1. Einleitung\n"
        "## 2. Theorie\n"
        "## 3. Analyse\n"
        "## 4. Fazit\n"
    )
    c = classify_assignment(msg)
    assert c["specificity"] == "complete"
    assert c["output_stage"] == "full"
    assert c["case_assumption_conflicts"] == []


# ---------------------------------------------------------------------------
# Priority 6: contradictory case-assumption detection (19 vs 40 Mio. Euro)
# ---------------------------------------------------------------------------

from ai_researcher.agentic_layer.controller.utils.briefing_detector import (
    detect_case_assumption_conflicts as _detect_conflicts,
)


_CONFLICT_BRIEFING = (
    "Fallunternehmen NexMach Systems GmbH.\n"
    "Jahresumsatz: rund 19 Mio. Euro.\n"
    "Unternehmensgr\u00f6\u00dfe: 85 Mitarbeitende.\n"
    "Umsatzleistung von rund 470.000 Euro pro Mitarbeitendem.\n"
)


def test_detect_conflicts_flags_contradictory_turnover():
    conflicts = _detect_conflicts(_CONFLICT_BRIEFING)
    assert len(conflicts) >= 1
    # The 19 Mio. vs (470k × 85 ≈ 40 Mio.) inconsistency must be reported.
    joined = " ".join(conflicts)
    assert "950" in joined or "nksistent" in joined or "rspr\u00fcchlich" in joined


def test_classify_conflict_briefing_needs_clarification():
    c = classify_assignment(_CONFLICT_BRIEFING + "\n" + _BUDGET_BRIEFING)
    assert c["specificity"] == "structured_needs_clarification"
    assert len(c["case_assumption_conflicts"]) >= 1


def test_detect_conflicts_none_for_consistent_briefing():
    msg = (
        "Jahresumsatz: rund 40 Mio. Euro.\n"
        "Unternehmensgr\u00f6\u00dfe: 85 Mitarbeitende.\n"
        "Umsatzleistung von rund 470.000 Euro pro Mitarbeitendem.\n"
    )
    # 470k × 85 ≈ 40 Mio. matches the stated 40 Mio. — no conflict.
    conflicts = _detect_conflicts(msg)
    assert conflicts == []


def test_detect_conflicts_no_false_positive_on_word_counts():
    """Word counts and source counts must not trigger case-assumption conflicts."""
    msg = (
        "Die Hausarbeit umfasst ca. 3.000 W\u00f6rter.\n"
        "Verwende 13 bis 16 Quellen. 85 Mitarbeitende arbeiten hier."
    )
    conflicts = _detect_conflicts(msg)
    assert conflicts == []


# ---------------------------------------------------------------------------
# Case-assumption CORRECTION resolution (review finding 1)
# ---------------------------------------------------------------------------
# The defining bug: the old approach concatenated the correction onto the
# original briefing text, so BOTH the stale figure (19 Mio.) and the corrected
# figure (40 Mio.) were still present and the conflict could never resolve.
# resolve_case_assumptions treats the follow-up as an authoritative override
# for the fields it mentions.

from ai_researcher.agentic_layer.controller.utils.briefing_detector import (  # noqa: E402
    resolve_case_assumptions,
    extract_case_assumptions,
)


def test_resolve_conflict_turnover_correction_resolves():
    """A follow-up giving the authoritative turnover resolves the conflict."""
    merged, remaining = resolve_case_assumptions(
        _CONFLICT_BRIEFING,
        "Jahresumsatz ist 40 Mio. Euro, die 470.000 \u20ac pro Mitarbeitendem "
        "sind die bewusste Fallannahme.",
    )
    # The follow-up's turnover (40 Mio) REPLACED the original's 19 Mio, so the
    # per-employee\u00d7headcount math is now consistent (470k\u00d785 \u2248 40 Mio).
    assert remaining == [], f"expected resolution, got {remaining}"
    # The merged turnover must be the CORRECTED value, not the stale one.
    assert merged.has_turnover()
    assert merged.turnovers[0][0] == 40_000_000
    assert merged.turnovers[0][0] != 19_000_000


def test_resolve_conflict_old_concat_approach_still_conflicts():
    """Regression guard: the naive text-concatenation approach the old code used
    must STILL report a conflict (this is the bug finding 1 fixes)."""
    followup = ("Jahresumsatz ist 40 Mio. Euro, die 470.000 \u20ac pro "
                "Mitarbeitendem sind die bewusste Fallannahme.")
    conflicts = _detect_conflicts(_CONFLICT_BRIEFING + "\n\n" + followup)
    assert conflicts, "concat approach must still conflict (bug reproduction)"


def test_resolve_conflict_non_correction_keeps_conflict():
    """A follow-up that does NOT mention turnover must keep the conflict."""
    merged, remaining = resolve_case_assumptions(
        _CONFLICT_BRIEFING,
        "Ich m\u00f6chte weitere Quellen zu NexMach hinzuf\u00fcgen.",
    )
    assert remaining, "a non-correction follow-up must not clear the conflict"
    # Untouched fields keep the original values.
    assert merged.has_per_employee()
    assert merged.per_employees[0][0] == 470_000


def test_resolve_conflict_only_per_employee_correction():
    """A follow-up giving only the per-employee figure overrides just that field."""
    merged, remaining = resolve_case_assumptions(
        _CONFLICT_BRIEFING,
        "Die korrekte Umsatzleistung betr\u00e4gt 220.000 Euro pro Mitarbeitendem.",
    )
    # Original turnover (19 Mio) and headcount (85) kept; per-employee replaced.
    assert merged.has_turnover() and merged.turnovers[0][0] == 19_000_000
    assert merged.has_headcount() and merged.headcounts[0][0] == 85
    assert merged.per_employees[0][0] == 220_000
    # 220k\u00d785 = 18.7M \u2248 19M -> now consistent (within 25%), no conflict.
    assert remaining == [], f"expected consistency, got {remaining}"


def test_extract_case_assumptions_per_employee_not_counted_as_turnover():
    """A per-employee metric ('470.000 Euro pro Mitarbeitendem') must NOT be
    double-counted as a company-total turnover figure (that previously caused
    both false conflicts and a mis-captured '1')."""
    ca = extract_case_assumptions(_CONFLICT_BRIEFING)
    # Only the company-total turnover (19 Mio) should appear as a turnover.
    turnover_values = [v for v, _ in ca.turnovers]
    assert 19_000_000 in turnover_values
    assert 470_000 not in turnover_values, "per-employee figure leaked into turnovers"
    assert ca.has_per_employee() and ca.per_employees[0][0] == 470_000
    assert ca.has_headcount() and ca.headcounts[0][0] == 85


# ---------------------------------------------------------------------------
# Integration: full create -> conflict -> correct -> resolved sequence
# (review finding 1). Exercises the metadata-state transitions the chat flow
# performs, using the pure _resolve_awaiting_clarification helper plus the
# /start guard's view of the metadata, so the whole sequence is covered without
# a live controller/database.
# ---------------------------------------------------------------------------

from ai_researcher.agentic_layer.controller.user_interaction import (  # noqa: E402
    UserInteractionManager,
)


def test_full_sequence_create_conflict_correct_resolved():
    """End-to-end metadata sequence:
      1. briefing classified -> awaiting_clarification set, conflicts stored,
         NO initial_questions staged (finding 2).
      2. user correction follow-up -> _resolve_awaiting_clarification returns
         an update that clears BOTH flags and carries the corrected overlay
         (finding 1).
      3. after applying the update, the /start guard sees no
         awaiting_clarification -> start would succeed; the overlay exposes the
         CORRECTED turnover (40 Mio), not the stale original (19 Mio).
    """
    # A briefing that is BOTH structured (has Gliederung, word count, task)
    # AND has contradictory case assumptions (19 Mio vs 470k*85). The conflict
    # detector only runs for briefings classified as structured.
    briefing = _CONFLICT_BRIEFING + "\n" + (
        "\nDie Hausarbeit umfasst ca. 3.000 W\u00f6rter (Hausarbeit).\n\n"
        "# Verbindliche Gliederung\n"
        "## 1. Einleitung\n"
        "## 2. Analyse\n"
        "## 3. Fazit\n"
        "Literaturportfolio, APA-7, Quellenangaben verpflichtend."
    )
    # --- Step 1: classify the briefing ---
    classification = classify_assignment(briefing)
    assert classification["specificity"] == "structured_needs_clarification", \
        f"step 1: expected structured_needs_clarification, got {classification['specificity']}"
    conflicts = classification["case_assumption_conflicts"]
    assert conflicts, "step 1: conflict must be detected"

    # Metadata as the chat flow would persist it on the HARD STOP branch
    # (finding 2: initial_questions is NOT staged while blocked).
    metadata = {
        "briefing_style": "structured",
        "full_briefing": briefing,
        "case_assumption_conflicts": conflicts,
        "awaiting_clarification": conflicts,
    }
    assert "initial_questions" not in metadata, "finding 2: no questions while blocked"

    # --- Step 2: user submits the authoritative correction ---
    correction = ("Jahresumsatz ist 40 Mio. Euro, die 470.000 \u20ac pro "
                 "Mitarbeitendem sind die bewusste Fallannahme.")
    update = UserInteractionManager._resolve_awaiting_clarification(metadata, correction)
    assert update is not None, "step 2: correction must resolve the conflict"
    # Both flags cleared (finding 1).
    assert update["awaiting_clarification"] is None
    assert update["case_assumption_conflicts"] == []
    overlay = update["case_assumption_corrections"]
    assert overlay["correction_text"] == correction

    # --- Step 3: apply the update and assert the /start guard + planner view ---
    metadata.update(update)
    # /missions/{id}/start checks `awaiting_clarification`; it is now cleared.
    assert not metadata.get("awaiting_clarification"), \
        "step 3: start guard must pass after resolution"
    # The planner overlay must expose the CORRECTED turnover (40 Mio), and must
    # NOT carry the stale contradictory turnover (19 Mio).
    turnovers = overlay["resolved_assumptions"]["turnovers"]
    assert any(t["value"] == 40_000_000 for t in turnovers), \
        "planner overlay must carry the corrected 40 Mio turnover"
    assert all(t["value"] != 19_000_000 for t in turnovers), \
        "planner overlay must NOT carry the stale 19 Mio turnover"


def test_full_sequence_non_correction_stays_blocked():
    """A follow-up that does not address the conflict must leave the mission
    blocked (awaiting_clarification stays set, /start guard still rejects)."""
    briefing = _CONFLICT_BRIEFING + "\n" + (
        "\nDie Hausarbeit umfasst ca. 3.000 W\u00f6rter (Hausarbeit).\n\n"
        "# Verbindliche Gliederung\n"
        "## 1. Einleitung\n## 2. Analyse\n## 3. Fazit\n"
        "Literaturportfolio, APA-7."
    )
    classification = classify_assignment(briefing)
    conflicts = classification["case_assumption_conflicts"]
    assert conflicts
    metadata = {
        "briefing_style": "structured",
        "full_briefing": briefing,
        "case_assumption_conflicts": conflicts,
        "awaiting_clarification": conflicts,
    }
    update = UserInteractionManager._resolve_awaiting_clarification(
        metadata, "Bitte verwende APA-7 als Zitierstil."
    )
    assert update is None, "non-correction must not clear the conflict"
    assert metadata.get("awaiting_clarification"), \
        "mission must stay blocked"
