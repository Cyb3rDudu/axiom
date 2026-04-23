"""Unit tests for the router-side helpers in SimplifiedWritingAgent.

Covers #42:
- _looks_like_draft_revision: verb + structural patterns
- _build_router_history: summarisation of older assistant turns
- _summarise_assistant_turn: content compaction preserves load-bearing bits

These tests intentionally import only the module-level helpers — they
do not spawn the agent, hit an LLM, or touch the DB.
"""

from __future__ import annotations

import sys
from pathlib import Path

_PROJECT_ROOT = Path(__file__).resolve().parents[3]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

# Prime the codebase's import graph. axiom_backend has a known cycle
# between agents, context_manager, database.async_crud, and api.* —
# main.py sidesteps it by importing api first. Other tests do the same.
import api as _api_primer  # noqa: F401, E402

from ai_researcher.agentic_layer.agents.simplified_writing_agent import (  # noqa: E402
    _build_router_history,
    _looks_like_draft_revision,
    _summarise_assistant_turn,
)


DRAFT = "a" * 500  # any non-trivial draft content


class TestRevisionDetection:
    """_looks_like_draft_revision should trigger on natural revision prompts."""

    @pytest.mark.parametrize(
        "prompt",
        [
            # Original verb list (regression)
            "Kürze diesen Entwurf auf 3000 Wörter.",
            "Überarbeite den letzten Entwurf.",
            "Shrink this draft.",
            "Shorten the following text.",
            "Revise the draft.",
            "Polish the last response.",
            # New verbs added in #42
            "Entferne alle Verweise auf studyflix.",
            "Ersetze Quelle A durch Quelle B.",
            "Tausche die Quellenliste aus.",
            "Streiche das Fazit.",
            "Korrigiere die Tippfehler.",
            "Fixe den Zitierfehler in Kapitel 2.",
            "Nachbessere die Einleitung.",
            "Ergänze die Wortbilanz.",
            "Aktualisiere die China-Daten.",
            "Remove all diplomarbeiten24 references.",
            "Replace source A with source B.",
            "Swap the bibliography entries.",
            "Fix the truncated sentence.",
            # Structural patterns (no verb at head)
            "Vier gezielte Fixes am letzten Entwurf:",
            "Drei kleine Änderungen:",
            "Zwei Swaps:",
            "Liste von Korrekturen:",
            "Three quick fixes for the draft.",
            "A few targeted edits to apply:",
        ],
    )
    def test_revision_verbs_and_patterns_trigger(self, prompt: str) -> None:
        assert _looks_like_draft_revision(prompt, DRAFT) is True

    @pytest.mark.parametrize(
        "prompt",
        [
            # Research / fresh content — must NOT trigger
            "Schreib mir einen Abschnitt über chinesische Handelspolitik.",
            "Recherchiere die aktuelle EU-China-Handelsbilanz.",
            "Finde Quellen zum Thema Dual Circulation.",
            "What are the main arguments in the Heckscher-Ohlin model?",
            "Explain the concept of shrinking demand in section 2.",  # contains "shrinking" mid-sentence
            # Edge — revision verb exists but deep in the prompt
            "In einem vorigen Absatz wurde überarbeitet, wie China seine …"
            * 3,  # verb not in head
        ],
    )
    def test_non_revision_prompts_dont_trigger(self, prompt: str) -> None:
        assert _looks_like_draft_revision(prompt, DRAFT) is False

    def test_requires_draft(self) -> None:
        # Even a clear revision verb does nothing if there's no draft
        assert _looks_like_draft_revision("Kürze das Dokument.", "") is False
        assert _looks_like_draft_revision("Kürze das Dokument.", "short") is False

    def test_requires_prompt(self) -> None:
        assert _looks_like_draft_revision("", DRAFT) is False


class TestRouterHistoryBuilder:
    """_build_router_history bounds the context sent to the router."""

    def test_empty_history(self) -> None:
        assert _build_router_history([], "hello") == []

    def test_user_turns_pass_through_verbatim(self) -> None:
        history = [
            {"role": "user", "content": "first request, important context"},
            {"role": "user", "content": "follow-up"},
        ]
        out = _build_router_history(history, "current")
        assert [m["role"] for m in out] == ["user", "user"]
        assert out[0]["content"] == "first request, important context"

    def test_last_assistant_kept_verbatim(self) -> None:
        long_body = "x" * 5000
        history = [
            {"role": "user", "content": "u1"},
            {"role": "assistant", "content": long_body},
        ]
        out = _build_router_history(history, "current")
        # Last assistant turn stays intact (for "fix what you just said" prompts)
        assert out[-1]["role"] == "assistant"
        assert out[-1]["content"] == long_body

    def test_older_assistants_are_summarised(self) -> None:
        old_body = "Wortbilanz: 3.010 insgesamt\n" + ("y" * 10000)
        recent_body = "z" * 3000
        history = [
            {"role": "user", "content": "u1"},
            {"role": "assistant", "content": old_body},
            {"role": "user", "content": "u2"},
            {"role": "assistant", "content": recent_body},
        ]
        out = _build_router_history(history, "current")
        summarised = out[1]["content"]
        assert len(summarised) < len(old_body)
        assert "summarised assistant turn" in summarised
        # Load-bearing wortbilanz header preserved
        assert "Wortbilanz: 3.010" in summarised
        # Recent assistant still verbatim
        assert out[3]["content"] == recent_body


class TestAssistantTurnSummariser:
    def test_short_content_returned_verbatim(self) -> None:
        short = "Here is a quick reply."
        assert _summarise_assistant_turn(short, cap=200) == short

    def test_preserves_wortbilanz_and_block_types(self) -> None:
        body = (
            "Wortbilanz: 2.910 insgesamt\n"
            "Einleitung (410) · Theorie (520) · ...\n\n"
            "```content-block:document\n" + ("a" * 8000) + "\n```\n\n"
            "```content-block:section\n" + ("b" * 2000) + "\n```\n"
        )
        out = _summarise_assistant_turn(body, cap=400)
        assert "Wortbilanz: 2.910" in out
        assert "document" in out and "section" in out
        assert len(out) < len(body)

    def test_preserves_swap_lines(self) -> None:
        body = (
            "SWAP 1 — alt: (X) → neu: (Y)\n"
            "SWAP 2 — alt: (A) → neu: (B)\n\n"
            "```content-block:document\n" + ("x" * 9000) + "\n```\n"
        )
        out = _summarise_assistant_turn(body, cap=400)
        assert "SWAP 1" in out
        assert "SWAP 2" in out
