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
    # Additional German-language academic publishers
    "franz steiner", "steiner-verlag.de",
    "peter lang", "peterlang.com",
    "transcript verlag", "transcript-verlag.de",
    "beltz", "beltz.de",
    "metropolis verlag", "metropolis-verlag.de",
    "waxmann", "waxmann.com",
    "lit-verlag.de", "lit verlag",
    "vs verlag", "vs-verlag", "vs verlag für sozialwissenschaften",
    # US / North American university presses
    "university of chicago press", "press.uchicago.edu",
    "yale university press", "yalebooks.yale.edu",
    "princeton university press", "press.princeton.edu",
    "harvard university press", "hup.harvard.edu",
    "columbia university press", "cup.columbia.edu",
    "cornell university press", "cornellpress.cornell.edu",
    "duke university press", "dukeupress.edu",
    "stanford university press", "sup.org",
    "penn state university press", "psupress.org",
    "university of pennsylvania press", "upenn.edu/pennpress",
    "university of michigan press", "press.umich.edu",
    "university of california press", "ucpress.edu",
    "university of minnesota press", "upress.umn.edu",
    "university of wisconsin press", "uwpress.wisc.edu",
    "university of texas press", "utpress.utexas.edu",
    "unc press", "uncpress.org",
    "university of toronto press", "utorontopress.com",
    "mcgill-queen's", "mqup.ca",
    # Journal platforms / citation indexes
    "project muse", "muse.jhu.edu",
    "scopus",
    "web of science", "webofscience",
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
    # Additional DACH Leibniz-Wirtschaftsforschung institutes
    "rwi-essen.de", "rwi essen", "rwi – leibniz-institut für wirtschaftsforschung",
    "iwh-halle.de", "iwh halle", "halle institute for economic research",
    "hwwi.org", "hwwi hamburg", "hamburg institute of international economics",
    # Austria — central bank, chamber, fiscal council
    "oenb.at", "oesterreichische nationalbank", "österreichische nationalbank",
    "wko.at", "wirtschaftskammer österreich",
    "fiskalrat.at", "österreichischer fiskalrat",
    "rechnungshof.gv.at", "rechnungshof österreich",
    # Switzerland — central bank + research
    "snb.ch", "swiss national bank", "schweizerische nationalbank",
    "bak-economics.com", "bak economics",
    "iwsb.ch", "iw schweiz",
    # German federal research + regulatory agencies
    "iab.de", "institut für arbeitsmarkt- und berufsforschung",
    "iab-forum.de",
    "bafin.de", "bundesanstalt für finanzdienstleistungsaufsicht",
    "bundeskartellamt.de", "bundeskartellamt",
    "bundesrechnungshof.de", "bundesrechnungshof",
    "umweltbundesamt.de", "german environment agency",
    "rki.de", "robert koch institut",
    "bmwk.de", "bmwi.de", "bmf.de", "bmas.de", "bmj.de", "bmg.de",
    "bmuv.de", "bmvg.de",
    # EU-level research beyond Commission
    "cepii.fr", "cepii",
    "eiopa.europa.eu", "european insurance and occupational pensions",
    "eba.europa.eu", "european banking authority",
    "esma.europa.eu", "european securities and markets authority",
    "europa.eu/eurostat",
    # US — federal statistical & regulatory agencies
    "bea.gov", "bureau of economic analysis",
    "bls.gov", "bureau of labor statistics",
    "census.gov", "us census bureau",
    "cbo.gov", "congressional budget office",
    "crsreports.congress.gov", "congressional research service",
    "treasury.gov", "us treasury",
    "sec.gov", "securities and exchange commission",
    "fdic.gov", "federal deposit insurance corporation",
    "occ.gov", "office of the comptroller",
    "ftc.gov", "federal trade commission",
    "gao.gov", "government accountability office",
    "nsf.gov", "national science foundation",
    "ustr.gov", "office of the us trade representative",
    # US — Federal Reserve system (Board + FRED + regional banks)
    "federalreserve.gov", "federal reserve board",
    "fred.stlouisfed.org", "fred federal reserve economic data",
    "stlouisfed.org", "st. louis fed",
    "newyorkfed.org", "ny.frb.org", "new york fed",
    "bostonfed.org", "boston fed",
    "philadelphiafed.org", "philadelphia fed",
    "clevelandfed.org", "cleveland fed",
    "richmondfed.org", "richmond fed",
    "atlantafed.org", "atlanta fed",
    "chicagofed.org", "chicago fed",
    "minneapolisfed.org", "minneapolis fed",
    "kansascityfed.org", "kansas city fed",
    "dallasfed.org", "dallas fed",
    "frbsf.org", "san francisco fed",
    # US think tanks / policy research
    "aei.org", "american enterprise institute",
    "cato.org", "cato institute",
    "heritage.org", "heritage foundation",
    "hoover.org", "hoover institution",
    "urban.org", "urban institute",
    "pewresearch.org", "pew research center",
    "rff.org", "resources for the future",
    "bipartisanpolicy.org", "bipartisan policy center",
    "americanprogress.org", "center for american progress",
    "cfr.org", "council on foreign relations",
    "milkeninstitute.org", "milken institute",
    "epi.org", "economic policy institute",
    "cbpp.org", "center on budget and policy priorities",
    "taxpolicycenter.org", "tax policy center",
    "taxfoundation.org", "tax foundation",
    "mercatus.org", "mercatus center",
    "manhattan-institute.org", "manhattan institute",
    "cnas.org", "center for a new american security",
    "csis.org", "center for strategic and international studies",
    "bakerinstitute.org", "baker institute",
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
    "taz.de",  # left-leaning but respected broadsheet
    # Austria — broadsheets
    "derstandard.at", "der standard",
    "diepresse.com", "die presse",
    "kurier.at",
    "wienerzeitung.at", "wiener zeitung",
    "trend.at",
    # Switzerland — broadsheets
    "tagesanzeiger.ch", "tages-anzeiger",
    "srf.ch", "schweizer radio und fernsehen",
    # US — business & general press
    "ft.com", "financial times",
    "economist.com",
    "bloomberg",
    "reuters",
    "statista.com",
    "wsj.com", "wall street journal",
    "nytimes.com", "new york times",
    "washingtonpost.com", "washington post",
    "cnbc.com",
    "forbes.com",
    "fortune.com",
    "businessinsider.com",
    "marketwatch.com",
    "barrons.com",
    "axios.com",
    "politico.com",
    "theatlantic.com",
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
    "heute.at",
    "blick.ch",
    "spiegel.de/online",  # the tabloidy sibling — Spiegel proper is tier D
    "tz.de",  # Münchner Boulevard
    "express.de",
    "huffpost.de", "huffingtonpost.de",
    "heise.de/news",
    "tomshardware",
    "fibu-magazin.de",    # tax/financial blog without editorial review
    "finanzkun.de",
    "martinkaessler.com",  # personal blog
    # Common academic-help blogs misused as sources
    "thesiusbettina.com", "ghostwriter-24.com",
    "diplomarbeiten24.de",  # essay mill — blacklist per KMU explicitly
    "studyflix.de", "studysmarter.de", "studyhelp.de", "studyhelper",
    "hausarbeiten.de",  # essay mill
    "lehrer-online.de/blog",
    "grin.com",  # unreviewed student-paper marketplace
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
