"""Writer system-prompt builder.

Extracted from ``simplified_writing_agent._run_main_llm`` so the
~250-line system-prompt construction becomes a pure, segment-composed
function that can be unit-tested per segment and per flag combination.

Composition order (fixed):
    base → citation → structured_bibliography → replace_mode → custom → external_context

Each segment is built by a private helper returning a plain string; the
public entry point concatenates them and returns the final prompt. No
model calls, no DB access, no I/O — pure string assembly.

The caller passes the resolved citation profile, flag values, and the
user prompt; this module does not know about user settings, context
dicts, or feature-flag resolution helpers. That resolution stays in the
pipeline / agent.
"""

from __future__ import annotations

import re
from typing import Optional, Protocol


# Replace-mode trigger words — German + English revision verbs that the
# writer keeps interpreting as "add alongside" rather than "replace".
# The lookahead cap keeps a long fix-list with a replace instruction in
# item 3 still triggering the injection.
_REPLACE_VERBS = re.compile(
    r"\b(ersetze|ersetzen|ersatz|tausche|tauschen|swap|swaps|swapping|"
    r"replace|replaces|replacing)\b",
    re.IGNORECASE,
)


def is_replace_mode_prompt(prompt: str, lookahead: int = 500) -> bool:
    """Return True when the prompt contains replace-style instructions.

    Matched against the first ``lookahead`` chars so a long fix-list
    ("5 tweaks: … 3. ersetze X durch Y …") still triggers the
    replace-mode system-prompt injection.
    """
    if not prompt:
        return False
    return bool(_REPLACE_VERBS.search(prompt[:lookahead]))


class _CitationProfileLike(Protocol):
    """Minimal shape of a citation profile used here.

    The full ``services.citation_profiles.CitationProfile`` satisfies this
    but we avoid the import coupling — only ``in_text_rules`` is read.
    """

    in_text_rules: str


# ---------------------------------------------------------------------------
# Segments
# ---------------------------------------------------------------------------


_BASE_SYSTEM_PROMPT = (
    "You are Axiom, a collaborative writing assistant helping users write documents. "
    "Your responses should be helpful, informative, and directly address the user's request. "
    "You have access to information about which tools are currently enabled or disabled. "
    "\n\nCRITICAL - MATHEMATICAL NOTATION: Always use standard Markdown/LaTeX notation:\n"
    "• For inline math: $formula$ (single dollar signs)\n"
    "• For display math: $$formula$$ (double dollar signs on separate lines)\n"
    "• NEVER use square brackets [ ], parentheses \\( \\), or \\begin{equation} for math delimiters\n"
    "If a user's request would benefit from a tool that is currently disabled, suggest they enable it.\n\n"
)


_BLOCK_FORMATTING_SUFFIX = (
    "If the user's request implies changes to the document, describe what changes you would make. \n\n"
    "WRITING STYLE GUIDELINES:\n"
    "Use bullet points only sparingly and only if absolutely necessary. Otherwise, write in reasonably length paragraphs that flow naturally and provide comprehensive coverage of topics.\n\n"
    "IMPORTANT FORMATTING INSTRUCTIONS:\n"
    "When generating substantial content, wrap each distinct content block using this format:\n"
    "```content-block:BLOCK_TYPE\n"
    "Your content here...\n"
    "```\n\n"
    "Available BLOCK_TYPE options (USE THE MOST APPROPRIATE ONE):\n"
    "- document: Complete document or article (use for full documents with multiple sections)\n"
    "- section: Individual section with headings and content (use for major parts of a document)\n"
    "- paragraph: Single paragraph or brief explanatory text (use for short responses)\n"
    "- list: Bullet points or numbered lists (use ONLY for actual lists)\n"
    "- note: Important notes, warnings, or callouts (use sparingly for special notices)\n"
    "- code: ONLY for actual programming code, scripts, or terminal commands\n\n"
    "CRITICAL RULES:\n"
    "1. DO NOT use 'code' block type for regular text, formulas, or tables\n"
    "2. For mathematical formulas and equations, use $ / $$ delimiters (NEVER [ ], \\( \\), or \\begin{equation}). "
    "ALWAYS escape backslashes in LaTeX commands (use \\\\ instead of \\).\n"
    "3. For tables, use 'section' or 'paragraph' with Markdown table syntax\n"
    "4. Default to 'section' for most structured content\n"
    "5. Use 'paragraph' for brief responses\n\n"
    "Example for scientific content with formulas:\n"
    "```content-block:section\n"
    "# Mathematical Formulas\n\n"
    "The quadratic equation $ax^2 + bx + c = 0$ has solutions given by:\n\n"
    "$$x = \\frac{-b \\pm \\sqrt{b^2 - 4ac}}{2a}$$\n\n"
    "For quantum mechanics, the Schrödinger equation is:\n\n"
    "$$i\\hbar\\frac{\\partial}{\\partial t}\\Psi = \\hat{H}\\Psi$$\n"
    "```\n"
)


_AUTHOR_YEAR_CITATION_PREAMBLE = (
    "CITATION INSTRUCTIONS:\n"
    "When you have access to external information (web search or document search results), you MUST:\n"
    "1. Integrate that information naturally into your response\n"
    "2. Use author-year citation format as described below\n"
    "3. Place citations IMMEDIATELY after the relevant statement or claim\n\n"
)


_NUMBERED_CITATION_BLOCK = (
    "CITATION INSTRUCTIONS:\n"
    "When you have access to external information (web search or document search results), you MUST:\n"
    "1. Integrate that information naturally into your response\n"
    "2. Add citations using the EXACT Citation IDs provided in square brackets\n"
    "3. Place citations IMMEDIATELY after the relevant statement or claim\n"
    "4. Use ONLY the 8-character Citation IDs shown in the search results\n\n"
    "CORRECT citation examples:\n"
    "- 'Recent studies show that climate change is accelerating [a3b4c5d6].'\n"
    "- 'The document states that revenue increased by 25% [f2e8d9c1] in Q3.'\n"
    "- 'According to the research [b7a4e3f2], this method improves accuracy.'\n\n"
    "INCORRECT citations (NEVER do this):\n"
    "- 'Recent studies show this [1].' ← Wrong! Don't use numbers\n"
    "- 'The data shows [Source 1]...' ← Wrong! Use the exact Citation ID\n"
    "- 'According to research...' ← Wrong! Missing citation\n\n"
    "Each search result will show '**Citation ID: [xxxxxxxx]**' - use these EXACT IDs.\n"
    "Always be specific about where information comes from when using external sources.\n\n"
)


_STRUCTURED_BIBLIOGRAPHY_BLOCK = (
    "\n\nSTRUCTURED BIBLIOGRAPHY (required when you cite any source):\n"
    "\n"
    "OUTPUT ORDER (critical for token budget):\n"
    "1. EMIT THE `content-block:references` BLOCK FIRST, before any\n"
    "   other content-block. The block is small JSON; if the response\n"
    "   gets truncated, the recoverable prose tail is what we lose,\n"
    "   not the structured bibliography.\n"
    "2. THEN emit the `content-block:document` with the draft body.\n"
    "3. DO NOT emit a Markdown `## Literaturverzeichnis` section\n"
    "   inside the document body. The structured registry is the\n"
    "   single source of truth — the DOCX export and UI render the\n"
    "   bibliography deterministically from it. A duplicate inline\n"
    "   section just inflates the token budget and risks drift.\n"
    "\n"
    "FORMAT:\n"
    "```content-block:references\n"
    "[\n"
    '  {\n'
    '    \"entry_key\": \"destatis-2024\",\n'
    '    \"authors\": [{\"family\": \"Destatis\", \"given\": \"\"}],\n'
    '    \"year\": 2024,\n'
    '    \"title\": \"Außenhandel 2024\",\n'
    '    \"container_title\": \"Statistisches Bundesamt\",\n'
    '    \"url\": \"https://www.destatis.de/...\",\n'
    '    \"accessed_at\": \"2026-04-24\",\n'
    '    \"reference_type\": \"web\"\n'
    '  }\n'
    "]\n"
    "```\n"
    "RULES for the references block:\n"
    "1. entry_key: stable per-draft slug (lowercase, ASCII, dash-separated). "
    "The SAME key that your in-text citations reference.\n"
    "2. authors: array of {family, given}. Institutional authors use "
    "{family: 'Destatis', given: ''}.\n"
    "3. year: integer; omit the field for 'n.d.' / 'o. J.' sources.\n"
    "4. At least one of url / container_title / publisher must be set. "
    "For BOOK references without a URL, `publisher` is MANDATORY "
    "(e.g. 'Vahlen', 'Springer Gabler', 'vdf Hochschulverlag ETH Zürich', "
    "'Mohr Siebeck', 'Nomos'). Entries without any of the four "
    "fields get rejected by the backend and drop out silently.\n"
    "5. Every in-text citation in this response MUST have a matching "
    "entry in this block. No orphan citations.\n"
    "6. EVERY entry in this block MUST be cited at least once in the "
    "body with a proper in-text citation. An entry that is not "
    "cited ANYWHERE in the body gets flagged as a DEAD ENTRY "
    "by the backend sync and damages the response quality score. "
    "Before emitting an entry, mentally check: where exactly in "
    "my body does a citation for it appear? If the answer is "
    "'nowhere', OMIT THE ENTRY.\n"
    "7. This block REPLACES the full bibliography — on each revision "
    "turn you own the entire registry. Missing a previous entry means "
    "it gets deleted.\n"
    "8. EMIT THE BLOCK EXACTLY ONCE per response. Do not split it, "
    "do not re-emit a 'continued' second references block. Start "
    "by listing the in-text citations you intend to make, THEN "
    "emit only those entries. A registry handed to you from a "
    "prior turn (e.g. mission handoff) is a candidate set, not a "
    "requirement — keep only what you actually cite.\n"
    "\n"
    "IN-TEXT CITATION FORMAT (strict, for reliable sync against the "
    "references block):\n"
    "- Use FAMILY-NAME ONLY in in-text citations, no given names:\n"
    "  ✅ (Smith & Jones, 2020, p. 45) / (Hotz-Hart & Rohner, 2014, S. 4-6)\n"
    "  ❌ (John Smith & Alice Jones, 2020, p. 45)\n"
    "- Hyphenated surnames stay intact: (Hotz-Hart, 2014, S. 4).\n"
    "- Three or more authors: use 'et al.' from the first citation, "
    "no given names, no '&' expansion.\n"
    "- Institutional authors follow the first-full-then-abbreviation "
    "convention of the active citation profile.\n"
)


_REPLACE_MODE_BLOCK = (
    "\n\nREPLACE-MODE: The user's prompt asks you to replace existing "
    "content, sources, or phrases. For EVERY swap the user requests "
    "you MUST:\n"
    "1. Remove ALL occurrences of the old item from the draft body.\n"
    "2. Remove the old item from the bibliography if present.\n"
    "3. Insert the new item in its place, keeping surrounding context.\n"
    "4. Do NOT leave the old item alongside the new one — that is the "
    "failure mode to avoid.\n"
    "If you cannot find the old item, or cannot locate a suitable "
    "replacement, state that explicitly in the response rather than "
    "silently skipping. When multiple old items should be removed, "
    "confirm in one line per swap how many instances you removed."
)


_EXTERNAL_CONTEXT_FALLBACK_BLOCK = (
    "\n\nIMPORTANT: Some or all search results were filtered as not relevant to your query. "
    "You should inform the user that you couldn't find highly relevant current sources. "
    "Suggest they either: 1) Enable deep search mode for more thorough results, 2) Rephrase their query to be more specific, "
    "or 3) Check if the information they're looking for might be too recent or specialized. "
    "Do NOT attempt to answer based on general knowledge when the user explicitly asked for current/recent information."
)


_EXTERNAL_CONTEXT_AVAILABLE_BLOCK = (
    "\n\nIMPORTANT: You have access to external information from enabled tools. "
    "Use this information to provide more accurate, detailed, and well-sourced responses. "
    "When referencing information from these sources, mention the source (e.g., 'According to [source]' or 'Based on the search results'). "
    "If the external information contradicts something in the current draft, point this out to the user."
)


_NO_EXTERNAL_CONTEXT_BLOCK = (
    "\n\nNOTE: No external information was gathered for this request. "
    "If the user's question would benefit from web search or document search, "
    "suggest they enable the appropriate tools using the controls in the interface."
)


# ---------------------------------------------------------------------------
# Segment builders
# ---------------------------------------------------------------------------


def _citation_segment(
    citation_mode: str,
    citation_profile: Optional[_CitationProfileLike],
) -> str:
    """Author-year vs numbered citation instructions.

    Author-year requires a resolved citation profile for the in-text rules;
    if one isn't provided we fall back to the numbered block to avoid
    emitting an unbounded/undefined citation format.
    """
    if citation_mode == "author_year" and citation_profile is not None:
        return (
            _AUTHOR_YEAR_CITATION_PREAMBLE
            + f"{citation_profile.in_text_rules}\n\n"
            "Always be specific about where information comes from when using external sources.\n\n"
        )
    return _NUMBERED_CITATION_BLOCK


def _external_context_segment(external_context: str) -> str:
    """Trailer explaining whether external info is available / filtered / absent."""
    if not external_context:
        return _NO_EXTERNAL_CONTEXT_BLOCK
    if "sources were deemed not relevant and excluded" in external_context:
        return _EXTERNAL_CONTEXT_FALLBACK_BLOCK
    return _EXTERNAL_CONTEXT_AVAILABLE_BLOCK


def _custom_prompt_segment(custom_prompt: str) -> str:
    """Optional user-supplied addendum block."""
    custom = (custom_prompt or "").strip()
    if not custom:
        return ""
    return f"\n\nADDITIONAL USER INSTRUCTIONS:\n{custom}"


# ---------------------------------------------------------------------------
# Public entry point
# ---------------------------------------------------------------------------


def build_writer_system_prompt(
    *,
    citation_mode: str,
    citation_profile: Optional[_CitationProfileLike] = None,
    structured_bibliography_enabled: bool = False,
    user_prompt: str = "",
    custom_prompt: str = "",
    external_context: str = "",
) -> str:
    """Compose the writer's system prompt from pure segments.

    Fixed ordering: base + citation + content-block formatting →
    structured bibliography (flagged) → replace mode (prompt-triggered) →
    custom user instructions → external-context trailer.

    Each segment is independently testable. The function does no I/O —
    it's safe to call from tests without a DB or dispatcher.
    """
    parts: list[str] = [
        _BASE_SYSTEM_PROMPT,
        _citation_segment(citation_mode, citation_profile),
        _BLOCK_FORMATTING_SUFFIX,
    ]
    system_prompt = "".join(parts)

    if structured_bibliography_enabled:
        system_prompt += _STRUCTURED_BIBLIOGRAPHY_BLOCK

    if is_replace_mode_prompt(user_prompt):
        system_prompt += _REPLACE_MODE_BLOCK

    system_prompt += _custom_prompt_segment(custom_prompt)
    system_prompt += _external_context_segment(external_context)

    return system_prompt
