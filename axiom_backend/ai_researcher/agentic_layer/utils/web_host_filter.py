# ai_researcher/agentic_layer/utils/web_host_filter.py
"""Shared web-host junk filter.

A LEAF utility module: it imports only the standard library so it can be safely
imported by BOTH ResearchAgent and SimplifiedWritingAgent without crossing the
agent import graph (a direct cross-agent import such as
``simplified_writing_agent -> research_agent`` previously created a circular
import — ``ImportError: partially initialized module`` — that only worked after
``import api`` had pre-loaded the full graph).

What lives here
---------------
* ``JUNK_WEB_HOSTS``: explicit list of junk hosts (social / shopping /
  navigation / dictionary / support sites that can never yield usable academic
  research material). Domains are listed EXPLICITLY (no wildcards) so a wildcard
  can never hit an unrelated registrable domain (review round 7, issue 3: the
  old ``amazon.`` wildcard matched ``amazon.example.com``).
* ``host_or_subdomain``: exact-domain-or-real-subdomain predicate. Real host
  matching via ``urlparse().hostname`` — NOT a substring (review round 7, issue
  1: ``'reddit.com' in url`` wrongly matched ``notreddit.com``).
* ``is_junk_web_host``: hard, deterministic pre-filter on the URL host.

The snippet-length "weak signal" classification is research-specific and stays
in ``research_agent``; only the host parser is shared here.
"""

from __future__ import annotations

from typing import Optional, Tuple
from urllib.parse import urlparse


# Domains / patterns that never yield usable research material for an academic /
# business-analysis mission. A web result whose URL host matches one of these is
# dropped BEFORE any LLM note-generation call is spent on it.
#
# Matching semantics (see ``is_junk_web_host``):
#   * a bare host (``reddit.com``) matches the exact domain OR a real subdomain
#     (``www.reddit.com``), but NOT a substring look-alike (``notreddit.com``);
#   * a path-qualified host (``bing.com/translator``) matches the host AND the
#     exact path / its sub-paths (``/translator``, ``/translator/...``), but not
#     ``/translatorfoo``;
#   * marketplace domains are listed EXPLICITLY per TLD (no wildcard) so they
#     cannot hit foreign registrable domains.
JUNK_WEB_HOSTS: Tuple[str, ...] = (
    # social / community
    "facebook.com", "instagram.com", "tiktok.com",
    "twitter.com", "x.com", "youtube.com",
    "pinterest.com", "reddit.com", "zhihu.com",
    # marketplaces — explicit TLDs (no wildcard, review round 7 issue 3)
    "amazon.com", "amazon.de", "amazon.co.uk", "amazon.at", "amazon.fr",
    "amazon.it", "amazon.es", "amazon.nl", "amazon.ca", "amazon.com.au",
    "amazon.com.br", "amazon.com.mx", "amazon.co.jp", "amazon.in", "amazon.sg",
    "amazon.pl", "amazon.se", "amazon.ae", "amazon.sa", "amazon.com.tr",
    "ebay.com", "ebay.de", "ebay.co.uk", "ebay.at", "ebay.fr", "ebay.it",
    "ebay.es", "ebay.nl", "ebay.ca", "ebay.com.au", "ebay.ch", "ebay.be",
    "ebay.ie", "ebay.pl", "aliexpress.com",
    # dictionaries / support / translate utility pages
    "langenscheidt.com", "support.microsoft.com", "support.google.com",
    "translate.google.com",
    # sports / non-academic
    "soccerway.com",
    # path-qualified: only this specific path is junk (not all of bing.com)
    "bing.com/translator",
)


def host_or_subdomain(hostname: str, domain: str) -> bool:
    """``hostname`` is exactly ``domain`` OR a real subdomain of it.

    ``'reddit.com'`` matches ``www.reddit.com`` but NOT ``notreddit.com`` — the
    previous substring check (``'reddit.com' in url``) wrongly matched the
    latter (review round 7, issue 1).
    """
    return hostname == domain or hostname.endswith("." + domain)


def is_junk_web_host(url: str) -> Optional[Tuple[str, str]]:
    """Hard, deterministic pre-filter on the URL HOST (exact domain or a real
    subdomain — never a bare substring).

    Returns ``(entry, reason)`` when the URL host is an obvious non-academic /
    navigational / shopping / social site, else ``None``. This is a HARD drop
    applied before any LLM call — such hosts have no value regardless of
    snippet length.
    """
    if not url:
        return None
    # urlparse needs a scheme; tolerate schemeless inputs like 'notreddit.com'.
    candidate = url if "://" in url else "//" + url
    try:
        parsed = urlparse(candidate)
    except Exception:
        return None
    hostname = (parsed.hostname or "").lower()
    if not hostname:
        return None
    path = (parsed.path or "").lower()

    for entry in JUNK_WEB_HOSTS:
        e = entry.lower()
        if "/" in e:
            # path-qualified entry, e.g. 'bing.com/translator'. Match the host
            # AND the exact path or a real sub-path — NOT a prefix, so
            # '/translatorfoo' does not match '/translator' (review round 7,
            # issue 2).
            host_part, _, path_part = e.partition("/")
            if not host_part or not path_part:
                continue
            path_match = (
                path == "/" + path_part
                or path.startswith("/" + path_part + "/")
            )
            if path_match and host_or_subdomain(hostname, host_part):
                return entry, "junk host ({})".format(entry)
        else:
            # bare host: exact domain or real subdomain. NOT a substring.
            if host_or_subdomain(hostname, e):
                return entry, "junk host ({})".format(entry)
    return None
