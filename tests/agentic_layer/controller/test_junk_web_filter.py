# Prime axiom_backend's import graph (matches the other controller tests).
import api as _api_primer  # noqa: F401  # isort: skip

import unittest

from ai_researcher.agentic_layer.agents.research_agent import (
    _is_obvious_junk_web_result,
    _is_junk_web_host,
    _classify_web_result,
    _OBVIOUS_JUNK_WEB_HOSTS,
    _MIN_WEB_SNIPPET_CHARS,
)


class TestJunkWebHostHardFilter(unittest.TestCase):
    """The URL HOST is a hard, deterministic drop (social/shopping/navigation
    sites can never yield usable research material). Applied before any LLM
    call in BOTH ResearchAgent and SimplifiedWritingAgent."""

    def test_junk_hosts_dropped(self):
        for host in [
            "https://us.soccerway.com/team/burnley/z3dmTMMO/",
            "https://www.zhihu.com/question/2053058575203656580",
            "https://en.langenscheidt.com/german-english/aufbau",
            "https://support.microsoft.com/en-gb/contactus",
            "https://support.google.com/translate/?hl=en",
        ]:
            self.assertIsNotNone(_is_junk_web_host(host), f"should hard-drop {host}")

    def test_junk_host_in_result_dict_dropped(self):
        result = {"url": "https://www.reddit.com/r/anything", "content": "z" * 5000}
        is_junk, reason = _is_obvious_junk_web_result(result)
        self.assertTrue(is_junk)
        self.assertIn("junk host", reason)

    def test_clean_host_not_dropped(self):
        self.assertIsNone(_is_junk_web_host("https://link.springer.com/chapter/x"))

    def test_substring_lookalike_not_dropped(self):
        """Review round 6, issue 1: matching must be on the real host, not a
        substring, so a domain that merely CONTAINS a junk host string is kept.
        The old ``host in url`` check wrongly dropped these."""
        # 'notreddit.com' contains 'reddit.com' as a substring but is a
        # different domain — must NOT be dropped.
        self.assertIsNone(_is_junk_web_host("https://notreddit.com/page"))
        self.assertIsNone(_is_junk_web_host("https://www.notreddit.com/page"))
        # 'notamazon.com' must not match the 'amazon.' wildcard.
        self.assertIsNone(_is_junk_web_host("https://notamazon.com/x"))
        # 'fx.com' must not match 'x.com'.
        self.assertIsNone(_is_junk_web_host("https://fx.com/x"))

    def test_real_subdomain_is_dropped(self):
        """Conversely, a genuine subdomain of a junk host IS dropped."""
        self.assertIsNotNone(_is_junk_web_host("https://www.reddit.com/r/x"))
        self.assertIsNotNone(_is_junk_web_host("https://us.soccerway.com/team/x"))

    def test_schemeless_url_handled(self):
        self.assertIsNone(_is_junk_web_host("notreddit.com"))
        self.assertIsNotNone(_is_junk_web_host("www.reddit.com/r/x"))

    def test_marketplace_wildcard_matches_across_tlds(self):
        """The 'amazon.' / 'ebay.' wildcards match the SLD across any TLD and
        under any subdomain."""
        for url in [
            "https://amazon.com/dp/1",
            "https://amazon.de/dp/1",
            "https://www.amazon.co.uk/dp/1",
            "https://www.ebay.de/itm/1",
        ]:
            self.assertIsNotNone(_is_junk_web_host(url), f"should drop {url}")

    def test_bing_translator_path_filtered_but_bing_search_kept(self):
        """'bing.com/translator' is path-qualified: only the translator is junk,
        not all of bing.com."""
        self.assertIsNotNone(_is_junk_web_host("https://www.bing.com/translator?text=x"))
        self.assertIsNone(_is_junk_web_host("https://www.bing.com/search?q=x"))

    def test_empty_url_safe(self):
        self.assertIsNone(_is_junk_web_host(""))
        self.assertIsNone(_is_junk_web_host(None))


class TestShortSnippetIsWeakSignal(unittest.TestCase):
    """A short search-engine snippet from a CLEAN host is NOT a hard drop: the
    full page behind it may still be valuable, so the fetch-then-evaluate flow
    must get a chance to recover it. (Review issue: a legitimate academic
    source with a short snippet but a valuable full page was being discarded
    permanently.)"""

    def test_short_snippet_from_clean_host_not_junk(self):
        # Was previously hard-dropped; now kept so it can be fetched.
        result = {"url": "https://example.org/page", "content": "x" * 120}
        is_junk, _ = _is_obvious_junk_web_result(result)
        self.assertFalse(is_junk)

    def test_short_snippet_classified_as_weak_signal(self):
        result = {"url": "https://example.org/page", "snippet": "short"}
        classification, reason = _classify_web_result(result)
        self.assertEqual(classification, "short")
        self.assertIn("fetch", reason.lower())

    def test_long_snippet_classified_ok(self):
        result = {"url": "https://example.org/pestel", "content": "y" * _MIN_WEB_SNIPPET_CHARS}
        classification, _ = _classify_web_result(result)
        self.assertEqual(classification, "ok")

    def test_junk_host_short_snippet_still_hard_dropped(self):
        # Junk host takes precedence even with a short snippet.
        result = {"url": "https://www.amazon.de/dp/123", "snippet": "x"}
        classification, reason = _classify_web_result(result)
        self.assertEqual(classification, "junk")
        self.assertIn("junk host", reason)


class TestLegitimateSources(unittest.TestCase):
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
        classification, _ = _classify_web_result(result)
        self.assertEqual(classification, "ok")

    def test_wikipedia_not_in_junk_list_handled_separately(self):
        # Wikipedia is filtered by the existing explicit check in the loop,
        # NOT by this helper — confirm the helper itself does not touch it.
        result = {"url": "https://en.wikipedia.org/wiki/PEST_analysis", "content": "x" * 500}
        self.assertFalse(_is_obvious_junk_web_result(result)[0])
        self.assertIsNone(_is_junk_web_host(result["url"]))

    def test_missing_fields_safe(self):
        # No url, no content — must not raise.
        self.assertFalse(_is_obvious_junk_web_result({})[0])
        self.assertIsNone(_is_junk_web_host(None))


if __name__ == "__main__":
    unittest.main()
