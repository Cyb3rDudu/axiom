"""Shared KMU compliance + markdown rendering for Literaturportfolio (#72).

Both `LiteraturePortfolioManager` (mission-side) and
`WritingPortfolioManager` (writing-side) need identical compliance
thresholds and identical markdown tables. Prior to this module both
copied the same ~160 lines of logic — this kept byte-identity by
convention, not by contract, so any rubric tweak would silently drift
between the two code paths.

The public API is two pure functions + four constants. Both managers
import and call through; no state, no DB, no LLM.

**Do not fork this module.** If a caller needs different compliance
thresholds, add a parameter here rather than duplicating the logic.
"""

from __future__ import annotations

from typing import Iterable, List

from ai_researcher.agentic_layer.schemas.portfolio import (
    ComplianceReport,
    PortfolioEntry,
)


# ---------------------------------------------------------------------------
# KMU compliance thresholds
# ---------------------------------------------------------------------------

COMPLIANCE_MIN_SOURCES = 10
COMPLIANCE_MAX_SOURCES = 20
COMPLIANCE_SCIENTIFIC_SHARE_MIN = 0.5
RECENCY_WARNING_YEARS = 10


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------


def compute_compliance(
    entries: List[PortfolioEntry], *, language_code: str
) -> ComplianceReport:
    """KMU rubric: 10–20 Quellen, ≥50 % wissenschaftlich (Tier A/B), no
    blacklist hits. Recency warnings (>10 years) are advisory, not red.

    Traffic-light resolution:
    - green: source-count in range + share ok + no blacklist hits
    - red:   blacklist hits OR share below 50 %
    - yellow: everything else (typically count outside range only)
    """
    n = len(entries)
    scientific = sum(1 for e in entries if e.scientific_tier in {"A", "B"})
    share = (scientific / n) if n else 0.0
    blacklist_hits = [
        e.apa_citation
        for e in entries
        if e.quality_signals.publisher_tier == "blacklist"
    ]
    recency_warnings = [
        e.apa_citation
        for e in entries
        if e.quality_signals.recency_years is not None
        and e.quality_signals.recency_years > RECENCY_WARNING_YEARS
    ]
    source_count_ok = COMPLIANCE_MIN_SOURCES <= n <= COMPLIANCE_MAX_SOURCES
    share_ok = share >= COMPLIANCE_SCIENTIFIC_SHARE_MIN

    advice: List[str] = []
    de = language_code.startswith("de")
    if n < COMPLIANCE_MIN_SOURCES:
        advice.append(
            f"Quellenanzahl {n} unter dem Zielkorridor ({COMPLIANCE_MIN_SOURCES}–{COMPLIANCE_MAX_SOURCES}) — zusätzliche Quellen aufnehmen."
            if de
            else f"Source count {n} is below target ({COMPLIANCE_MIN_SOURCES}–{COMPLIANCE_MAX_SOURCES}) — add more sources."
        )
    if n > COMPLIANCE_MAX_SOURCES:
        advice.append(
            f"Quellenanzahl {n} über dem Zielkorridor — Liste fokussieren."
            if de
            else f"Source count {n} exceeds target — tighten the selection."
        )
    if not share_ok:
        advice.append(
            f"Wissenschaftlicher Anteil nur {share:.0%} (Ziel ≥ 50 %) — mehr peer-reviewte Quellen aufnehmen."
            if de
            else f"Scientific share only {share:.0%} (target ≥ 50%) — include more peer-reviewed sources."
        )
    if blacklist_hits:
        advice.append(
            "Blacklist-Treffer erkannt (z. B. Wikipedia, Gabler, Boulevard) — gemäß KMU Dos-and-Don'ts durch facheinschlägige Quellen ersetzen."
            if de
            else "Blacklist hits detected (e.g. Wikipedia, Gabler, tabloids) — replace with scholarly sources per KMU guidelines."
        )
    if recency_warnings:
        advice.append(
            f"{len(recency_warnings)} Quelle(n) älter als {RECENCY_WARNING_YEARS} Jahre — falls nicht bewusst gewählt, aktuellere Literatur einbinden."
            if de
            else f"{len(recency_warnings)} source(s) older than {RECENCY_WARNING_YEARS} years — consider more recent literature unless chosen deliberately."
        )

    if source_count_ok and share_ok and not blacklist_hits:
        traffic = "green"
    elif blacklist_hits or not share_ok:
        traffic = "red"
    else:
        traffic = "yellow"

    return ComplianceReport(
        source_count=n,
        source_count_ok=source_count_ok,
        scientific_share=share,
        scientific_share_ok=share_ok,
        blacklist_hits=blacklist_hits,
        recency_warnings=recency_warnings,
        traffic_light=traffic,  # type: ignore[arg-type]
        advice=advice,
    )


def render_markdown(
    entries: List[PortfolioEntry],
    compliance: ComplianceReport,
    *,
    language_code: str,
    intro: str | None = None,
) -> str:
    """Render the Literaturportfolio markdown table + compliance summary.

    `intro` lets callers inject a context-specific one-liner (mission
    vs. writing-mode) under the heading. Defaults to the
    mission-phrasing when omitted, for backwards compatibility.
    """
    de = language_code.startswith("de")
    heading = "## Literaturportfolio" if de else "## Literature Portfolio"
    if intro is None:
        intro = (
            "_Automatisch erstellt. Umfasst alle im Bericht tatsächlich zitierten Quellen._"
            if de
            else "_Automatically generated. Covers every source actually cited in the report._"
        )
    cols = (
        ["Quellenangabe (lt. Literaturverzeichnis)", "Recherchetool", "Relevanz", "Qualität"]
        if de
        else ["Source (as in bibliography)", "Discovery tool", "Relevance", "Quality"]
    )

    def _bullets(lines: Iterable[str]) -> str:
        return "<br>".join(f"• {ln}" for ln in lines)

    out: List[str] = [heading, "", intro, ""]
    out.append("| " + " | ".join(cols) + " |")
    out.append("|" + "|".join(["---"] * len(cols)) + "|")
    for e in entries:
        citation_cell = e.apa_citation.replace("\n", " ").replace("|", r"\|")
        discovery_cell = e.discovery_tool.replace("|", r"\|")
        rel_cell = _bullets(e.relevance_bullets).replace("|", r"\|")
        qua_cell = _bullets(e.quality_bullets).replace("|", r"\|")
        out.append(f"| {citation_cell} | {discovery_cell} | {rel_cell} | {qua_cell} |")

    traffic_emoji = {"green": "🟢", "yellow": "🟡", "red": "🔴"}[compliance.traffic_light]
    traffic_word = (
        {"green": "grün", "yellow": "gelb", "red": "rot"}[compliance.traffic_light]
        if de
        else compliance.traffic_light
    )
    label = "Compliance-Ampel" if de else "Compliance traffic light"
    share_pct = f"{compliance.scientific_share * 100:.0f}%"

    out.append("")
    out.append(f"**{label}: {traffic_emoji} {traffic_word}**")
    if de:
        out.append(
            f"- {compliance.source_count} Quellen (Zielkorridor "
            f"{COMPLIANCE_MIN_SOURCES}–{COMPLIANCE_MAX_SOURCES})"
        )
        out.append(
            f"- {share_pct} wissenschaftlich/facheinschlägig "
            f"(Ziel ≥ {int(COMPLIANCE_SCIENTIFIC_SHARE_MIN * 100)} %)"
        )
    else:
        out.append(
            f"- {compliance.source_count} sources (target range "
            f"{COMPLIANCE_MIN_SOURCES}–{COMPLIANCE_MAX_SOURCES})"
        )
        out.append(
            f"- {share_pct} scientific / in-field "
            f"(target ≥ {int(COMPLIANCE_SCIENTIFIC_SHARE_MIN * 100)}%)"
        )
    if compliance.blacklist_hits:
        tag = "Blacklist-Treffer" if de else "Blacklist hits"
        out.append(f"- **{tag}:** {len(compliance.blacklist_hits)}")
    if compliance.recency_warnings:
        tag = "Aktualitäts-Warnungen" if de else "Recency warnings"
        out.append(f"- {tag}: {len(compliance.recency_warnings)}")
    for msg in compliance.advice:
        out.append(f"- {msg}")

    return "\n".join(out)
