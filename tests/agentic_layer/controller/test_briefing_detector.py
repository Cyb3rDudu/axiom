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
