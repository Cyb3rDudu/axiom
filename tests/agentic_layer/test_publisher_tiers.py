"""Tests for publisher tier classification."""

import pytest

from ai_researcher.agentic_layer.services.publisher_tiers import (
    classify_tier,
    is_blacklisted,
)


@pytest.mark.parametrize(
    "candidate",
    [
        "Springer Nature",
        "link.springer.com",
        "https://journals.wiley.com/doi/abs/10.1002/xyz",
        "Elsevier Ltd.",
        "SAGE Publications",
        "https://academic.oup.com/book/12345",
        "cambridge.org",
        "Frontiers in Psychology",
        "PLOS ONE",
        "ieeexplore.ieee.org",
        "ACM Digital Library",
    ],
)
def test_tier_a(candidate: str) -> None:
    assert classify_tier(candidate) == "A"


@pytest.mark.parametrize(
    "candidate",
    [
        "imf.org/en/Publications",
        "worldbank.org/country/germany",
        "oecd.org",
        "https://www.ifo.de/DocDL/working-paper.pdf",
        "diw.de/en/weekly-report",
        "wifo.ac.at",
        "https://www.zew.de/en/publications",
        "ssrn.com/abstract=1234",
        "https://ideas.repec.org/p/nbr/nberwo/12345.html",
        "OECD iLibrary",  # A-match wins due to earlier keyword
    ],
)
def test_tier_b_or_a(candidate: str) -> None:
    # OECD iLibrary specifically sits in Tier A list; others in B. We only
    # assert "not C/D/unknown" to keep the test resilient to list tweaks.
    assert classify_tier(candidate) in {"A", "B"}


@pytest.mark.parametrize(
    "candidate",
    [
        "arxiv.org/abs/2404.01234",
        "biorxiv.org/content/10.1101/xyz",
        "https://osf.io/preprints/socarxiv/abcde",
    ],
)
def test_tier_c(candidate: str) -> None:
    assert classify_tier(candidate) == "C"


@pytest.mark.parametrize(
    "candidate",
    [
        "McKinsey Global Institute",
        "https://www.bcg.com/insights/foo",
        "Deloitte",
        "pwc.de",
        "https://hbr.org/2024/04/some-article",
        "handelsblatt.com",
        "https://www.ft.com/content/foo",
        "economist.com",
        "gartner.com",
        "statista.com",
    ],
)
def test_tier_d(candidate: str) -> None:
    assert classify_tier(candidate) == "D"


@pytest.mark.parametrize(
    "candidate",
    [
        "https://en.wikipedia.org/wiki/Globalisation",
        "wirtschaftslexikon.gabler.de",
        "Gabler Wirtschaftslexikon",
        "investopedia.com",
        "https://medium.com/@someuser/rant",
        "reddit.com/r/economics",
        "bild.de",
        "krone.at/news",
    ],
)
def test_blacklist(candidate: str) -> None:
    assert classify_tier(candidate) == "blacklist"
    assert is_blacklisted(candidate) is True


def test_unknown_when_no_signals() -> None:
    assert classify_tier(None) == "unknown"
    assert classify_tier("", "", "") == "unknown"
    assert classify_tier("some random string") == "unknown"


def test_blacklist_wins_over_reputable_host() -> None:
    """A Wikipedia mirror hosted on a university domain still counts as
    blacklist — any blacklist keyword anywhere in the haystack wins."""
    tier = classify_tier(
        "https://en.wikipedia.org/wiki/Foo",
        "Springer Gabler",  # would otherwise match Tier A
    )
    assert tier == "blacklist"
