"""DeepSeek V4 thinking-mode policy tests.

Reviews (round 5) found that the thinking-disabled policy only covered 3 of 6
content-generation agent modes, and the streaming dispatch path did not apply
the policy at all. These tests lock in:

  1. EVERY user-visible prose mode disables thinking (so reasoning tokens can't
     eat a tight word-budget max_tokens and return empty content).
  2. The SHARED builder applies the same policy to streaming AND non-streaming.
  3. Reasoning-heavy modes keep thinking enabled.
  4. caller_cap is applied as a ceiling.
"""

# Prime axiom_backend's import graph (matches the other controller tests).
import api as _api_primer  # noqa: F401  # isort: skip

import unittest

from ai_researcher.agentic_layer.model_dispatcher import (
    _V4_CONTENT_GENERATION_MODES,
    _v4_thinking_for_mode,
    _build_deepseek_v4_params,
)


class TestThinkingPolicyPerMode(unittest.TestCase):
    def test_content_generation_modes_disable_thinking(self):
        for mode in _V4_CONTENT_GENERATION_MODES:
            self.assertEqual(
                _v4_thinking_for_mode(mode), "disabled",
                f"{mode} is a content-generation mode and MUST disable thinking",
            )

    def test_writing_disabled(self):
        self.assertEqual(_v4_thinking_for_mode("writing"), "disabled")

    def test_simplified_writing_disabled(self):
        # Review issue: this prose/revision mode was missing from the original
        # set and could return empty content on a thinking model.
        self.assertEqual(_v4_thinking_for_mode("simplified_writing"), "disabled")

    def test_writing_content_generator_disabled(self):
        # Review issue: writing_tools.propose_and_add_paragraph used this mode.
        self.assertEqual(_v4_thinking_for_mode("writing_content_generator"), "disabled")

    def test_writing_planner_disabled(self):
        # Review issue: EnhancedCollaborativeWritingAgent outline/plan prose.
        self.assertEqual(_v4_thinking_for_mode("writing_planner"), "disabled")

    def test_query_preparation_disabled(self):
        self.assertEqual(_v4_thinking_for_mode("query_preparation"), "disabled")

    def test_research_disabled(self):
        self.assertEqual(_v4_thinking_for_mode("research"), "disabled")

    def test_reasoning_modes_keep_thinking_enabled(self):
        for mode in ("planning", "reflection", "query_strategy",
                     "router", "messenger", "note_assignment", "verifier"):
            self.assertEqual(
                _v4_thinking_for_mode(mode), "enabled",
                f"{mode} is reasoning-heavy and should keep thinking enabled",
            )

    def test_unknown_mode_defaults_enabled(self):
        # A typo'd/unknown mode is NOT silently treated as content-generation.
        self.assertEqual(_v4_thinking_for_mode("something_new"), "enabled")


class TestSharedBuilder(unittest.TestCase):
    """The shared builder is used by BOTH the streaming and non-streaming
    dispatch paths — this is what guarantees the streaming V4 path receives the
    thinking policy (review round 5, issue 2)."""

    def test_content_mode_params_disable_thinking(self):
        for mode in _V4_CONTENT_GENERATION_MODES:
            params = _build_deepseek_v4_params(
                "deepseek-v4-flash", [{"role": "user", "content": "x"}],
                caller_cap=256, agent_mode=mode,
            )
            self.assertEqual(
                params["extra_body"], {"thinking": {"type": "disabled"}},
                f"{mode}: extra_body must disable thinking",
            )
            # caller_cap applied as ceiling against v4_max (65536).
            self.assertEqual(params["max_tokens"], 256)

    def test_reasoning_mode_params_enable_thinking(self):
        params = _build_deepseek_v4_params(
            "deepseek-v4-pro", [{"role": "user", "content": "x"}],
            caller_cap=None, agent_mode="planning",
        )
        self.assertEqual(params["extra_body"], {"thinking": {"type": "enabled"}})

    def test_streaming_carries_thinking_policy(self):
        # Review issue 2: the streaming branch must ALSO apply the thinking
        # toggle, not just model/messages/max_tokens/stream.
        params = _build_deepseek_v4_params(
            "deepseek-v4-flash", [{"role": "user", "content": "x"}],
            caller_cap=256, agent_mode="writing", stream=True,
        )
        self.assertTrue(params.get("stream"), "stream must be True")
        self.assertIn("extra_body", params, "streaming must include thinking toggle")
        self.assertEqual(params["extra_body"], {"thinking": {"type": "disabled"}})
        self.assertEqual(params["max_tokens"], 256)

    def test_streaming_reasoning_mode_enabled(self):
        params = _build_deepseek_v4_params(
            "deepseek-v4-flash", [{"role": "user", "content": "x"}],
            caller_cap=None, agent_mode="reflection", stream=True,
        )
        self.assertTrue(params["stream"])
        self.assertEqual(params["extra_body"], {"thinking": {"type": "enabled"}})

    def test_no_caller_cap_uses_v4_max(self):
        params = _build_deepseek_v4_params(
            "deepseek-v4-flash", [{"role": "user", "content": "x"}],
            caller_cap=None, agent_mode="writing",
        )
        # v4_max default = 65536.
        self.assertEqual(params["max_tokens"], 65536)

    def test_non_streaming_has_no_stream_key(self):
        params = _build_deepseek_v4_params(
            "deepseek-v4-flash", [{"role": "user", "content": "x"}],
            caller_cap=None, agent_mode="writing", stream=False,
        )
        self.assertNotIn("stream", params)


if __name__ == "__main__":
    unittest.main()
