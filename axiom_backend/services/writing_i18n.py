"""Minimal translation helper for backend-generated writing messages.

Prior state: `if de: <german> else: <english>` blocks appeared in
writing_continuation.py, writing_response_audit.py,
figure_resolution.py, writing_portfolio_manager.py,
portfolio_compliance.py — ~80 lines of string branching. Every new
flag or prompt block duplicated the shape.

New rule: any user-facing or LLM-facing string whose surface form
depends on the active language belongs in `MESSAGES` here, keyed by a
short identifier. Callers do `t(key, lang, **vars)`. Adding a new
language requires one dict column; adding a new message requires one
dict row.

This is NOT a replacement for full i18n (we don't have a PO-file
pipeline). It's a pragmatic single-file dispatch for the 15-20
backend-generated strings the writing subsystem emits.
"""

from __future__ import annotations

from typing import Any, Mapping


# Supported language codes. Anything else falls back to English.
SUPPORTED_LANGUAGES = ("de", "en")
DEFAULT_LANGUAGE = "en"


# Flat key → language-map → template. Templates use str.format placeholders.
# Order keys topic-wise (continuation, audit, portfolio, …) for readability.
MESSAGES: dict[str, dict[str, str]] = {
    # --- Continuation orchestrator ---
    "continuation.warning_fallback": {
        "de": (
            "\n\n> ⚠️ *Die Antwort konnte trotz mehrerer Fortsetzungen nicht "
            "vollständig generiert werden. Die persistierten Quellenangaben "
            "bleiben erhalten. Für die fehlenden Abschnitte bitte einen "
            "gezielten Folge-Prompt (ein Abschnitt pro Turn) verwenden.*"
        ),
        "en": (
            "\n\n> ⚠️ *The response could not be fully generated after "
            "multiple continuation attempts. The persisted references are "
            "retained. Please use a focused follow-up prompt (one section "
            "per turn) for the missing sections.*"
        ),
    },
    "continuation.prompt_section_partial": {
        "de": "Abschnitt {index} ('{title}') — aktuell abgeschnitten, beende ihn nahtlos",
        "en": "Section {index} ('{title}') — currently truncated, continue seamlessly to close it",
    },
    "continuation.prompt_section_missing": {
        "de": "Abschnitt {index}",
        "en": "Section {index}",
    },
    "continuation.budget_hint": {
        "de": " — Zielbudget: {words} Wörter",
        "en": " — target budget: {words} words",
    },
    "continuation.locked_sections": {
        "de": (
            "Abschnitte 1–{last_done} sind bereits fertig und gelockt — NICHT "
            "wiederholen, nicht referenzieren mit 'wie oben erwähnt'."
        ),
        "en": (
            "Sections 1–{last_done} are already complete and locked — do NOT "
            "repeat them, do not reference with 'as mentioned above'."
        ),
    },
    "continuation.locked_sections_no_last": {
        "de": "Abschnitte vor dem abgeschnittenen sind fertig und gelockt.",
        "en": "Sections before the truncated one are complete and locked.",
    },
    "continuation.instruction_header_de": {
        "de": "Deine vorherige Antwort wurde am Token-Limit abgeschnitten.",
        "en": "Your previous response was cut off at the token limit.",
    },
    "continuation.to_write_header": {
        "de": "Zu schreiben sind nur noch:",
        "en": "Only the following sections remain to write:",
    },
    "continuation.tail_intro": {
        "de": (
            "Letzte 400 Zeichen deiner abgeschnittenen Antwort (damit du "
            "nahtlos weitermachen kannst):"
        ),
        "en": (
            "Last 400 characters of your truncated response (so you can "
            "resume seamlessly):"
        ),
    },
    "continuation.rules_block": {
        "de": (
            "WICHTIG:\n"
            "- Emit KEIN content-block:document-Wrapper um deinen Output.\n"
            "- Emit KEINEN content-block:references-Block (bleibt unverändert).\n"
            "- Schreibe NUR den fehlenden Prosa-Text, exakt so wie er in den "
            "existierenden Body eingefügt wird.\n"
            "- Wenn du den abgeschnittenen Abschnitt weiterführst: KEINE neue "
            "Überschrift emittieren. Falls du neue Abschnitte startest: "
            "nummerierte `# N. Titel`-Überschriften wie gewohnt.\n"
            "- KEINE Wortbilanz/Wordcount-Zeile am Ende — der Backend rechnet "
            "die aus."
        ),
        "en": (
            "IMPORTANT:\n"
            "- Do NOT wrap your output in a content-block:document fence.\n"
            "- Do NOT emit a content-block:references block (unchanged).\n"
            "- Write ONLY the missing prose, exactly as it will be inserted "
            "into the existing body.\n"
            "- If you're continuing the truncated section: do NOT emit a new "
            "heading. If you're starting new sections: use numbered `# N. "
            "Title` headings as usual.\n"
            "- Do NOT emit a word-count trailer — the backend computes it."
        ),
    },
    # --- Figure resolver ---
    "figures.no_hits": {
        "de": (
            "VERFÜGBARE ABBILDUNGEN AUS DEINEM DOKUMENT-KORPUS:\n"
            "Die Suche nach passenden Abbildungen hat keine Treffer "
            "ergeben. Verwende Platzhalter-Pfade für die im Prompt "
            "geforderten Abbildungen und markiere sie klar als "
            "'später zu ersetzen'."
        ),
        "en": (
            "AVAILABLE FIGURES FROM YOUR DOCUMENT CORPUS:\n"
            "No matching figures were found. Use placeholder paths for "
            "the requested figures and mark them clearly as 'replace later'."
        ),
    },
    "figures.injection_header": {
        "de": (
            "VERFÜGBARE ABBILDUNGEN AUS DEINEM DOKUMENT-KORPUS:\n"
            "\n"
            "Jede Abbildung ist als fertige Markdown-Zeile vorgegeben mit\n"
            "  ![<REPLACE-WITH-GERMAN-CAPTION>](<echte-URL-NICHT-ÄNDERN>)\n"
            "\n"
            "WAS DU TUST:\n"
            "1. URL unverändert kopieren — Zeichen für Zeichen. KEINE Pfade "
            "erfinden, KEINE Domänen ersetzen, KEINE placeholder-fig.png\n"
            "2. Nur den alt-Text in den eckigen Klammern ersetzen durch eine "
            "inhaltlich passende Beschreibung, Format 'Abbildung N: <was zu "
            "sehen ist>'.\n"
            "3. Danach eine Bildunterschriften-Zeile in Kursiv direkt drunter:\n"
            "  `*Abbildung N: <Beschreibung>. Quelle: <Quellenangabe>. "
            "Eigene Darstellung.*`\n"
            "\n"
            "NIEMALS: 'candidate figure', 'stored caption', 'REPLACE-WITH' "
            "als alt-Text belassen — das sind Platzhalter.\n"
        ),
        "en": (
            "AVAILABLE FIGURES FROM YOUR DOCUMENT CORPUS:\n"
            "\n"
            "Each figure is pre-rendered as a ready-to-use Markdown line:\n"
            "  ![<REPLACE-WITH-CAPTION>](<real-url-do-not-modify>)\n"
            "\n"
            "WHAT YOU DO:\n"
            "1. Copy the URL unchanged — character for character. Do NOT "
            "fabricate paths, do NOT substitute domains, do NOT use "
            "placeholder-fig.png.\n"
            "2. Replace ONLY the alt text between the square brackets with "
            "a meaningful 'Figure N: <what it shows>' description.\n"
            "3. Follow with an italic caption line:\n"
            "  `*Figure N: <description>. Source: <citation>.*`\n"
            "\n"
            "NEVER leave 'candidate figure', 'stored caption', or "
            "'REPLACE-WITH' as alt text — those are placeholder tokens.\n"
        ),
    },
    "figures.for_query": {
        "de": "\nFür '{description}':",
        "en": "\nFor '{description}':",
    },
    "figures.fallback_query": {
        "de": "(allgemeine Abbildung)",
        "en": "(generic figure)",
    },
    # --- Wordcount recompute trailer ---
    "wordcount.trailer_header": {
        "de": "**Wortbilanz (exkl. Titelblatt und Literaturverzeichnis): {total} Wörter**",
        "en": "**Word count (excl. title page and bibliography): {total} words**",
    },
    # --- Portfolio fallback bullets ---
    "portfolio.fallback_relevance_cited": {
        "de": "In {n} Abschnitt(en) zitiert; konkreter Beitrag bitte manuell ergänzen.",
        "en": "Cited in {n} section(s); specific contribution to be filled in manually.",
    },
    "portfolio.fallback_relevance_uncited": {
        "de": "Konkreter Beitrag bitte manuell ergänzen.",
        "en": "Specific contribution to be filled in manually.",
    },
    "portfolio.fallback_quality": {
        "de": "Publisher-Tier: {tier}; Publikationstyp: {ptype}",
        "en": "Publisher tier: {tier}; publication type: {ptype}",
    },
}


def normalize_language_code(raw: Any) -> str:
    """Return a supported language code; fall back to DEFAULT_LANGUAGE."""
    if not raw:
        return DEFAULT_LANGUAGE
    code = str(raw)[:2].lower()
    return code if code in SUPPORTED_LANGUAGES else DEFAULT_LANGUAGE


def t(key: str, lang: str = DEFAULT_LANGUAGE, /, **kwargs: Any) -> str:
    """Look up `key` in the active language and substitute placeholders.

    Falls back to English if the language isn't supported or the key
    isn't translated. Missing keys raise KeyError (programmer error —
    a string should exist in MESSAGES or not be routed through t()).
    """
    normalised = normalize_language_code(lang)
    entry = MESSAGES[key]
    template = entry.get(normalised) or entry.get(DEFAULT_LANGUAGE) or next(iter(entry.values()))
    if not kwargs:
        return template
    return template.format(**kwargs)


def has_translation(key: str, lang: str) -> bool:
    """Tiny testing helper — does MESSAGES[key][lang] exist?"""
    normalised = normalize_language_code(lang)
    return key in MESSAGES and normalised in MESSAGES[key]
