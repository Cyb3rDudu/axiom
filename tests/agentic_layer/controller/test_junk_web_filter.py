# Prime axiom_backend's import graph (matches the other controller tests).
import api as _api_primer  # noqa: F401  # isort: skip

import unittest

from ai_researcher.agentic_layer.agents.research_agent import (
    _is_obvious_junk_web_result,
    _OBVIOUS_JUNK_WEB_HOSTS,
    _MIN_WEB_SNIPPET_CHARS,
)


class TestJunkWebResultFilter(unittest.TestCase):
    """Fix #2b: deterministic pre-filter for obviously useless web results so
    no LLM note-generation call is wasted on junk (observed for NexMach:
    soccerway.com, zhihu.com, langenscheidt dictionary, etc.)."""

    def test_junk_hosts_dropped(self):
        for host in [
            "https://us.soccerway.com/team/burnley/z3dmTMMO/",
            "https://www.zhihu.com/question/2053058575203656580",
            "https://en.langenscheidt.com/german-english/aufbau",
            "https://support.microsoft.com/en-gb/contactus",
            "https://support.google.com/translate/?hl=en",
        ]:
            result = {"url": host, "content": "x" * 500}
            is_junk, reason = _is_obvious_junk_web_result(result)
            self.assertTrue(is_junk, f"should drop {host} ({reason})")
            self.assertIn("junk host", reason)

    def test_too_short_snippet_dropped(self):
        # The junk mission had 74-150 char snippets from nav/error pages.
        result = {"url": "https://example.org/page", "content": "x" * 120}
        is_junk, reason = _is_obvious_junk_web_result(result)
        self.assertTrue(is_junk)
        self.assertIn("too short", reason)

    def test_short_snippet_under_threshold_dropped(self):
        result = {"url": "https://example.org/page", "snippet": "short"}
        is_junk, _ = _is_obvious_junk_web_result(result)
        self.assertTrue(is_junk)

    def test_legitimate_academic_source_kept(self):
        # A real, content-rich source from a non-junk host must pass.
        result = {
            "url": "https://link.springer.com/chapter/10.1007/978-3-662-63105-8_3",
            "content": "PESTEL analysis is a framework for analyzing the macro-environmental "
                       "factors affecting an organisation, encompassing Political, Economic, "
                       "Social, Technological, Environmental and Legal dimensions." * 3,
        }
        is_junk, reason = _is_obvious_junk_web_result(result)
        self.assertFalse(is_junk, f"should keep legitimate source ({reason})")

    def test_kept_when_content_long_enough_and_host_clean(self):
        result = {"url": "https://example.org/pestel", "content": "y" * _MIN_WEB_SNIPPET_CHARS}
        is_junk, _ = _is_obvious_junk_web_result(result)
        self.assertFalse(is_junk)

    def test_wikipedia_not_in_junk_list_handled_separately(self):
        # Wikipedia is filtered by the existing explicit check in the loop,
        # NOT by this helper — confirm the helper itself does not touch it
        # (so the two filters compose without surprises).
        result = {"url": "https://en.wikipedia.org/wiki/PEST_analysis", "content": "x" * 500}
        is_junk, _ = _is_obvious_junk_web_result(result)
        self.assertFalse(is_junk)

    def test_missing_fields_safe(self):
        # No url, no content — must not raise, and should be dropped (too short).
        is_junk, reason = _is_obvious_junk_web_result({})
        self.assertTrue(is_junk)
        self.assertIn("too short", reason)

    def test_junk_host_takes_precedence_over_long_content(self):
        # Even with a long snippet, a junk host is dropped.
        result = {"url": "https://www.reddit.com/r/anything", "content": "z" * 5000}
        is_junk, reason = _is_obvious_junk_web_result(result)
        self.assertTrue(is_junk)
        self.assertIn("junk host", reason)


if __name__ == "__main__":
    unittest.main()
