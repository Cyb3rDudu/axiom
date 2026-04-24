"""
Curated publisher / institution tier classification used by the
Literaturportfolio workflow.

Lookup is intentionally substring-based on normalised strings — the incoming
metadata (URL domain, publisher name, journal title, DOI prefix) is messy,
so exact-match would miss too many sources. We normalise to lowercase and
strip common noise before matching.

Extensions should add entries here rather than introducing per-mission
configuration — drift in this list is acceptable; the agent always sees the
computed tier plus the raw publisher string, so it can sanity-check.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import List, Literal, Optional


Tier = Literal["A", "B", "C", "D", "blacklist", "unknown"]


@dataclass(frozen=True)
class TierDefinition:
    tier: Tier
    keywords: List[str] = field(default_factory=list)
    """Normalised substrings to match against publisher / URL / journal."""


# Tier A — established scientific publishers. Peer-review is the norm, though
# we still verify via metadata when possible.
_TIER_A: List[str] = [
    "springer", "springer nature", "nature publishing", "naturenature",
    "springer gabler", "springer vs", "springer fachmedien",
    "link.springer",  # DOI path for Springer chapters
    "wiley", "blackwell",
    "elsevier", "sciencedirect",
    "sage",
    "taylor & francis", "taylorandfrancis", "routledge", "tandfonline",
    "oxford university press", "oup.com", "academic.oup",
    "cambridge university press", "cambridge.org",
    "mit press",
    "ieee", "ieee xplore", "ieeexplore",
    "acm", "dl.acm.org",
    "emerald",
    "palgrave",
    "de gruyter", "degruyter",
    "nomos",
    # German academic / university-press publishers
    "schäffer-poeschel", "schaeffer-poeschel", "schaffer poeschel",
    "vahlen", "beck.de", "beck-online", "beck eLibrary", "c.h.beck",
    "duncker & humblot", "duncker&humblot",
    "mohr siebeck", "mohr-siebeck",
    "campus verlag",
    "utb-shop", "utb.de",
    "hanser", "carl hanser",
    "haufe lexware", "haufe-lexware", "haufe.de",
    "pearson education",
    # Swiss / Austrian academic presses
    "vdf hochschulverlag", "vdf.ethz.ch", "vdf-hochschulverlag",
    "böhlau", "boehlau", "böhlau verlag",
    "facultas", "facultas.at",
    "linde verlag", "linde.at",
    # Indexes / aggregators
    "jstor",
    "oecd ilibrary",  # peer-reviewed OECD working series
    "american psychological association", "apa.org",
    "american economic association",
    "frontiers in",
    "plos",
    "bmc ",
    "mdpi",  # controversial, but peer-reviewed; agent can flag
]

# Tier B — reputable research institutions, central banks, standards bodies,
# official statistics. Not strictly peer-reviewed but authoritative.
_TIER_B: List[str] = [
    "imf.org", "international monetary fund",
    "worldbank.org", "world bank",
    "oecd.org",
    "unctad.org", "un.org", "united nations",
    "wto.org",
    "bis.org", "bank for international settlements",
    "ecb.europa.eu", "european central bank",
    "ec.europa.eu", "european commission",
    "jrc.ec.europa.eu",
    "eurostat.ec.europa.eu", "eurostat",
    "destatis", "statistik austria", "statistik.at", "bfs.admin",
    "bundesbank.de", "deutsche bundesbank",
    "ifo.de", "ifo institute",
    "diw.de", "diw berlin",
    "iwkoeln", "iwkoeln.de", "iw köln", "iw koeln",
    "wifo.ac.at", "wifo",
    "ihs.ac.at",
    "zew.de", "zew mannheim",
    "ifw-kiel.de", "kielinstitut.de", "ifw kiel", "kiel institute",
    "wsi.de", "wirtschafts- und sozialwissenschaftliches institut",
    "bpb.de", "bundeszentrale für politische bildung",
    "bmwi.de", "bmwk.de", "bmwe.de", "bundesministerium",
    "auswaertiges-amt.de", "auswärtiges amt",
    "kfw.de", "kfw research",
    "swp-berlin.org", "stiftung wissenschaft und politik",
    "dgap.org", "dgap.de",
    "giga-hamburg.de", "giga hamburg",
    "boell.de", "heinrich-böll-stiftung",  # politische Stiftungen
    "bruegel.org",
    "piie.com", "peterson institute",
    "csis.org",
    "merics.org",
    "rhodium group",
    "brookings",
    "rand.org",
    "nber.org",
    "cepr.org",
    "ssrn.com",  # working papers (peer-review uncertain but reputable)
    "repec.org", "ideas.repec",
    "iso.org",
    "din.de",
    "bmbf.de", "dfg.de", "deutsche forschungsgemeinschaft",
    # Wirtschaftsberatung / Sachverständige
    "sachverstaendigenrat-wirtschaft.de", "sachverständigenrat",
    "sachverstandigenrat", "svr-wirtschaft.de",
    # EU institutions beyond Commission / Parliament / ECB
    "europarl.europa.eu", "european parliament", "europäisches parlament",
    "consilium.europa.eu", "rat der eu",
    "eca.europa.eu", "european court of auditors",
    # Think tanks / research networks
    "cer.eu", "centre for european reform",
    "ceps.eu", "centre for european policy studies",
    "epc.eu", "european policy centre",
    "chathamhouse.org", "chatham house",
    "iiss.org", "international institute for strategic studies",
    "kof.ethz.ch", "kof ethz", "kof konjunkturforschungsstelle",
    "seco.admin.ch", "seco bern",
    # Scholarly repositories (peer-review mixed, but authoritative hosts)
    "econstor.eu",
    "cesifo.org", "cesifo-group",
    "vfst.de",  # Verein für Socialpolitik
]

# Tier C — scientific-ish but unverified (generic preprints, unknown
# academic publishers, edited collections we can't classify).
_TIER_C: List[str] = [
    "arxiv.org",
    "biorxiv.org",
    "chemrxiv.org",
    "researchsquare",
    "preprints.org",
    "osf.io",
]

# Tier D — practitioner / grey literature. Useful as "Praxisquelle" per KMU
# rules, but counts against the ≥ 50 % scientific share.
_TIER_D: List[str] = [
    "mckinsey", "bcg.com", "boston consulting",
    "deloitte", "pwc.", "kpmg", "ey.com", "ernst & young",
    "accenture",
    "gartner", "forrester",
    "harvard business review", "hbr.org",
    "handelsblatt", "wiwo.de", "wirtschaftswoche",
    "faz.net", "nzz.ch",
    "sueddeutsche.de", "sueddeutsche",
    "fr.de", "frankfurter rundschau",
    "spiegel.de", "spiegel online",
    "zeit.de", "die zeit",
    "welt.de",
    "manager-magazin.de", "manager magazin",
    "ft.com", "financial times",
    "economist.com",
    "bloomberg",
    "reuters",
    "statista.com",
]

# Blacklist — disallowed as primary scientific source per KMU "Dos and
# Don'ts" handout (Wikipedia, Gabler Wirtschaftslexikon, Tageszeitungen
# ausdrücklich genannt). Agent must surface as warnings, not silently include.
_BLACKLIST: List[str] = [
    "wikipedia.org",
    "wirtschaftslexikon.gabler", "gabler wirtschaftslexikon",
    "bwl-lexikon.de",  # similar lexicon-style site
    "wirtschaftslexikon24.com",
    "economics-online.co.uk",
    "scribbr.de", "scribbr.com",  # academic-help blog, not a source
    "investopedia",
    "medium.com",
    "quora.com",
    "reddit.com",
    "stackexchange",
    "substack.com",
    "linkedin.com/pulse",
    "bild.de",
    "kronen zeitung", "krone.at",
    "oe24.at",
    "blick.ch",
    "spiegel.de/online",  # the tabloidy sibling — Spiegel proper is tier D
    "heise.de/news",
    "tomshardware",
    "fibu-magazin.de",    # tax/financial blog without editorial review
    "finanzkun.de",
    "martinkaessler.com",  # personal blog
]


def _normalise(s: Optional[str]) -> str:
    if not s:
        return ""
    return s.lower().strip().replace("  ", " ")


def classify_tier(*candidates: Optional[str]) -> Tier:
    """Return the tier for a source given one or more candidate strings
    (publisher name, journal title, URL, DOI prefix). The *strongest* tier
    wins — blacklist > A > B > C > D > unknown. That way a Wikipedia mirror
    hosted at a reputable university's domain still ends up flagged.
    """
    haystack = " ".join(_normalise(c) for c in candidates if c)
    if not haystack:
        return "unknown"

    for kw in _BLACKLIST:
        if kw in haystack:
            return "blacklist"
    for kw in _TIER_A:
        if kw in haystack:
            return "A"
    for kw in _TIER_B:
        if kw in haystack:
            return "B"
    for kw in _TIER_C:
        if kw in haystack:
            return "C"
    for kw in _TIER_D:
        if kw in haystack:
            return "D"
    return "unknown"


def is_blacklisted(*candidates: Optional[str]) -> bool:
    return classify_tier(*candidates) == "blacklist"
