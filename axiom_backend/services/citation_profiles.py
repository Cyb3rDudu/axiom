"""
Citation Profile Service for configurable citation styles.

Manages built-in and custom citation profiles that control how the writing agent
formats in-text citations and bibliographies. Profiles are injected into the
writing agent's system prompt to replace hardcoded citation rules.
"""

import logging
import re
from typing import List, Optional, Dict, Any
from api.schemas import CitationProfile

logger = logging.getLogger(__name__)


# =============================================================================
# Built-in Profiles
# =============================================================================

NUMBERED_PROFILE = CitationProfile(
    id="numbered",
    name="Numbered Citations",
    citation_mode="numbered",
    is_builtin=True,
    in_text_rules="""1.  Whenever you incorporate information *directly derived* from notes belonging to a specific source document (`Research Notes`), you MUST insert a citation placeholder immediately following that piece of information (or within the table cell).
2.  The placeholder format MUST be **exactly** `[doc_id]`, using the specific Document ID (e.g., `f28769c8`) provided in the 'Research Notes' section header for that source.
3.  **Frequency:** If multiple consecutive sentences or a distinct passage of thought draws *only* from notes belonging to the *same source document*, place a SINGLE `[doc_id]` placeholder at the **end** of that passage or the last sentence drawing from that source. Do NOT add a placeholder after every sentence if the source remains the same for the immediate context.
4.  **Synthesized Sentences:** If a single sentence combines information or claims originating from *different* source documents (based on their `doc_id` in the `Research Notes`), you MUST place the corresponding `[doc_id]` placeholder *immediately after each specific piece of information or claim* it supports within that sentence. Do not group citations at the end if they support distinct parts of the sentence derived from different sources. Example: "In the literature we see increased risk [f28769c8] but improved outcomes with intervention [7525d6d3]."
5.  **DO NOT** combine multiple doc IDs inside a single bracket (e.g., `[f28769c8, 7525d6d3]`). Each citation must be separate: `[f28769c8] [7525d6d3]`.
6.  **DO NOT** invent citations or use any other format (like [1], [Source A], Author Year, etc.). Use ONLY the `[doc_id]` format provided in the 'Research Notes' headers (e.g., `[f28769c8]`, `[a3b1c9d0]`).
7.  **ABSOLUTELY DO NOT use the `Note ID` (e.g., `note_xyz123`) as a citation.** The `Note ID` is for internal reference only. Citations MUST use the `Document ID` (`doc_id`) specified in the source header (like `[f28769c8]`). Using `note_id` in brackets is incorrect and will break the referencing system.
8.  **Grounding:** Ensure every claim or piece of information you write is directly supported by the provided 'Research Notes' (for `research_based` sections) or the 'Content from Previous Sections' (for `content_based` or synthesis sections). If you cannot find support in the provided context, DO NOT include the information.""",
    bibliography_rules="""Format the bibliography as a numbered list matching the in-text citation numbers [1], [2], etc.
Each entry should include: author(s), year, title, and source/publication information.
Sort entries by the order they first appear in the text (matching their citation number).""",
)

KMU_APA6_PROFILE = CitationProfile(
    id="kmu_apa6",
    name="KMU Akademie APA 6 (Deutsch)",
    citation_mode="author_year",
    is_builtin=True,
    in_text_rules="""1.  **In-Text-Zitierformat:** Verwende IMMER das Format `(Autor, Jahr, S. XX)` für direkte und indirekte Zitate. Die Seitenangabe ist PFLICHT. Wenn keine Seitenzahl verfügbar ist, verwende `o. S.` (ohne Seite).
2.  **Einzelner Autor:** `(Müller, 2020, S. 45)` oder `(Müller, 2020, o. S.)`.
3.  **Zwei Autoren:** Verwende `&` zwischen den Namen: `(Müller & Schmidt, 2020, S. 45)`.
4.  **Drei oder mehr Autoren:** Verwende beim ERSTEN Zitat alle Autoren, danach `et al.`: Erstes Mal: `(Müller, Schmidt & Weber, 2020, S. 45)`, danach: `(Müller et al., 2020, S. 45)`.
5.  **Mehrere Quellen:** Trenne verschiedene Quellen innerhalb einer Klammer mit Semikolon: `(Müller, 2020, S. 45; Schmidt, 2019, S. 12)`.
6.  **Sekundärzitate:** Wenn eine Quelle eine andere zitiert: `(Meier, 1991, S. 30 zit. in Müller, 1993, S. 45)`.
7.  **Seitenbereiche:** Verwende IMMER exakte Seitenbereiche (z.B. `S. 45-47`). Die Verwendung von `f.` oder `ff.` ist NICHT erlaubt.
8.  **Webquellen ohne Seitenzahlen:** Verwende `o. S.`: `(Müller, 2020, o. S.)`.
9.  **Wörtliche Zitate:** Setze wörtliche Zitate in Anführungszeichen und gib die exakte Seitenangabe an: `„Zitat" (Müller, 2020, S. 45)`.
10. **Organisationen als Autor:** Beim ersten Zitat den vollen Namen, danach die Abkürzung: Erstes Mal: `(Weltgesundheitsorganisation [WHO], 2020, S. 10)`, danach: `(WHO, 2020, S. 10)`.
11. **Quellenverankerung:** Verwende die Metadaten (Autor, Jahr) aus den 'Research Notes'-Überschriften. Erfinde KEINE Zitate. Jede Behauptung MUSS durch die bereitgestellten Forschungsnotizen belegt sein.
12. **WICHTIG:** Verwende NICHT das `[doc_id]`-Klammer-Format. Verwende NUR das `(Autor, Jahr, S. XX)`-Format.""",
    bibliography_rules="""Formatiere das Literaturverzeichnis nach APA 6 (Deutsch):
- **Bücher:** Autor, V. (Jahr). *Titel* (Auflage). Erscheinungsort: Verlag.
- **Zeitschriftenartikel:** Autor, V. (Jahr). Titel des Artikels. *Zeitschriftenname, Band*(Heft), Seiten.
- **Sammelwerke:** Autor, V. (Jahr). Titel des Beitrags. In V. Herausgeber (Hrsg.), *Titel des Sammelwerks* (Auflage, S. XX-XX). Erscheinungsort: Verlag.
- **Internetquellen:** Autor, V. (Jahr). *Titel*. Verfügbar unter: URL [Zugriff am TT.MM.JJJJ].
- Sortiere alphabetisch nach Nachnamen des Erstautors.
- Bei mehreren Werken desselben Autors: chronologisch sortieren (ältestes zuerst).
- Hängende Einrückung für jeden Eintrag.""",
)

APA7_EN_PROFILE = CitationProfile(
    id="apa7_en",
    name="APA 7th Edition (English)",
    citation_mode="author_year",
    is_builtin=True,
    in_text_rules="""1.  **In-text citation format:** ALWAYS use the format `(Author, Year, p. XX)` for direct and indirect quotes. Page numbers are required when available. Use `n.p.` (no page) when page numbers are not available.
2.  **Single author:** `(Smith, 2020, p. 45)` or `(Smith, 2020, n.p.)`.
3.  **Two authors:** Use `&` between names: `(Smith & Johnson, 2020, p. 45)`.
4.  **Three or more authors:** Use `et al.` from the FIRST citation: `(Smith et al., 2020, p. 45)`.
5.  **Multiple sources:** Separate different sources within parentheses with a semicolon, in alphabetical order: `(Johnson, 2019, p. 12; Smith, 2020, p. 45)`.
6.  **Secondary citations:** When a source cites another: `(Meier, 1991, as cited in Smith, 2020, p. 45)`.
7.  **Page ranges:** Use exact page ranges (e.g., `pp. 45-47`). Use `p.` for a single page and `pp.` for a range.
8.  **Direct quotes:** Place direct quotes in quotation marks with exact page: `"quote" (Smith, 2020, p. 45)`.
9.  **Organizations as author:** Use full name first time, abbreviation after: First: `(World Health Organization [WHO], 2020, p. 10)`, then: `(WHO, 2020, p. 10)`.
10. **Source grounding:** Use author/year metadata from the 'Research Notes' headers. Do NOT invent citations. Every claim MUST be supported by the provided research notes.
11. **IMPORTANT:** Do NOT use the `[doc_id]` bracket format. Use ONLY the `(Author, Year, p. XX)` format.""",
    bibliography_rules="""Format the reference list according to APA 7th Edition:
- **Books:** Author, A. A. (Year). *Title of work: Capital letter also for subtitle* (Edition). Publisher. DOI or URL
- **Journal articles:** Author, A. A. (Year). Title of article. *Title of Periodical, Volume*(Issue), pages. DOI or URL
- **Edited book chapters:** Author, A. A. (Year). Title of chapter. In E. E. Editor (Ed.), *Title of work* (pp. xx-xx). Publisher. DOI or URL
- **Websites:** Author, A. A. (Year, Month Day). *Title of page*. Site Name. URL
- Sort alphabetically by last name of first author.
- For multiple works by the same author: sort chronologically (earliest first).
- Use hanging indent for each entry.
- Include DOIs as hyperlinks when available.""",
)

BUILTIN_PROFILES: List[CitationProfile] = [
    NUMBERED_PROFILE,
    KMU_APA6_PROFILE,
    APA7_EN_PROFILE,
]

_BUILTIN_PROFILES_BY_ID: Dict[str, CitationProfile] = {
    p.id: p for p in BUILTIN_PROFILES
}


# =============================================================================
# Profile Access Functions
# =============================================================================

def get_builtin_profiles() -> List[CitationProfile]:
    """Return all built-in citation profiles."""
    return list(BUILTIN_PROFILES)


def get_all_profiles(user_settings: Optional[Dict[str, Any]] = None) -> List[CitationProfile]:
    """Return all profiles: built-in + user's custom profiles.

    Custom profiles are stored in user_settings["writing_settings"]["citation_profiles"].
    """
    profiles = list(BUILTIN_PROFILES)

    if user_settings:
        writing_settings = user_settings.get("writing_settings") or {}
        custom_profiles_data = writing_settings.get("citation_profiles") or []
        for cp_data in custom_profiles_data:
            try:
                profile = CitationProfile(**cp_data)
                # Ensure custom profiles are not marked as builtin
                profile.is_builtin = False
                profiles.append(profile)
            except Exception as e:
                logger.warning(f"Skipping malformed custom citation profile: {e}")

    return profiles


def get_profile_by_id(
    profile_id: str,
    user_settings: Optional[Dict[str, Any]] = None,
) -> Optional[CitationProfile]:
    """Look up a single profile by ID (built-in first, then custom)."""
    # Check built-in profiles
    if profile_id in _BUILTIN_PROFILES_BY_ID:
        return _BUILTIN_PROFILES_BY_ID[profile_id]

    # Check custom profiles from user settings
    if user_settings:
        writing_settings = user_settings.get("writing_settings") or {}
        custom_profiles_data = writing_settings.get("citation_profiles") or []
        for cp_data in custom_profiles_data:
            if cp_data.get("id") == profile_id:
                try:
                    profile = CitationProfile(**cp_data)
                    profile.is_builtin = False
                    return profile
                except Exception as e:
                    logger.warning(f"Failed to parse custom citation profile '{profile_id}': {e}")
                    return None

    return None


def resolve_citation_profile(
    mission_metadata: Optional[Dict[str, Any]] = None,
    user_settings: Optional[Dict[str, Any]] = None,
) -> CitationProfile:
    """Resolve which citation profile to use.

    Resolution chain:
    1. Mission-level override (mission_metadata["citation_profile_id"])
    2. User default (user_settings["writing_settings"]["default_citation_profile"])
    3. Fallback to "numbered"
    """
    profile_id = None

    # 1. Mission-level override
    if mission_metadata:
        profile_id = mission_metadata.get("citation_profile_id")

    # 2. User default
    if not profile_id and user_settings:
        writing_settings = user_settings.get("writing_settings") or {}
        profile_id = writing_settings.get("default_citation_profile")

    # 3. Fallback
    if not profile_id:
        profile_id = "numbered"

    profile = get_profile_by_id(profile_id, user_settings)
    if profile is None:
        # If the requested profile doesn't exist, fall back to numbered
        return NUMBERED_PROFILE

    return profile
