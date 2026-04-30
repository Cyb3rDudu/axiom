"""Tests for the response-postprocess passes:
- synthesize_sources_block
- validate_figure_urls
"""

from __future__ import annotations

import sys
from pathlib import Path
from types import SimpleNamespace

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402
import api as _api_primer  # noqa: F401, E402

from services.response_postprocess import (  # noqa: E402
    synthesize_sources_block,
    validate_figure_urls,
)


def _make_ref(key, year=2024, title="Some Title", url=None, publisher=None,
              authors=None, reference_type="web"):
    return SimpleNamespace(
        entry_key=key,
        authors=authors or [{"family": "Author", "given": "X."}],
        year=year,
        title=title,
        container_title=None,
        publisher=publisher,
        pages=None,
        url=url,
        web_url=None,
        accessed_at=None,
        doi=None,
        reference_type=reference_type,
    )


class TestSynthesizeSourcesBlock:
    def test_empty_registry_noop(self):
        content = "Some prose with no refs."
        updated, tele = synthesize_sources_block(content, [])
        assert updated == content
        assert tele["action"] == "no_registry"
        assert tele["registry_count"] == 0

    def test_replaces_llm_emitted_block_with_registry(self):
        refs = [_make_ref("dst-2024", url="https://destatis.de")]
        content = (
            "```content-block:references\n"
            '[{"entry_key": "wrong", "title": "Wrong"}]\n'
            "```\n\n"
            "```content-block:document\nBody\n```"
        )
        updated, tele = synthesize_sources_block(content, refs)
        assert "wrong" not in updated.lower()
        assert "dst-2024" in updated
        assert tele["action"] in ("replaced_corrected", "replaced_equivalent")
        assert tele["registry_count"] == 1

    def test_prepends_when_no_fence_present(self):
        refs = [_make_ref("a-2024"), _make_ref("b-2023")]
        content = "```content-block:document\n# 1. Body\n\nProse.\n```"
        updated, tele = synthesize_sources_block(content, refs)
        # Fence appears before the document block
        refs_idx = updated.find("content-block:references")
        doc_idx = updated.find("content-block:document")
        assert 0 <= refs_idx < doc_idx
        assert tele["action"] == "prepended"

    def test_equivalent_block_still_replaces(self):
        refs = [_make_ref("a-2024")]
        # LLM emitted same entry_key — action should be replaced_equivalent
        content = (
            "```content-block:references\n"
            '[{"entry_key": "a-2024", "title": "X"}]\n'
            "```"
        )
        updated, tele = synthesize_sources_block(content, refs)
        assert tele["action"] == "replaced_equivalent"
        assert "a-2024" in updated

    def test_malformed_llm_fence_still_overridden(self):
        refs = [_make_ref("a-2024")]
        content = (
            "```content-block:references\n"
            "not even valid json\n"
            "```"
        )
        updated, tele = synthesize_sources_block(content, refs)
        assert tele["action"] == "replaced_malformed"
        assert "a-2024" in updated

    def test_entries_sorted_by_key(self):
        refs = [_make_ref("z-2024"), _make_ref("a-2020"), _make_ref("m-2022")]
        content = "just prose"
        updated, _ = synthesize_sources_block(content, refs)
        a_idx = updated.find("a-2020")
        m_idx = updated.find("m-2022")
        z_idx = updated.find("z-2024")
        assert 0 <= a_idx < m_idx < z_idx

    def test_skips_refs_without_entry_key(self):
        refs = [_make_ref("a-2024"), SimpleNamespace(entry_key=None)]
        content = "prose"
        updated, tele = synthesize_sources_block(content, refs)
        assert tele["registry_count"] == 1
        assert "a-2024" in updated


class TestValidateFigureUrls:
    def test_leaves_valid_urls_alone(self):
        content = "![Chart](/api/documents/images/doc-1/fig.png)"
        updated, tele = validate_figure_urls(
            content, {"/api/documents/images/doc-1/fig.png"}
        )
        assert updated == content
        assert tele["figures_resolved"] == 1
        assert tele["figures_placeholder"] == 0

    def test_flags_placeholder_paths(self):
        content = "![Chart](placeholder-fig1.png)"
        updated, tele = validate_figure_urls(content)
        assert "about:blank#figure-not-resolved" in updated
        assert tele["figures_placeholder"] == 1
        assert tele["figures_resolved"] == 0

    def test_flags_example_com_paths(self):
        content = "![Chart](https://example.com/chart.png)"
        updated, tele = validate_figure_urls(content)
        assert "about:blank" in updated
        assert tele["figures_placeholder"] == 1

    def test_flags_invalid_when_set_provided_and_miss(self):
        content = "![Chart](/api/documents/images/doc-2/other.png)"
        updated, tele = validate_figure_urls(
            content, {"/api/documents/images/doc-1/fig.png"}
        )
        # Valid shape but not in the known set → counted as invalid
        assert "about:blank" in updated
        assert tele["figures_invalid"] == 1

    def test_preserves_alt_text(self):
        content = "![Chinas BIP](placeholder-fig1.png)"
        updated, _ = validate_figure_urls(content)
        assert "![Chinas BIP](" in updated

    def test_counts_multiple_figures(self):
        content = (
            "![A](placeholder-a.png)\n"
            "![B](/api/documents/images/doc/b.png)\n"
            "![C](example.com/c.png)\n"
        )
        _, tele = validate_figure_urls(
            content, {"/api/documents/images/doc/b.png"}
        )
        assert tele["figures_total"] == 3
        assert tele["figures_resolved"] == 1
        assert tele["figures_placeholder"] == 2

    def test_empty_content_noop(self):
        updated, tele = validate_figure_urls("")
        assert updated == ""
        assert tele["figures_total"] == 0


class TestTrustedExternalImageUrls:
    """Citable institutional sources (Destatis, IWF, IEA, KPMG, …) may
    legitimately appear as figure URLs when the user pastes them into
    the writing prompt for the agent to copy verbatim. The validator
    must accept these without flipping them to about:blank, while
    still rejecting hallucinated URLs from random hosts.
    """

    def test_accepts_kpmg_klardenker_chart(self):
        url = (
            "https://klardenker.kpmg.de/wp-content/uploads/2025/03/"
            "250305-Aussenhandel-1.ai_-1-e1763042397568.png"
        )
        content = f"![Außenhandel D-China]({url})"
        updated, tele = validate_figure_urls(content)
        assert updated == content
        assert tele["figures_resolved"] == 1

    def test_accepts_globaleconomy_dynamic_chart(self):
        # PHP-served PNG — no .png extension but graph_country.php is
        # a known chart-serving idiom we whitelist.
        url = "https://www.theglobaleconomy.com/graph_country.php?p=0&c=China&i=current_account"
        content = f"![Leistungsbilanz]({url})"
        updated, tele = validate_figure_urls(content)
        assert updated == content
        assert tele["figures_resolved"] == 1

    def test_accepts_iea_charts_path(self):
        url = "https://www.iea.org/data-and-statistics/charts/some-chart"
        content = f"![Tech share]({url})"
        updated, tele = validate_figure_urls(content)
        assert updated == content
        assert tele["figures_resolved"] == 1

    def test_accepts_wikimedia_commons(self):
        url = "https://upload.wikimedia.org/wikipedia/commons/a/aa/China_GDP.png"
        content = f"![BIP]({url})"
        updated, tele = validate_figure_urls(content)
        assert updated == content
        assert tele["figures_resolved"] == 1

    def test_rejects_random_host_with_png_extension(self):
        url = "https://random-host.example/figure.png"
        content = f"![Chart]({url})"
        updated, tele = validate_figure_urls(content)
        assert "about:blank" in updated
        # placeholder-keyword regex catches `example.com|.org`, but
        # `random-host.example` doesn't match — should land in invalid
        assert tele["figures_invalid"] == 1 or tele["figures_placeholder"] == 1

    def test_rejects_http_to_trusted_host(self):
        # Plain http (not https) → rejected to avoid mixed-content
        # warnings in the editor.
        url = "http://destatis.de/some-chart.png"
        content = f"![Chart]({url})"
        updated, tele = validate_figure_urls(content)
        assert "about:blank" in updated

    def test_rejects_trusted_host_without_image_path_hint(self):
        # iea.org but pointing at a press-release HTML path with no
        # image-shaped suffix nor known chart-serving idiom → reject.
        url = "https://www.iea.org/news/some-press-release"
        content = f"![Bogus]({url})"
        updated, tele = validate_figure_urls(content)
        assert "about:blank" in updated

    def test_subdomains_of_trusted_apex_pass(self):
        # Apex domain `kpmg.de` is trusted; a subdomain like
        # `klardenker.kpmg.de` should pass automatically.
        url = "https://klardenker.kpmg.de/wp-content/uploads/2025/01/foo.png"
        assert validate_figure_urls(f"![X]({url})")[1]["figures_resolved"] == 1
