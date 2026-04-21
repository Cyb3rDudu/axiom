"""Map URLs/domains to canonical institution names for APA-style citations.

When a web source has no explicit ``authors`` metadata, the writing agent
used to fall back to the page title or the raw URL as the author in the
in-text citation — producing `(https://www.bpb.de/..., o. S.)` style
citations that violate KMU APA 7 Rule 12 (*use the publishing
institution as author when no personal author is known*).

This module provides a single entry point ``organization_for_url`` that
returns a short, canonical German/DACH-leaning institution name when
the URL is from a recognized source, plus ``short_domain`` as a bare
fallback. Together they let the writing agent replace raw URLs with
``(BPB, 2024, o. S.)`` / ``(bpb.de, 2024, o. S.)`` shapes.

Only add entries for sources the user is likely to cite — broad web
crawler noise should just fall through to the domain short form.
"""

from __future__ import annotations

import re
from typing import Any, Optional
from urllib.parse import urlparse


# Canonical institution names, keyed by host suffix (everything after
# the optional ``www.``/subdomain prefix). Longest-suffix wins so that
# ``research.ifo.de`` beats a generic ``ifo.de`` entry if both were here.
_DOMAIN_TO_INSTITUTION: dict[str, str] = {
    # ---- German/DACH political & research institutions ----
    "bpb.de": "BPB",
    "bundesbank.de": "Deutsche Bundesbank",
    "bundesregierung.de": "Bundesregierung",
    "bundestag.de": "Deutscher Bundestag",
    "bmwk.de": "BMWK",
    "bmwi.de": "BMWK",  # legacy name
    "bmf.de": "BMF",
    "bmbf.de": "BMBF",
    "bmz.de": "BMZ",
    "destatis.de": "Destatis",
    "dihk.de": "DIHK",
    "ifo.de": "ifo Institut",
    "diw.de": "DIW Berlin",
    "iwh-halle.de": "IWH Halle",
    "iwkoeln.de": "IW Köln",
    "iw-koeln.de": "IW Köln",
    "ifw-kiel.de": "IfW Kiel",
    "wsi.de": "WSI",
    "boeckler.de": "Hans-Böckler-Stiftung",
    "sachverstaendigenrat-wirtschaft.de": "Sachverständigenrat",
    "swp-berlin.org": "SWP Berlin",
    "dgap.org": "DGAP",
    "giga-hamburg.de": "GIGA",
    "kas.de": "Konrad-Adenauer-Stiftung",
    "fes.de": "Friedrich-Ebert-Stiftung",
    "boell.de": "Heinrich-Böll-Stiftung",
    "freiheit.org": "Friedrich-Naumann-Stiftung",
    "hss.de": "Hanns-Seidel-Stiftung",
    "merics.org": "MERICS",
    "kfw.de": "KfW",
    # Austria
    "oenb.at": "Oesterreichische Nationalbank",
    "wifo.ac.at": "WIFO",
    "iwm.at": "IWM Wien",
    "bmaw.gv.at": "BMAW",
    "parlament.gv.at": "Österreichisches Parlament",
    "wko.at": "WKÖ",
    "statistik.at": "Statistik Austria",
    # Switzerland
    "snb.ch": "Schweizerische Nationalbank",
    "admin.ch": "Schweizer Bundesverwaltung",
    "seco.admin.ch": "SECO",
    "kof.ethz.ch": "KOF ETH Zürich",
    "bfs.admin.ch": "Bundesamt für Statistik",

    # ---- International economic bodies ----
    "imf.org": "IMF",
    "worldbank.org": "World Bank",
    "oecd.org": "OECD",
    "oecd-ilibrary.org": "OECD",
    "wto.org": "WTO",
    "bis.org": "BIS",
    "un.org": "United Nations",
    "unctad.org": "UNCTAD",
    "undp.org": "UNDP",
    "europa.eu": "Europäische Kommission",
    "ecb.europa.eu": "EZB",
    "eea.europa.eu": "EEA",
    "eurostat.ec.europa.eu": "Eurostat",

    # ---- Academic publishers / databases ----
    "springer.com": "Springer",
    "link.springer.com": "Springer",
    "wiley.com": "Wiley",
    "onlinelibrary.wiley.com": "Wiley",
    "elsevier.com": "Elsevier",
    "sciencedirect.com": "Elsevier",
    "tandfonline.com": "Taylor & Francis",
    "sagepub.com": "SAGE",
    "cambridge.org": "Cambridge University Press",
    "oup.com": "Oxford University Press",
    "jstor.org": "JSTOR",
    "ssrn.com": "SSRN",
    "papers.ssrn.com": "SSRN",
    "nber.org": "NBER",
    "repec.org": "RePEc",
    "ideas.repec.org": "RePEc",
    "econstor.eu": "EconStor",
    "openalex.org": "OpenAlex",
    "doi.org": "DOI",
    "arxiv.org": "arXiv",

    # ---- Major German/DACH media ----
    "faz.net": "FAZ",
    "faz.de": "FAZ",
    "sueddeutsche.de": "Süddeutsche Zeitung",
    "zeit.de": "Die Zeit",
    "handelsblatt.com": "Handelsblatt",
    "wiwo.de": "WirtschaftsWoche",
    "manager-magazin.de": "Manager Magazin",
    "spiegel.de": "Der Spiegel",
    "welt.de": "Die Welt",
    "tagesschau.de": "Tagesschau",
    "deutschlandfunk.de": "Deutschlandfunk",
    "dw.com": "Deutsche Welle",
    "nzz.ch": "Neue Zürcher Zeitung",
    "derstandard.at": "Der Standard",
    "diepresse.com": "Die Presse",

    # ---- Major international media (likely to appear in China coverage) ----
    "ft.com": "Financial Times",
    "economist.com": "The Economist",
    "nytimes.com": "New York Times",
    "wsj.com": "Wall Street Journal",
    "bloomberg.com": "Bloomberg",
    "reuters.com": "Reuters",
    "bbc.com": "BBC",
    "bbc.co.uk": "BBC",

    # ---- Consultancies / think tanks commonly cited on China ----
    "mckinsey.com": "McKinsey & Company",
    "bcg.com": "Boston Consulting Group",
    "rolandberger.com": "Roland Berger",
    "pwc.com": "PwC",
    "deloitte.com": "Deloitte",
    "kpmg.com": "KPMG",
    "ey.com": "EY",
    "weforum.org": "World Economic Forum",
    "brookings.edu": "Brookings Institution",
    "rand.org": "RAND Corporation",
    "cfr.org": "Council on Foreign Relations",
    "chathamhouse.org": "Chatham House",
    "piie.com": "Peterson Institute",

    # ---- China-specific sources (state media + stats) ----
    "stats.gov.cn": "National Bureau of Statistics of China",
    "pbc.gov.cn": "People's Bank of China",
    "mofcom.gov.cn": "MOFCOM",
    "fmprc.gov.cn": "Chinese Foreign Ministry",
    "xinhuanet.com": "Xinhua",
    "chinadaily.com.cn": "China Daily",
    "scmp.com": "South China Morning Post",
}


def _host_from_url(url: str) -> Optional[str]:
    """Return the lowercased bare host of ``url`` (no ``www.``), or None."""
    if not url:
        return None
    # Tolerate bare domain input (urlparse returns it as path when no scheme).
    if "://" not in url:
        url = "http://" + url
    try:
        parsed = urlparse(url)
    except Exception:
        return None
    host = (parsed.hostname or "").lower().strip()
    if not host or "." not in host:
        # Real domains always have a TLD (dot); reject "not a url at all",
        # stray localhost-style names, and whitespace-only input.
        return None
    if host.startswith("www."):
        host = host[4:]
    return host or None


def short_domain(url: str) -> Optional[str]:
    """Return a short ``bpb.de`` style host for ``url``.

    Strips the scheme, userinfo, path, and leading ``www.`` — so
    ``https://www.bpb.de/themen/asien/china/…`` becomes ``bpb.de``.
    Returns None when the input can't be parsed as a URL.
    """
    return _host_from_url(url)


def organization_for_url(url: str) -> Optional[str]:
    """Return a canonical institution name for ``url`` if we know one.

    Matches by longest host suffix so that subdomains (``kof.ethz.ch``,
    ``ecb.europa.eu``) win over their parent domains. Returns None when
    the host isn't in the curated map — callers should fall back to
    ``short_domain`` rather than the raw URL.
    """
    host = _host_from_url(url)
    if not host:
        return None

    # Exact match first.
    hit = _DOMAIN_TO_INSTITUTION.get(host)
    if hit:
        return hit

    # Longest-suffix match: try progressively shorter dotted suffixes.
    parts = host.split(".")
    for i in range(1, len(parts)):
        suffix = ".".join(parts[i:])
        hit = _DOMAIN_TO_INSTITUTION.get(suffix)
        if hit:
            return hit
    return None


# Anything that looks like a URL inside a citation parenthesis is almost
# always a bug (see KMU APA 7 Rule 12 — prefer institution over URL).
_URL_IN_CITATION = re.compile(r"https?://|www\.")


def looks_like_raw_url_author(candidate: str) -> bool:
    """Return True when ``candidate`` is clearly a raw URL, not a name.

    Used as a defensive last check when formatting a citation — if the
    caller somehow ended up with a URL as the 'author' field, the
    writing agent should downgrade to the domain short form.
    """
    if not candidate:
        return False
    return bool(_URL_IN_CITATION.search(candidate))


def web_citation_author(authors: Any, url: Optional[str]) -> str:
    """Return the best 'author' string for an APA-style web citation.

    Priority:
      1. Explicit ``authors`` metadata (dropped if it's obviously a URL
         — happens when upstream put the URL in the authors slot).
      2. Curated organization mapping for the URL's host (e.g.
         ``bpb.de`` → ``BPB``, ``diw.de`` → ``DIW Berlin``).
      3. The bare host as a short domain (``bpb.de``).
      4. Last-resort: the string ``Unbekannte Quelle`` — we deliberately
         do NOT fall back to the raw URL because that was the Run 1 bug
         (``(https://www.bpb.de/…, o. S.)`` violates KMU APA 7 Rule 12).
    """
    if authors:
        if isinstance(authors, (list, tuple)):
            candidate = ", ".join(str(a).strip() for a in authors if a)
        else:
            candidate = str(authors).strip()
        if candidate and not looks_like_raw_url_author(candidate):
            return candidate

    if url:
        org = organization_for_url(url)
        if org:
            return org
        host = short_domain(url)
        if host:
            return host

    return "Unbekannte Quelle"
