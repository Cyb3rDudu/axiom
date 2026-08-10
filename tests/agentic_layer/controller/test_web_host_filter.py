"""Cycle-proof test for the shared web-host filter (review round 7, issue 1).

This test imports the filter utility with NO ``import api`` primer and NO
research_agent import. Before the fix, importing simplified_writing_agent pulled
in research_agent directly and raised
``ImportError: partially initialized module`` — a cross-agent circular import
that the other tests only passed because ``import api`` had pre-loaded the full
graph (masking the bug).

The filter now lives in a LEAF utility module (stdlib-only imports), so this
standalone import must succeed and the matcher must behave correctly.
"""

# Deliberately NO `import api` primer here.
import unittest

from ai_researcher.agentic_layer.utils.web_host_filter import (
    is_junk_web_host,
    host_or_subdomain,
    JUNK_WEB_HOSTS,
)


class TestUtilIsCleanLeaf(unittest.TestCase):
    """The util must import without priming the full app graph (no cycle)."""

    def test_imports_without_api_primer(self):
        # If this test collected and ran, the import at module top succeeded
        # with no api primer — the cycle is gone.
        self.assertTrue(callable(is_junk_web_host))
        self.assertTrue(callable(host_or_subdomain))
        self.assertGreater(len(JUNK_WEB_HOSTS), 10)

    def test_filter_source_is_the_util_not_an_agent(self):
        self.assertEqual(is_junk_web_host.__module__,
                         "ai_researcher.agentic_layer.utils.web_host_filter")


class TestHostMatchingBoundaries(unittest.TestCase):
    """Review round 7: real host matching, path boundary, no wildcard."""

    def test_substring_lookalike_not_dropped(self):
        # Issue 1: 'reddit.com' must not match 'notreddit.com'.
        self.assertIsNone(is_junk_web_host("https://notreddit.com/x"))
        self.assertIsNone(is_junk_web_host("https://www.notreddit.com/x"))
        self.assertIsNone(is_junk_web_host("https://notamazon.com/x"))
        self.assertIsNone(is_junk_web_host("https://fx.com/x"))

    def test_real_subdomain_dropped(self):
        self.assertIsNotNone(is_junk_web_host("https://www.reddit.com/r/x"))
        self.assertIsNotNone(is_junk_web_host("https://us.soccerway.com/x"))

    def test_foreign_domain_not_hit_by_marketplace_entry(self):
        # Issue 3: explicit domains only — amazon.example.com is NOT amazon.
        self.assertIsNone(is_junk_web_host("https://amazon.example.com/x"))
        self.assertIsNone(is_junk_web_host("https://ebay.example.com/x"))
        # but real marketplace TLDs are dropped.
        self.assertIsNotNone(is_junk_web_host("https://amazon.de/dp/1"))
        self.assertIsNotNone(is_junk_web_host("https://www.amazon.co.uk/dp/1"))
        self.assertIsNotNone(is_junk_web_host("https://www.ebay.de/itm/1"))

    def test_path_boundary_not_prefix(self):
        # Issue 2: '/translator' exact or '/translator/...' only.
        self.assertIsNotNone(is_junk_web_host("https://www.bing.com/translator?x=1"))
        self.assertIsNotNone(is_junk_web_host("https://www.bing.com/translator/sub"))
        # '/translatorfoo' must NOT match.
        self.assertIsNone(is_junk_web_host("https://www.bing.com/translatorfoo"))
        # bing SEARCH is not the translator -> kept.
        self.assertIsNone(is_junk_web_host("https://www.bing.com/search?q=x"))

    def test_clean_and_edge_inputs(self):
        self.assertIsNone(is_junk_web_host("https://link.springer.com/chapter/x"))
        self.assertIsNone(is_junk_web_host("https://en.wikipedia.org/wiki/PEST"))
        self.assertIsNone(is_junk_web_host("notreddit.com"))
        self.assertIsNone(is_junk_web_host(""))
        self.assertIsNone(is_junk_web_host(None))

    def test_host_or_subdomain_predicate(self):
        self.assertTrue(host_or_subdomain("www.reddit.com", "reddit.com"))
        self.assertTrue(host_or_subdomain("reddit.com", "reddit.com"))
        self.assertFalse(host_or_subdomain("notreddit.com", "reddit.com"))
        self.assertFalse(host_or_subdomain("evilreddit.com", "reddit.com"))


if __name__ == "__main__":
    unittest.main()
