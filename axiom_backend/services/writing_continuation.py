"""Transparent section-scoped continuation for the writing agent.

Long-form writing tasks (market reports, academic papers, technical
deliverables) regularly exceed provider output-token ceilings —
DeepSeek-chat caps at 8192, GPT-4o at 16k, Claude Sonnet at 8192 etc.
For a single-turn revision that includes a structured references
block, multi-section prose, and figures, that ceiling is binary:
either the response fits, or it truncates.

Previous approaches and their failure modes:

1. Unbounded continuation with verbatim stitching — garbled mid-word
   joins.
2. "Resume where you stopped" continuation prompts — models ignore
   and restart the entire document block, producing duplicate content
   (observed across DeepSeek + Claude when the partial response is a
   wrapped content-block).
3. Disabled continuation — user sees a truncation warning and has to
   craft a follow-up prompt manually.

The correct approach, implemented here: detect structural cut-points
in the partial response, fire a SECTION-SCOPED continuation prompt
that explicitly names which sections are locked and which remain, and
stitch deterministically. The model can't "restart" when its prompt
only permits section N+1 through end.

This is domain-agnostic: a short academic paper with 5 sections of
400-900 words each, a market research report with 8 thematic
sections, or a technical spec with 12 numbered chapters all work the
same way as long as the writing agent's output follows numbered
`# N. Title` headings. The section-budget hints are optional.

Public entry point: `run_continuations(initial_content, dispatcher,
messages, max_attempts=2, expected_sections=N) -> str`. Returns the
fully-stitched content when it fits, falls back to the partial +
warning when it still doesn't fit after the budget is exhausted.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass
from typing import Any, Awaitable, Callable, Dict, List, Optional, Tuple

from services.writing_i18n import normalize_language_code, t
from services.writing_markdown import (
    SECTION_HEADING_RE as _SECTION_HEADING_RE,
    extract_document_body,
)

logger = logging.getLogger(__name__)


# Cheap token-based language detector — no ML dependency needed for the
# binary de/en decision. Counts characteristic tokens; ties default to
# English (the product's lingua franca). Use `infer_language_code` when
# the caller has no explicit language preference.
_DE_MARKERS = re.compile(
    r"\b(der|die|das|und|nicht|mit|sich|für|auf|aus|zwischen|Wörter|"
    r"Abschnitt|Einleitung|Schlussfolgerung|Ergebnis|Quelle|"
    r"Abbildung|Diagramm|bzw\.|d\.h\.)\b",
    re.IGNORECASE,
)
_EN_MARKERS = re.compile(
    r"\b(the|and|not|with|between|for|from|into|words|section|"
    r"introduction|conclusion|summary|source|figure|diagram|e\.g\.|i\.e\.)\b",
    re.IGNORECASE,
)


def infer_language_code(text: str) -> str:
    """Return 'de' if the text looks majority-German, else 'en'.

    Conservative — ties or empty input → 'en'. Designed for selecting
    the language of a backend-generated instruction message, not for
    user-visible content classification.

    If the input contains a `content-block:document` fence, the
    classifier scores only the body inside that fence — not the
    response as a whole — because the references-block JSON is
    English (`entry_key`, `title`, `reference_type`) and would
    otherwise swamp a German document's marker count.
    """
    if not text:
        return "en"
    # Prefer the document-block body when present; JSON fields outside
    # it are English irrespective of the deliverable's language.
    sample_source = extract_document_body(text) or text
    sample = sample_source[:8000]
    de_hits = len(_DE_MARKERS.findall(sample))
    en_hits = len(_EN_MARKERS.findall(sample))
    return "de" if de_hits > en_hits else "en"


@dataclass
class SectionInfo:
    index: int         # the 1-based number from `# N.`
    title: str         # everything after the number+dot
    start_offset: int  # char offset in document body
    end_offset: int    # exclusive; == len(body) for last section
    words: int
    is_complete: bool  # True if section ends on a full sentence, not mid-word


# ---------------------------------------------------------------------------
# Cut-point analysis
# ---------------------------------------------------------------------------


def _last_real_terminator(text: str) -> Optional[str]:
    """Strip trailing Markdown artifacts / whitespace from `text` and
    return the last character IF it is a real sentence terminator.

    Markdown bold/italic/code fences (`**`, `*`, `_`, `` ` ``) are NOT
    sentence terminators — they're formatting markers. A response that
    ends on `**` is truncated inside bold emphasis, not finished.
    Parentheses/brackets mid-sentence also don't count unless they
    follow a terminator.
    """
    if not text:
        return None
    # Peel off trailing whitespace
    s = text.rstrip()
    # Peel off trailing markdown formatting sigils — these wrap content,
    # they never end a sentence on their own.
    while s and s[-1] in "*_`":
        s = s[:-1].rstrip()
    if not s:
        return None
    last = s[-1]
    if last in ".!?…":
        return last
    # Handle `... ).` / `... ].` — the closing bracket follows after
    # the period. We already peeled formatting; accept if second-to-
    # last is a terminator and last is a closer.
    if last in ")]" and len(s) >= 2 and s[-2] in ".!?…":
        return s[-2]
    return None


def parse_sections(document_body: str) -> List[SectionInfo]:
    """Walk a content-block:document body, return structural info per section.

    A section is "complete" if its last non-whitespace, non-formatting
    character is a real sentence terminator (`.`, `!`, `?`, `…`).
    Markdown sigils (`**`, `*`, `_`, `` ` ``) are stripped before the
    check — they wrap content but don't end sentences. This prevents
    the detector from treating a mid-bold truncation ("Zwei Szenarien**")
    as a complete section.

    Conservative: false-positive "incomplete" is fine (triggers
    redundant continuation), false-negative "complete" is bad (user
    sees a cut sentence).
    """
    if not document_body:
        return []

    matches = list(_SECTION_HEADING_RE.finditer(document_body))
    if not matches:
        return []

    sections: List[SectionInfo] = []
    for i, m in enumerate(matches):
        idx = int(m.group(1))
        title = m.group(2).strip()
        start = m.start()
        end = matches[i + 1].start() if i + 1 < len(matches) else len(document_body)
        body = document_body[m.end():end]
        words = len(body.split())

        is_complete = _last_real_terminator(body) is not None
        sections.append(
            SectionInfo(
                index=idx,
                title=title,
                start_offset=start,
                end_offset=end,
                words=words,
                is_complete=is_complete,
            )
        )
    return sections


# --- Under-budget detection thresholds -------------------------------------
# When the response is structurally complete but a section's body is much
# smaller than its planner-resolved target, fire an expand-mode continuation.
# Two-axis trigger (architect's recommendation):
#   total_actual < total_min * _UNDER_BUDGET_TOTAL_RATIO  → underbudget
#   any section.words < target * _UNDER_BUDGET_SECTION_RATIO  AND
#       target >= _MIN_SECTION_BUDGET_FOR_TRIGGER → underbudget
# The per-section minimum-target guard prevents triggering on tiny sections
# (e.g. 100-word intros where a 35-word shortfall is noise).
_UNDER_BUDGET_TOTAL_RATIO = 0.85
_UNDER_BUDGET_SECTION_RATIO = 0.6
_MIN_SECTION_BUDGET_FOR_TRIGGER = 200


def detect_cut_point(
    content: str,
    expected_sections: int = 5,
    section_budgets: Optional[dict] = None,
    total_word_budget: Optional[Tuple[int, int]] = None,
) -> Optional[dict]:
    """Return structured diagnosis for an incomplete response.

    Returns None when the response looks complete (no continuation
    needed). Returns a dict describing how to continue otherwise:

        {
            "mode": "truncated" | "underbudget",
            "last_complete_section_index": int | None,
            "last_partial_section": SectionInfo | None,
            "missing_section_indices": [int, ...],
            "partial_tail": str,       # last 400 chars of document body
            "expected_sections": int,
            # underbudget mode only:
            "underbudget_section": SectionInfo | None,
            "underbudget_target": int | None,
            "total_actual": int | None,
            "total_target_min": int | None,
        }

    Detection is two-pass:
    1. Structural — missing sections / mid-sentence cuts. Returns mode
       ``truncated`` when triggered.
    2. Word-budget (only when ``section_budgets`` and/or
       ``total_word_budget`` are provided AND the structural pass cleared).
       Picks the **last** numbered section as the expansion target when
       it's underbudget (mid-body insertion is intentionally out of scope
       for this pass; stitch_continuation only handles tail-append safely).
       Returns mode ``underbudget`` when triggered.
    """
    body = extract_document_body(content)
    if body is None:
        return None
    sections = parse_sections(body)
    if not sections:
        return None

    seen = {s.index for s in sections}
    expected = set(range(1, expected_sections + 1))
    missing = sorted(expected - seen)

    complete_indices = [s.index for s in sections if s.is_complete]
    partial_sections = [s for s in sections if not s.is_complete]
    last_complete = max(complete_indices) if complete_indices else None
    last_partial = partial_sections[-1] if partial_sections else None

    # ---- Pass 1: structural truncation ------------------------------------
    if missing or last_partial is not None:
        return {
            "mode": "truncated",
            "last_complete_section_index": last_complete,
            "last_partial_section": last_partial,
            "missing_section_indices": missing,
            "partial_tail": body[-400:] if partial_sections or missing else "",
            "expected_sections": expected_sections,
        }

    # ---- Pass 2: word-budget underrun -------------------------------------
    if not section_budgets and not total_word_budget:
        return None

    total_actual = sum(s.words for s in sections)
    total_target_min: Optional[int] = None
    if total_word_budget:
        try:
            total_target_min = int(total_word_budget[0])
        except (TypeError, ValueError, IndexError):
            total_target_min = None

    total_underrun = (
        total_target_min is not None
        and total_actual < total_target_min * _UNDER_BUDGET_TOTAL_RATIO
    )

    # Pick the last section (highest index) when underbudget. Stitching
    # only supports tail-append; mid-body expansion of section 2 of 5 would
    # require splicing before section 3's heading and is left for future
    # work. The last-section restriction is intentional and conservative.
    last_section = sections[-1] if sections else None
    last_target: Optional[int] = None
    last_under = False
    if section_budgets and last_section is not None:
        last_target = section_budgets.get(last_section.index)
        if (
            last_target is not None
            and last_target >= _MIN_SECTION_BUDGET_FOR_TRIGGER
            and last_section.words < last_target * _UNDER_BUDGET_SECTION_RATIO
        ):
            last_under = True

    if not (total_underrun or last_under):
        return None

    return {
        "mode": "underbudget",
        "last_complete_section_index": last_complete,
        "last_partial_section": None,
        "missing_section_indices": [],
        "partial_tail": body[-400:],
        "expected_sections": expected_sections,
        "underbudget_section": last_section,
        "underbudget_target": last_target,
        "total_actual": total_actual,
        "total_target_min": total_target_min,
    }


# ---------------------------------------------------------------------------
# Scoped continuation prompts
# ---------------------------------------------------------------------------


def build_continuation_prompt(
    cut: dict,
    section_budgets: Optional[dict] = None,
    language_code: str = "en",
) -> str:
    """Produce a section-scoped user prompt for the continuation call.

    Two modes, dispatched by ``cut["mode"]``:
      - ``"truncated"`` — model stopped mid-document. Prompt locks
        finished sections, names the partial / missing ones, and
        instructs seamless resume.
      - ``"underbudget"`` — document is structurally complete but the
        last section came in below the planner's word target. Prompt
        instructs in-place expansion of that section without a new
        heading or re-wrapping.

    The scope is narrow by design: the model gets emit-only-the-tail
    instructions and is told NOT to re-wrap in a content-block:document
    or re-emit the references block.

    ``language_code`` selects the surface language (default "en"; "de"
    for German deliverables).
    """
    mode = cut.get("mode", "truncated")
    if mode == "underbudget":
        return _build_underbudget_prompt(cut, language_code)

    missing = cut["missing_section_indices"]
    partial = cut.get("last_partial_section")
    tail = cut.get("partial_tail") or ""
    last_done = cut.get("last_complete_section_index")

    de = (language_code or "en").lower().startswith("de")

    sections_to_write: List[str] = []
    if partial is not None:
        if de:
            sections_to_write.append(
                f"Abschnitt {partial.index} ('{partial.title}') — aktuell "
                "abgeschnitten, beende ihn nahtlos"
            )
        else:
            sections_to_write.append(
                f"Section {partial.index} ('{partial.title}') — currently "
                "truncated, continue seamlessly to close it"
            )
    for idx in missing:
        budget_hint = ""
        if section_budgets and idx in section_budgets:
            unit = "Wörter" if de else "words"
            label = "Zielbudget" if de else "target budget"
            budget_hint = f" — {label}: {section_budgets[idx]} {unit}"
        kw = "Abschnitt" if de else "Section"
        sections_to_write.append(f"{kw} {idx}{budget_hint}")

    if de:
        locked = (
            f"Abschnitte 1–{last_done} sind bereits fertig und gelockt — NICHT "
            "wiederholen, nicht referenzieren mit 'wie oben erwähnt'."
            if last_done
            else "Abschnitte vor dem abgeschnittenen sind fertig und gelockt."
        )
        return (
            "Deine vorherige Antwort wurde am Token-Limit abgeschnitten.\n\n"
            f"{locked}\n\n"
            f"Zu schreiben sind nur noch:\n- "
            + "\n- ".join(sections_to_write)
            + "\n\n"
            "Letzte 400 Zeichen deiner abgeschnittenen Antwort (damit du "
            "nahtlos weitermachen kannst):\n"
            f"```\n{tail}\n```\n\n"
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
        )

    # English variant
    locked = (
        f"Sections 1–{last_done} are already complete and locked — do NOT "
        "repeat them, do not reference with 'as mentioned above'."
        if last_done
        else "Sections before the truncated one are complete and locked."
    )
    return (
        "Your previous response was cut off at the token limit.\n\n"
        f"{locked}\n\n"
        f"Only the following sections remain to write:\n- "
        + "\n- ".join(sections_to_write)
        + "\n\n"
        "Last 400 characters of your truncated response (so you can resume "
        "seamlessly):\n"
        f"```\n{tail}\n```\n\n"
        "IMPORTANT:\n"
        "- Do NOT wrap your output in a content-block:document fence.\n"
        "- Do NOT emit a content-block:references block (unchanged).\n"
        "- Write ONLY the missing prose, exactly as it will be inserted into "
        "the existing body.\n"
        "- If you're continuing the truncated section: do NOT emit a new "
        "heading. If you're starting new sections: use numbered `# N. Title` "
        "headings as usual.\n"
        "- Do NOT emit a word-count trailer — the backend computes it."
    )


def _build_underbudget_prompt(cut: dict, language_code: str) -> str:
    """User prompt for an in-place expansion of the last numbered section.

    Distinct from the truncation prompt: the section IS structurally
    complete (it ended on a sentence terminator), it's just thinner than
    the planner-resolved budget. The model is asked to deepen the
    analysis with concrete examples / counter-arguments rather than to
    pad with filler. Naming the rhetorical purpose is the architect's
    recommendation — see review for context.
    """
    section = cut.get("underbudget_section")
    target = cut.get("underbudget_target")
    actual = section.words if section is not None else 0
    title = section.title if section is not None else "(unbekannt)"
    idx = section.index if section is not None else 0
    delta = max(0, (target or 0) - actual)
    tail = cut.get("partial_tail") or ""

    de = (language_code or "en").lower().startswith("de")

    if de:
        return (
            "Deine vorherige Antwort ist strukturell vollständig, aber "
            f"Abschnitt {idx} ('{title}') liegt deutlich unter dem "
            f"Wortbudget des Planners ({actual} Wörter, Ziel ~{target}, "
            f"Differenz ~{delta} Wörter).\n\n"
            f"Letzte 400 Zeichen des Body (Anker, damit du im Stil und "
            "Argumentationsfaden bleibst):\n"
            f"```\n{tail}\n```\n\n"
            f"AUFGABE: Erweitere Abschnitt {idx} um ca. {delta} Wörter "
            "AN ORT UND STELLE — vertiefe die Analyse mit konkreten "
            "Beispielen, Gegenargumenten oder zusätzlichen Belegen aus "
            "dem mitgegebenen Literaturportfolio. KEINE Wiederholungen, "
            "KEINE Auflistungen, KEINE Floskeln.\n\n"
            "WICHTIG:\n"
            "- Emit KEINE neue Überschrift — die Sektion existiert "
            "bereits.\n"
            "- Emit KEINEN content-block:document-Wrapper.\n"
            "- Emit KEINEN content-block:references-Block (bleibt "
            "unverändert).\n"
            "- Schreibe NUR den zusätzlichen Prosa-Text, der nahtlos "
            "an das bestehende Sektions-Ende anschließt.\n"
            "- KEINE Wortbilanz/Wordcount-Zeile am Ende — der Backend "
            "rechnet die deterministisch aus.\n"
            "- Setze gegebenenfalls neue (Autor, Jahr, S. XX)-Zitate, "
            "die im Referenzen-Block bereits enthalten sind oder dort "
            "ohnehin gelistet werden müssen."
        )

    return (
        "Your previous response is structurally complete, but "
        f"section {idx} ('{title}') is significantly below the planner's "
        f"word budget ({actual} words, target ~{target}, "
        f"shortfall ~{delta} words).\n\n"
        "Last 400 characters of the body (anchor — match the existing "
        "voice + argument flow):\n"
        f"```\n{tail}\n```\n\n"
        f"TASK: Expand section {idx} in place by roughly {delta} words "
        "— deepen the analysis with concrete examples, counter-arguments, "
        "or additional evidence from the supplied literature portfolio. "
        "No repetition, no bullet lists, no filler.\n\n"
        "IMPORTANT:\n"
        "- Do NOT emit a new heading — the section already exists.\n"
        "- Do NOT wrap your output in a content-block:document fence.\n"
        "- Do NOT emit a content-block:references block (unchanged).\n"
        "- Write ONLY the additional prose, fitting seamlessly onto the "
        "existing section end.\n"
        "- Do NOT emit a word-count trailer — the backend computes it.\n"
        "- Cite (Author, Year, p. XX) only references that are already "
        "in the registry or will be added there."
    )


# ---------------------------------------------------------------------------
# Deterministic stitching
# ---------------------------------------------------------------------------


def stitch_continuation(
    original_content: str,
    continuation_text: str,
    cut: dict,
) -> str:
    """Insert continuation text into the original response at the cut point.

    Strategy:
    1. Extract the document body from the original content.
    2. Strip trailing whitespace from the cut body.
    3. Append the continuation text (with a joining space if the cut
       landed mid-word).
    4. Re-wrap into the original content-block:document fence.
    5. Preserve everything outside the document block (references
       block, Wortbilanz, anything else).

    The continuation text is expected to NOT contain its own
    content-block:document wrapper (per the prompt). If it does
    (LLM disobeys), we strip the wrapper before stitching.
    """
    # Clean the continuation of any wrapper the LLM may have emitted
    # despite our instructions.
    continuation_text = re.sub(
        r"```content-block:document\s*\n", "", continuation_text or ""
    )
    continuation_text = continuation_text.replace("```", "").strip()
    if not continuation_text:
        return original_content

    doc_match = re.search(
        r"(```content-block:document\s*\n)(.*?)(\n```)", original_content or "", re.DOTALL
    )
    if not doc_match:
        # No document block to stitch into — append continuation at end
        return (original_content or "").rstrip() + "\n\n" + continuation_text

    prefix, body, suffix = doc_match.group(1), doc_match.group(2), doc_match.group(3)

    # Cleanup the body's trailing tail if it ended mid-word
    body_stripped = body.rstrip()
    # Join without swallowing a sentence boundary: if body ends on a
    # word-character, insert a space; otherwise no separator needed.
    if body_stripped and body_stripped[-1].isalnum():
        joiner = " "
    elif body_stripped.endswith(("—", "–", "-")):
        # Em-dash / hyphen — the sentence isn't done, continue directly
        joiner = ""
    else:
        joiner = "\n\n"

    new_body = body_stripped + joiner + continuation_text
    updated_doc_block = prefix + new_body + suffix

    return (
        original_content[: doc_match.start()]
        + updated_doc_block
        + original_content[doc_match.end():]
    )


# ---------------------------------------------------------------------------
# Orchestrator
# ---------------------------------------------------------------------------


async def run_continuations(
    *,
    initial_content: str,
    finish_reason: Optional[str],
    base_messages: List[dict],
    dispatcher: Any,
    agent_mode: str = "simplified_writing",
    max_attempts: int = 2,
    expected_sections: int = 5,
    section_budgets: Optional[dict] = None,
    total_word_budget: Optional[Tuple[int, int]] = None,
    language_code: str = "en",
    stats_callback: Optional[Callable[[Any], Awaitable[None]]] = None,
) -> Tuple[str, dict]:
    """Drive section-scoped continuations until the response is complete
    or the budget is exhausted.

    Two trigger paths:
      - Structural truncation (mode ``truncated``) — fires regardless of
        the planner flag, identical behaviour to before.
      - Word-budget underrun (mode ``underbudget``) — only fires when
        ``section_budgets`` and/or ``total_word_budget`` are provided.
        Limits expansion to the last numbered section per attempt;
        ``stitch_continuation`` only safely handles tail-append.

    Returns (final_content, telemetry). Telemetry outcomes:
      - ``not_needed``        — content is structurally complete and
        either no budget signal or budget is met.
      - ``success``           — at least one truncation continuation
        ran and the result is now structurally complete (and
        on-budget if signals provided).
      - ``underbudget_resolved`` — at least one underbudget expansion
        ran and the result is now on-budget.
      - ``still_truncated``   — structural cuts remained after retries.
      - ``underbudget_unfilled`` — attempts exhausted but the body is
        still under the planner's word budget.
      - ``dispatch_error`` / ``empty_response`` / ``empty_text`` —
        provider-side failures.
    """
    telemetry: Dict[str, Any] = {
        "attempts": 0,
        "outcome": "not_needed",
        "initial_finish_reason": finish_reason,
        "sections_missing_initially": 0,
        "sections_missing_final": 0,
        "initial_mode": None,
    }

    content = initial_content or ""

    cut = detect_cut_point(
        content,
        expected_sections,
        section_budgets=section_budgets,
        total_word_budget=total_word_budget,
    )
    if cut is None:
        return content, telemetry

    telemetry["initial_mode"] = cut.get("mode")
    if cut.get("mode") == "truncated":
        telemetry["sections_missing_initially"] = len(cut["missing_section_indices"]) + (
            1 if cut["last_partial_section"] else 0
        )

    while telemetry["attempts"] < max_attempts:
        telemetry["attempts"] += 1
        prompt = build_continuation_prompt(
            cut, section_budgets=section_budgets, language_code=language_code
        )

        cont_messages = list(base_messages) + [
            {"role": "assistant", "content": content},
            {"role": "user", "content": prompt},
        ]
        if cut.get("mode") == "underbudget":
            ub_section = cut.get("underbudget_section")
            logger.info(
                "writing continuation attempt %d/%d: mode=underbudget section=%s "
                "actual=%s target=%s",
                telemetry["attempts"],
                max_attempts,
                ub_section.index if ub_section else None,
                ub_section.words if ub_section else None,
                cut.get("underbudget_target"),
            )
        else:
            logger.info(
                "writing continuation attempt %d/%d: mode=truncated missing=%s partial=%s",
                telemetry["attempts"],
                max_attempts,
                cut["missing_section_indices"],
                cut["last_partial_section"].index if cut["last_partial_section"] else None,
            )

        try:
            response, details = await dispatcher.dispatch(
                messages=cont_messages, agent_mode=agent_mode
            )
        except Exception as exc:
            logger.warning("continuation dispatch failed: %s", exc)
            telemetry["outcome"] = "dispatch_error"
            break

        if stats_callback and details:
            try:
                await stats_callback(details)
            except Exception as exc:
                logger.debug("stats_callback failed: %s", exc)

        if not (response and response.choices):
            telemetry["outcome"] = "empty_response"
            break

        cont_choice = response.choices[0]
        cont_text = (cont_choice.message.content or "").strip()
        if not cont_text:
            telemetry["outcome"] = "empty_text"
            break

        content = stitch_continuation(content, cont_text, cut)
        new_finish = getattr(cont_choice, "finish_reason", None)

        cut = detect_cut_point(
            content,
            expected_sections,
            section_budgets=section_budgets,
            total_word_budget=total_word_budget,
        )
        if cut is None:
            # All cleared — attribute the win to whichever path opened it.
            telemetry["outcome"] = (
                "underbudget_resolved"
                if telemetry["initial_mode"] == "underbudget"
                else "success"
            )
            telemetry["sections_missing_final"] = 0
            return content, telemetry

        if cut.get("mode") == "truncated":
            telemetry["sections_missing_final"] = len(cut["missing_section_indices"]) + (
                1 if cut["last_partial_section"] else 0
            )

        if new_finish != "length":
            # LLM thinks it's done, but cut detector says otherwise
            # — likely it gave us fewer words than we asked for. Try
            # one more round if budget allows.
            continue

    # Attempts exhausted. Pick the outcome string + warning copy that
    # matches what was being chased — truncation vs underbudget have
    # different user-facing failure modes.
    if telemetry["outcome"] == "not_needed":
        telemetry["outcome"] = (
            "underbudget_unfilled"
            if telemetry["initial_mode"] == "underbudget"
            else "still_truncated"
        )

    de = (language_code or "en").lower().startswith("de")
    if telemetry["outcome"] == "underbudget_unfilled":
        if de:
            warning = (
                "\n\n> ⚠️ *Die Antwort ist strukturell vollständig, aber "
                "unter dem geplanten Wortbudget. Mehrere Erweiterungs-Turns "
                "haben das Defizit nicht geschlossen. Für eine vollständige "
                "Hausarbeit bitte gezielt einen Folge-Prompt mit Fokus auf "
                "die zu kurze Sektion senden.*"
            )
        else:
            warning = (
                "\n\n> ⚠️ *The response is structurally complete but below "
                "the planner's word budget. Multiple expansion attempts "
                "could not close the gap. For a full deliverable please "
                "send a focused follow-up prompt for the underweight "
                "section.*"
            )
    else:
        if de:
            warning = (
                "\n\n> ⚠️ *Die Antwort konnte trotz mehrerer Fortsetzungen nicht "
                "vollständig generiert werden. Die persistierten Quellenangaben "
                "bleiben erhalten. Für die fehlenden Abschnitte bitte einen "
                "gezielten Folge-Prompt (ein Abschnitt pro Turn) verwenden.*"
            )
        else:
            warning = (
                "\n\n> ⚠️ *The response could not be fully generated after "
                "multiple continuation attempts. The persisted references are "
                "retained. Please use a focused follow-up prompt (one section "
                "per turn) for the missing sections.*"
            )
    if "Token-Limit" not in content and "token limit" not in content.lower():
        content = content + warning
    return content, telemetry
