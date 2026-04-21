"""
Metadata enrichment pipeline for Axiom research platform.

Provides external metadata lookups via CrossRef, OpenLibrary, OpenAlex, and DOI.org
to supplement LLM-based metadata extraction. Includes web page metadata extraction
from HTML using Schema.org JSON-LD, Dublin Core, and Open Graph tags.
"""

import asyncio
import re
import json
import logging
from typing import Optional, List, Dict, Any
from difflib import SequenceMatcher

import httpx

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Identifier patterns
# ---------------------------------------------------------------------------

DOI_PATTERN = re.compile(r'\b(10\.\d{4,9}/[^\s]+)\b')
ISBN_PATTERN = re.compile(r'ISBN[-:]?\s*((?:97[89][-\s]?)?(?:\d[-\s]?){9}[\dXx])')
ARXIV_PATTERN = re.compile(r'(?:arXiv:)?(\d{4}\.\d{4,5}(?:v\d+)?)')

def normalize_doi(doi: str) -> str:
    """Normalize a DOI to its canonical bare form (e.g., '10.xxxx/yyyy').

    Strips URL prefixes like https://doi.org/, http://dx.doi.org/, etc.
    """
    if not doi:
        return doi
    doi = doi.strip()
    for prefix in ("https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/"):
        if doi.lower().startswith(prefix):
            doi = doi[len(prefix):]
            break
    return doi


# Common user-agent for polite pool access
_USER_AGENT = "Axiom/1.0 (mailto:admin@axiom.local)"

# Shared httpx client – created lazily, reused across calls
_http_client: Optional[httpx.AsyncClient] = None


def _get_client() -> httpx.AsyncClient:
    """Return a module-level async HTTP client (created once, reused)."""
    global _http_client
    if _http_client is None or _http_client.is_closed:
        _http_client = httpx.AsyncClient(
            timeout=httpx.Timeout(10.0, connect=5.0),
            follow_redirects=True,
            headers={"User-Agent": _USER_AGENT},
        )
    return _http_client


async def _get_with_retry(
    url: str,
    *,
    max_retries: int = 2,
    backoff_base: float = 1.0,
    label: str = "",
) -> Optional[httpx.Response]:
    """GET ``url`` with retry on timeout / transient 5xx / connection errors.

    Lookups to CrossRef/OpenLibrary/OpenAlex routinely see spurious 10 s
    timeouts during heavy ingest windows — one retry with short backoff
    avoids losing the entire enrichment for a single network blip (observed
    on Woeckener_Mikrooekonomik DOI 10.1007/978-3-642-36897-4). Retries
    happen inline and never block longer than ~3 s total on the happy path.
    """
    last_exc: Optional[Exception] = None
    for attempt in range(max_retries + 1):
        try:
            resp = await _get_client().get(url)
            # Retry on transient server errors; let 4xx propagate to the caller.
            if resp.status_code >= 500 and attempt < max_retries:
                logger.info(
                    f"{label or url}: HTTP {resp.status_code} "
                    f"(attempt {attempt + 1}/{max_retries + 1}), retrying"
                )
                await asyncio.sleep(backoff_base * (2 ** attempt))
                continue
            return resp
        except (httpx.TimeoutException, httpx.ConnectError, httpx.ReadError) as exc:
            last_exc = exc
            if attempt < max_retries:
                delay = backoff_base * (2 ** attempt)
                logger.info(
                    f"{label or url}: {type(exc).__name__} "
                    f"(attempt {attempt + 1}/{max_retries + 1}), retry in {delay:.1f}s"
                )
                await asyncio.sleep(delay)
                continue
            logger.warning(
                f"{label or url}: {type(exc).__name__} after "
                f"{max_retries + 1} attempts – {exc}"
            )
            return None
    if last_exc is not None:
        logger.warning(f"{label or url}: exhausted retries – {last_exc}")
    return None


# ---------------------------------------------------------------------------
# Identifier detection
# ---------------------------------------------------------------------------

def detect_identifiers(text: str) -> dict:
    """Detect DOI, ISBN, and arXiv IDs in *text*.

    Returns a dict with keys ``doi``, ``isbn``, ``arxiv`` whose values are
    the first matched identifier string, or ``None`` if not found.
    """
    result: Dict[str, Optional[str]] = {"doi": None, "isbn": None, "arxiv": None}

    doi_match = DOI_PATTERN.search(text)
    if doi_match:
        # Strip trailing punctuation and normalize URL prefix
        doi = doi_match.group(1).rstrip(".,;:)")
        result["doi"] = normalize_doi(doi)

    isbn_matches = ISBN_PATTERN.findall(text)
    if isbn_matches:
        # Prefer ISBN-13 (978-...) over ISBN-10, and prefer last occurrence (current edition)
        isbn13s = [re.sub(r'[-\s]', '', m) for m in isbn_matches if m.replace('-', '').replace(' ', '').startswith('978')]
        if isbn13s:
            result["isbn"] = isbn13s[-1]  # Last ISBN-13 = most likely current edition
        else:
            result["isbn"] = re.sub(r'[-\s]', '', isbn_matches[-1])

    arxiv_match = ARXIV_PATTERN.search(text)
    if arxiv_match:
        result["arxiv"] = arxiv_match.group(1)

    return result


# ---------------------------------------------------------------------------
# CrossRef (DOI → metadata)
# ---------------------------------------------------------------------------

async def lookup_crossref(doi: str) -> Optional[dict]:
    """Look up a DOI via the CrossRef API (free, no auth).

    Returns a dict matching the Axiom metadata schema, or ``None`` on failure.
    The returned dict may carry two private keys — ``_isbn_print`` and
    ``_isbn_electronic`` — for callers that want to drive an OpenLibrary
    fallback with the print variant. Both are stripped before persistence.
    """
    url = f"https://api.crossref.org/works/{doi}"
    resp = await _get_with_retry(url, label=f"CrossRef {doi}")
    if resp is None:
        return None
    if resp.status_code == 404:
        logger.info(f"CrossRef lookup for DOI {doi}: not found (404)")
        return None
    try:
        resp.raise_for_status()
        data = resp.json()
        item = data.get("message", {})

        # Authors
        authors = []
        for author in item.get("author", []):
            given = author.get("given", "")
            family = author.get("family", "")
            name = f"{given} {family}".strip()
            if name:
                authors.append(name)

        # Publication year from date-parts
        year = None
        for date_field in ("published-print", "published-online", "issued", "created"):
            parts = item.get(date_field, {}).get("date-parts", [[]])
            if parts and parts[0] and parts[0][0]:
                year = int(parts[0][0])
                break

        # Journal / container title
        container = item.get("container-title", [])
        journal = container[0] if container else None

        # Title
        title_list = item.get("title", [])
        title = title_list[0] if title_list else None

        # Publisher
        publisher = item.get("publisher")

        # ISBN — split print vs electronic when CrossRef discloses it. Prefer
        # print for the canonical field since OpenLibrary indexes print
        # editions more reliably than eBooks.
        isbn_print: Optional[str] = None
        isbn_electronic: Optional[str] = None
        for entry in item.get("isbn-type", []):
            val = entry.get("value")
            if not val:
                continue
            if entry.get("type") == "print" and not isbn_print:
                isbn_print = val
            elif entry.get("type") == "electronic" and not isbn_electronic:
                isbn_electronic = val
        isbn_list = item.get("ISBN", [])
        canonical_isbn = isbn_print or isbn_electronic or (isbn_list[0] if isbn_list else None)

        result = {
            "title": title,
            "authors": authors if authors else None,
            "publication_year": year,
            "journal_or_source": journal,
            "publisher": publisher,
            "isbn": canonical_isbn,
            "_isbn_print": isbn_print,
            "_isbn_electronic": isbn_electronic,
            "doi": normalize_doi(item.get("DOI") or doi or ""),
            "document_type": "paper",
        }
        logger.info(f"CrossRef lookup for DOI {doi}: found")
        return result

    except Exception as exc:
        logger.warning(f"CrossRef lookup for DOI {doi}: parse error – {exc}")
        return None


# ---------------------------------------------------------------------------
# OpenLibrary (ISBN → metadata)
# ---------------------------------------------------------------------------

async def lookup_openlibrary(isbn: str) -> Optional[dict]:
    """Look up an ISBN via OpenLibrary (free, no auth).

    Fetches the book record and resolves author names from author keys.
    Returns a dict matching the Axiom metadata schema, or ``None`` on failure.
    """
    url = f"https://openlibrary.org/isbn/{isbn}.json"
    resp = await _get_with_retry(url, label=f"OpenLibrary {isbn}")
    if resp is None:
        return None
    if resp.status_code == 404:
        logger.info(f"OpenLibrary lookup for ISBN {isbn}: not found (404)")
        return None
    try:
        resp.raise_for_status()
        book = resp.json()
        client = _get_client()

        # Resolve author names
        authors = []
        for author_ref in book.get("authors", []):
            key = author_ref.get("key")
            if key:
                try:
                    author_resp = await client.get(f"https://openlibrary.org{key}.json")
                    if author_resp.status_code == 200:
                        author_data = author_resp.json()
                        name = author_data.get("name") or author_data.get("personal_name")
                        if name:
                            authors.append(name)
                except Exception:
                    pass  # skip unresolvable authors

        # Publication year
        year = None
        publish_date = book.get("publish_date", "")
        if publish_date:
            year_match = re.search(r'\b(\d{4})\b', publish_date)
            if year_match:
                year = int(year_match.group(1))

        publishers = book.get("publishers", [])

        result = {
            "title": book.get("title"),
            "authors": authors if authors else None,
            "publication_year": year,
            "publisher": publishers[0] if publishers else None,
            "isbn": isbn,
            "page_count": book.get("number_of_pages"),
            "document_type": "book",
        }
        logger.info(f"OpenLibrary lookup for ISBN {isbn}: found")
        return result

    except Exception as exc:
        logger.warning(f"OpenLibrary lookup for ISBN {isbn}: parse error – {exc}")
        return None


# ---------------------------------------------------------------------------
# OpenAlex (title search → metadata)
# ---------------------------------------------------------------------------

def _title_similarity(a: str, b: str) -> float:
    """Return a 0-1 similarity score between two title strings."""
    return SequenceMatcher(None, a.lower().strip(), b.lower().strip()).ratio()


async def lookup_openalex(title: str, authors: Optional[List[str]] = None) -> Optional[dict]:
    """Search for a work by title via OpenAlex (free, no auth).

    Picks the best match by title similarity from the first page of results.
    Returns a dict matching the Axiom metadata schema, or ``None`` on failure.
    """
    if not title or len(title.strip()) < 5:
        return None

    # Clean the title for the search query — OpenAlex filter syntax chokes on
    # colons, commas, percent signs, parentheses and other special chars
    search_title = title.strip()[:200]
    search_title = re.sub(r'[,:;%§()"\[\]{}|/\\&#+*<>]', ' ', search_title)
    search_title = re.sub(r'\s+', ' ', search_title).strip()
    if len(search_title) < 5:
        return None
    url = "https://api.openalex.org/works"
    params = {
        "filter": f"title.search:{search_title}",
        "per_page": "5",
    }
    try:
        client = _get_client()
        resp = await client.get(url, params=params)
        resp.raise_for_status()
        data = resp.json()
        results = data.get("results", [])
        if not results:
            logger.info(f"OpenAlex lookup for title '{search_title[:60]}...': no results")
            return None

        # Pick best match by title similarity
        best = None
        best_score = 0.0
        for work in results:
            work_title = work.get("title", "")
            score = _title_similarity(search_title, work_title)
            if score > best_score:
                best_score = score
                best = work

        # Require a reasonable match
        if best is None or best_score < 0.6:
            logger.info(
                f"OpenAlex lookup for title '{search_title[:60]}...': "
                f"best match score {best_score:.2f} too low"
            )
            return None

        # Extract authors
        found_authors = []
        for authorship in best.get("authorships", []):
            author = authorship.get("author", {})
            name = author.get("display_name")
            if name:
                found_authors.append(name)

        # Journal / source
        journal = None
        primary_loc = best.get("primary_location") or {}
        source = primary_loc.get("source") or {}
        journal = source.get("display_name")

        result = {
            "title": best.get("title"),
            "authors": found_authors if found_authors else None,
            "publication_year": best.get("publication_year"),
            "journal_or_source": journal,
            "doi": normalize_doi(best.get("doi") or ""),
            "document_type": "paper",
        }
        logger.info(
            f"OpenAlex lookup for title '{search_title[:60]}...': "
            f"found (score={best_score:.2f})"
        )
        return result

    except httpx.TimeoutException:
        logger.warning(f"OpenAlex lookup for title '{search_title[:60]}...': timeout")
        return None
    except Exception as exc:
        logger.warning(f"OpenAlex lookup for title '{search_title[:60]}...': error – {exc}")
        return None


# ---------------------------------------------------------------------------
# DOI.org content negotiation (DOI → BibTeX)
# ---------------------------------------------------------------------------

async def lookup_doi_bibtex(doi: str) -> Optional[str]:
    """Resolve a DOI to BibTeX via content negotiation at doi.org.

    Returns the raw BibTeX string or ``None`` on failure.
    """
    url = f"https://doi.org/{doi}"
    try:
        client = _get_client()
        resp = await client.get(url, headers={"Accept": "application/x-bibtex"})
        if resp.status_code == 404:
            logger.info(f"DOI BibTeX lookup for {doi}: not found")
            return None
        resp.raise_for_status()
        bibtex = resp.text.strip()
        if bibtex and bibtex.startswith("@"):
            logger.info(f"DOI BibTeX lookup for {doi}: found")
            return bibtex
        logger.info(f"DOI BibTeX lookup for {doi}: response not BibTeX")
        return None
    except httpx.TimeoutException:
        logger.warning(f"DOI BibTeX lookup for {doi}: timeout")
        return None
    except Exception as exc:
        logger.warning(f"DOI BibTeX lookup for {doi}: error – {exc}")
        return None


# ---------------------------------------------------------------------------
# Web page metadata extraction (HTML → metadata)
# ---------------------------------------------------------------------------

def extract_web_metadata(html: str) -> dict:
    """Extract metadata from HTML using Schema.org JSON-LD, Dublin Core, and Open Graph.

    Priority: JSON-LD > Dublin Core > Open Graph.
    Returns a dict with found fields (may be partially empty).
    """
    result: Dict[str, Any] = {}

    if not html:
        return result

    # --- 1. Schema.org JSON-LD ---
    try:
        jsonld_blocks = re.findall(
            r'<script[^>]+type=["\']application/ld\+json["\'][^>]*>(.*?)</script>',
            html,
            re.DOTALL | re.IGNORECASE,
        )
        for block in jsonld_blocks:
            try:
                data = json.loads(block)
                # Handle @graph arrays
                items = data if isinstance(data, list) else [data]
                if isinstance(data, dict) and "@graph" in data:
                    items = data["@graph"]

                for item in items:
                    if not isinstance(item, dict):
                        continue
                    item_type = item.get("@type", "")
                    # Accept Article, ScholarlyArticle, Book, WebPage, etc.
                    if isinstance(item_type, list):
                        item_type = " ".join(item_type)

                    if not result.get("title"):
                        result["title"] = item.get("headline") or item.get("name")

                    if not result.get("authors"):
                        author_raw = item.get("author")
                        if author_raw:
                            if isinstance(author_raw, str):
                                result["authors"] = [author_raw]
                            elif isinstance(author_raw, dict):
                                name = author_raw.get("name")
                                if name:
                                    result["authors"] = [name]
                            elif isinstance(author_raw, list):
                                names = []
                                for a in author_raw:
                                    if isinstance(a, str):
                                        names.append(a)
                                    elif isinstance(a, dict) and a.get("name"):
                                        names.append(a["name"])
                                if names:
                                    result["authors"] = names

                    if not result.get("publication_year"):
                        for key in ("datePublished", "dateCreated", "dateModified"):
                            dval = item.get(key)
                            if dval:
                                ym = re.search(r'(\d{4})', str(dval))
                                if ym:
                                    result["publication_year"] = int(ym.group(1))
                                    break

                    if not result.get("description"):
                        result["description"] = item.get("description")

                    if not result.get("url"):
                        result["url"] = item.get("url") or item.get("mainEntityOfPage")
                        if isinstance(result.get("url"), dict):
                            result["url"] = result["url"].get("@id")

            except (json.JSONDecodeError, TypeError):
                continue
    except Exception:
        pass  # JSON-LD parsing is best-effort

    # --- 2. Dublin Core meta tags ---
    try:
        dc_tags = re.findall(
            r'<meta\s+(?:name|property)=["\'](?:dc|DC|dcterms|DCTERMS)\.(\w+)["\']\s+content=["\']([^"\']*)["\']',
            html,
            re.IGNORECASE,
        )
        for name, content in dc_tags:
            name_lower = name.lower()
            if not result.get("title") and name_lower == "title":
                result["title"] = content
            elif not result.get("authors") and name_lower in ("creator", "author"):
                result["authors"] = [content]
            elif not result.get("publication_year") and name_lower == "date":
                ym = re.search(r'(\d{4})', content)
                if ym:
                    result["publication_year"] = int(ym.group(1))
            elif not result.get("description") and name_lower in ("description", "abstract"):
                result["description"] = content
    except Exception:
        pass

    # --- 3. Open Graph meta tags ---
    try:
        og_tags = re.findall(
            r'<meta\s+(?:property|name)=["\']og:(\w+)["\']\s+content=["\']([^"\']*)["\']',
            html,
            re.IGNORECASE,
        )
        for prop, content in og_tags:
            prop_lower = prop.lower()
            if not result.get("title") and prop_lower == "title":
                result["title"] = content
            elif not result.get("description") and prop_lower == "description":
                result["description"] = content
            elif not result.get("url") and prop_lower == "url":
                result["url"] = content
            elif not result.get("website_name") and prop_lower == "site_name":
                result["website_name"] = content

        # Also try article:author and article:published_time
        article_tags = re.findall(
            r'<meta\s+(?:property|name)=["\']article:(\w+)["\']\s+content=["\']([^"\']*)["\']',
            html,
            re.IGNORECASE,
        )
        for prop, content in article_tags:
            prop_lower = prop.lower()
            if not result.get("authors") and prop_lower == "author":
                result["authors"] = [content]
            elif not result.get("publication_year") and prop_lower in ("published_time", "published"):
                ym = re.search(r'(\d{4})', content)
                if ym:
                    result["publication_year"] = int(ym.group(1))
    except Exception:
        pass

    # Clean up None values
    return {k: v for k, v in result.items() if v is not None}


# ---------------------------------------------------------------------------
# URL-based metadata extraction
# ---------------------------------------------------------------------------

# Known institutional domains → organization name mapping
_KNOWN_ORGS: Dict[str, str] = {
    "ecb.europa.eu": "Europäische Zentralbank (EZB)",
    "europa.eu": "Europäische Union",
    "bundesbank.de": "Deutsche Bundesbank",
    "destatis.de": "Statistisches Bundesamt",
    "bmas.de": "Bundesministerium für Arbeit und Soziales",
    "bmwk.de": "Bundesministerium für Wirtschaft und Klimaschutz",
    "bmf.de": "Bundesministerium der Finanzen",
    "bundestag.de": "Deutscher Bundestag",
    "bundesregierung.de": "Bundesregierung",
    "sachverstaendigenrat-wirtschaft.de": "Sachverständigenrat",
    "imf.org": "International Monetary Fund (IMF)",
    "worldbank.org": "World Bank",
    "oecd.org": "OECD",
    "bis.org": "Bank for International Settlements (BIS)",
    "eurostat.ec.europa.eu": "Eurostat",
    "ec.europa.eu": "Europäische Kommission",
    "publications.europa.eu": "EU Publications Office",
}


def extract_metadata_from_url(url: str) -> dict:
    """Extract website name, organization, and year from a URL.

    Covers three gaps:
    1. Website/org name from domain (e.g., ecb.europa.eu → EZB)
    2. Publication year from URL path (e.g., /2023/ or /202303_)
    3. Known institutional org mapping
    """
    result: Dict[str, Any] = {}
    if not url:
        return result

    url_lower = url.lower()

    # --- Extract domain-based website name ---
    try:
        from urllib.parse import urlparse
        parsed = urlparse(url_lower)
        host = parsed.hostname or ""

        # Check known orgs (longest match first)
        for domain, org in sorted(_KNOWN_ORGS.items(), key=lambda x: -len(x[0])):
            if host.endswith(domain):
                result["organization"] = org
                result["website_name"] = org
                break

        # Generic website name from domain if no known org
        if "website_name" not in result and host:
            # Remove www. and common TLDs to get a readable name
            parts = host.replace("www.", "").split(".")
            if len(parts) >= 2:
                # Use the main domain part, capitalize
                site_name = parts[-2].capitalize()
                # Special cases for compound domains
                if parts[-1] in ("de", "com", "org", "net", "eu", "at", "ch"):
                    result["website_name"] = site_name
    except Exception:
        pass

    # --- Extract year from URL path ---
    # Common patterns: /2023/, /202303_, /2021-06, ?year=2024
    year_match = re.search(r'[/=_-](20[1-2]\d)(?:[/\-_.?&]|$)', url)
    if year_match:
        result["publication_year"] = int(year_match.group(1))

    return result


def extract_author_from_bundestag(title: str, metadata: dict) -> Optional[str]:
    """For Bundestag Wissenschaftliche Dienste, use the org as author."""
    journal = metadata.get("journal_or_source", "")
    if "wissenschaftliche dienste" in (journal or "").lower() or "bundestag" in (title or "").lower():
        return "Wissenschaftliche Dienste des Deutschen Bundestages"
    return None


# ---------------------------------------------------------------------------
# Document type classification
# ---------------------------------------------------------------------------

def classify_document_type(metadata: dict, filename: str = "") -> str:
    """Classify document into: academic, book, legal, institutional, web, wikipedia.

    Priority:
    1. LLM-extracted document_type (most reliable — the LLM read the content)
    2. Hard signals (Wikipedia URL, ISBN → book, DOI → academic)
    3. Soft rule-based fallback (title keywords, URL patterns)
    """
    title = (metadata.get('title') or '').lower()
    url = (metadata.get('url') or '').lower()
    fn = filename.lower()
    authors = metadata.get('authors') or []
    doi = metadata.get('doi')
    isbn = metadata.get('isbn')

    # --- Always override: Wikipedia detection from URL ---
    if 'wikipedia.org' in url or 'wikipedia' in title:
        return 'wikipedia'

    # --- Priority 1: Hard signals from metadata fields (override LLM) ---
    # ISBN is definitive proof of a book — only books have ISBNs
    if isbn:
        return 'book'

    # --- Priority 2: Trust LLM-extracted type if present and meaningful ---
    llm_type = metadata.get('document_type', '')
    if llm_type:
        # Map LLM enum to our types
        type_map = {
            'paper': 'academic',
            'academic': 'academic',
            'book': 'book',
            'legal': 'legal',
            'institutional': 'institutional',
            'web': 'web',
        }
        mapped = type_map.get(llm_type.lower())
        if mapped:
            return mapped
        # 'other' or unknown → fall through to rules

    # --- Priority 2: Hard signals from metadata fields ---
    if isbn or metadata.get('publisher') or metadata.get('edition') or metadata.get('chapters'):
        return 'book'

    if doi or metadata.get('journal_or_source'):
        return 'academic'

    # --- Priority 3: Soft rules (title/URL pattern matching) ---
    # Legal — only match actual statute references, not articles about legal topics
    # Require § at the START of the title (actual law reference, not article discussing law)
    if title.startswith('§') or title.startswith('sgb ') or title.startswith('ksvg'):
        return 'legal'

    # Institutional — known org domains or title patterns
    inst_patterns = ['ezb', 'ecb', 'bundesbank', 'euroraum', 'projektion', 'prognose',
                     'gemeinschaftsdiagnose', 'sachverständigenrat', 'bundesregierung',
                     'bundesministerium', 'european commission', 'imf', 'world bank', 'oecd']
    if any(p in title for p in inst_patterns) or metadata.get('organization'):
        return 'institutional'

    # PDF with authors → likely academic
    if fn.endswith('.pdf') and isinstance(authors, list) and len(authors) > 0:
        return 'academic'

    # Web documents (fetched by research agent)
    if '_web_document' in fn or url:
        return 'web'

    # Default: uploaded PDFs → academic, everything else → web
    if fn.endswith('.pdf') or fn.endswith('.docx'):
        return 'academic'
    return 'web'


# ---------------------------------------------------------------------------
# Completeness scoring
# ---------------------------------------------------------------------------

def calculate_completeness(metadata: dict, filename: str = "") -> int:
    """Score metadata completeness 0-100, adapted to document type."""
    doc_type = metadata.get('document_type') or classify_document_type(metadata, filename)

    if doc_type == 'academic':
        # Traditional: author + year + title + doi/journal
        score = 0
        if metadata.get('title'): score += 25
        authors = metadata.get('authors')
        if authors and authors not in ('', '[]', []): score += 25
        if metadata.get('publication_year'): score += 20
        if metadata.get('doi') or metadata.get('isbn'): score += 10
        if metadata.get('journal_or_source'): score += 10
        if metadata.get('description') or metadata.get('abstract'): score += 10
        return score

    elif doc_type == 'book':
        score = 0
        if metadata.get('title'): score += 25
        authors = metadata.get('authors')
        if authors and authors not in ('', '[]', []): score += 25
        if metadata.get('publication_year'): score += 20
        if metadata.get('isbn'): score += 15
        if metadata.get('publisher'): score += 15
        return score

    elif doc_type == 'legal':
        # Legal sources: statute name + section is enough
        score = 0
        title = metadata.get('title', '')
        if title: score += 40  # Title IS the citation (e.g., "§ 7 SGB IV")
        if '§' in title: score += 30  # Has section reference
        if metadata.get('url'): score += 15  # Has URL for reference
        if metadata.get('publication_year'): score += 15  # Effective date
        return min(score, 100)

    elif doc_type == 'institutional':
        # Org + year + title + URL
        score = 0
        if metadata.get('title'): score += 25
        org = metadata.get('organization') or metadata.get('website_name')
        if org: score += 25
        elif metadata.get('authors'): score += 25  # Some use org as author
        if metadata.get('publication_year'): score += 20
        if metadata.get('url'): score += 20
        if metadata.get('description') or metadata.get('abstract'): score += 10
        return min(score, 100)

    elif doc_type == 'wikipedia':
        # Wikipedia: always 0 for academic use — not a valid source
        return 0

    else:  # 'web' and other
        # Web: site/author + date + title + URL
        score = 0
        if metadata.get('title'): score += 25
        authors = metadata.get('authors')
        site = metadata.get('website_name') or metadata.get('organization')
        if (authors and authors not in ('', '[]', [])) or site: score += 25
        if metadata.get('publication_year'): score += 20
        if metadata.get('url'): score += 20
        if metadata.get('description'): score += 10
        return min(score, 100)


# ---------------------------------------------------------------------------
# Shared helpers — used by all three paths (upload, reprocess, enrich button)
# ---------------------------------------------------------------------------

def needs_metadata_retry(metadata: Optional[dict], filename: str = "") -> bool:
    """Check if metadata extraction needs a retry with better text.

    Returns True when:
    - No metadata at all
    - Missing authors (incomplete extraction)
    - Title looks like it was derived from the filename (LLM fabrication)
    """
    if not metadata:
        return True
    title = metadata.get('title', '')
    authors = metadata.get('authors')
    has_authors = authors and authors != [] and authors != '[]' and authors != ''
    if not title and not has_authors:
        return True
    if title and not has_authors:
        return True
    if title and filename:
        fn_stem = re.sub(r'\.[^.]+$', '', filename)
        fn_normalized = fn_stem.replace('_', ' ').replace('-', ' ').lower().strip()
        title_normalized = title.lower().strip()
        if fn_normalized and (title_normalized == fn_normalized or
                              fn_normalized.startswith(title_normalized) or
                              title_normalized.startswith(fn_normalized)):
            return True
    return False



def prepare_text_sample(markdown_content: str) -> str:
    """Prepare a clean text sample from markdown for metadata extraction.

    Shared logic for all code paths:
    1. Strip image markdown (wastes chars, no metadata value)
    2. Detect and skip series catalog pages (find title heading before ISBN)
    3. Take first 4000 + last 2000 chars
    """
    md_clean = re.sub(r'!\[.*?\]\([^)]*\)\s*', '', markdown_content)

    sample_start = 0
    first_500 = md_clean[:500]
    italic_entries = len(re.findall(r'\*[A-Z][a-zäöü]+[\s·•,]', first_500))
    if italic_entries >= 5:
        headings = list(re.finditer(r'\n(#{1,2}\s+\*?\*?[A-ZÄÖÜ])', md_clean))
        isbn_pos = re.search(r'ISBN\s', md_clean)
        if isbn_pos and headings:
            title_heading = None
            for h in headings:
                if h.start() < isbn_pos.start() and h.start() > 200:
                    title_heading = h
            if title_heading:
                sample_start = max(0, title_heading.start() - 50)
        elif headings and len(headings) > 1:
            sample_start = max(0, headings[1].start() - 50)

    return md_clean[sample_start:sample_start + 4000] + (md_clean[-2000:] if len(md_clean) > 6000 else "")


def finalize_metadata(metadata: dict, filename: str = "") -> dict:
    """Apply final normalization, classification, and scoring to metadata.

    Shared logic for all code paths:
    1. Normalize ALL CAPS titles/authors to Title Case
    2. Classify document type
    3. Calculate completeness score
    """
    # Title case normalization
    title = metadata.get('title', '')
    if title and title == title.upper() and len(title) > 5:
        metadata['title'] = title.title()
    authors = metadata.get('authors', [])
    if isinstance(authors, list):
        metadata['authors'] = [
            a.title() if isinstance(a, str) and a == a.upper() and len(a) > 3 else a
            for a in authors
        ]

    # Classification and scoring
    metadata['document_type'] = classify_document_type(metadata, filename)
    metadata['metadata_completeness'] = calculate_completeness(metadata, filename)

    return metadata


# ---------------------------------------------------------------------------
# Merge helper
# ---------------------------------------------------------------------------

def _merge_metadata(existing: dict, external: dict, source_name: str, sources_tracker: dict) -> dict:
    """Merge *external* into *existing*, only filling ``None``/empty fields.

    Updates *sources_tracker* to record which source provided each field.
    Returns the merged dict (same object as *existing*, mutated in place).
    """
    for key, value in external.items():
        if value is None:
            continue
        # Private keys (e.g. CrossRef's _isbn_print/_isbn_electronic) are
        # for in-pipeline use only — never persist them.
        if key.startswith("_"):
            continue
        # For list fields, treat empty list as missing
        if isinstance(value, list) and len(value) == 0:
            continue

        existing_val = existing.get(key)
        is_missing = (
            existing_val is None
            or existing_val == ""
            or (isinstance(existing_val, list) and len(existing_val) == 0)
        )

        if is_missing:
            existing[key] = value
            sources_tracker[key] = source_name

    return existing


# ---------------------------------------------------------------------------
# Main enrichment function
# ---------------------------------------------------------------------------

async def enrich_metadata(
    existing_metadata: dict,
    document_text: str,
    html_content: Optional[str] = None,
    filename: str = "",
) -> dict:
    """Enrich metadata by looking up external databases.

    Pipeline:
      1. Detect DOI / ISBN / arXiv identifiers in *document_text*.
      2. Look up via CrossRef (DOI), OpenLibrary (ISBN), or OpenAlex (title).
      3. If *html_content* is provided, extract embedded web metadata.
      4. Merge results – external data only fills ``None``/empty fields.
      5. Attach ``metadata_completeness`` (0-100) and ``metadata_sources``.

    Returns the enriched metadata dict.
    """
    if existing_metadata is None:
        existing_metadata = {}

    # Copy so we don't mutate the caller's dict unexpectedly
    enriched = dict(existing_metadata)
    sources: Dict[str, str] = {}

    # Record which fields already came from the LLM
    for key, val in enriched.items():
        if val is not None and val != "" and not (isinstance(val, list) and len(val) == 0):
            sources[key] = "llm"

    # --- Step 1: Detect identifiers ---
    ids = detect_identifiers(document_text)
    logger.info(f"Detected identifiers: {ids}")

    # Normalize any existing DOI from LLM extraction
    if enriched.get("doi"):
        enriched["doi"] = normalize_doi(enriched["doi"])

    # If existing metadata already has a DOI/ISBN, prefer it
    if enriched.get("doi") and not ids["doi"]:
        ids["doi"] = enriched["doi"]
    if enriched.get("isbn") and not ids["isbn"]:
        ids["isbn"] = enriched["isbn"]

    # Store detected identifiers even if lookups fail
    if ids["doi"] and not enriched.get("doi"):
        enriched["doi"] = ids["doi"]
        sources["doi"] = "regex_detection"
    if ids["isbn"] and not enriched.get("isbn"):
        enriched["isbn"] = ids["isbn"]
        sources["isbn"] = "regex_detection"

    # Extract ISBN from DOI if DOI contains an ISBN-13 pattern (e.g., 10.xxxxx/9783791043500)
    doi_val = enriched.get("doi", "")
    if doi_val and not enriched.get("isbn"):
        isbn_in_doi = re.search(r'(97[89]\d{10})', doi_val.replace('-', ''))
        if isbn_in_doi:
            enriched["isbn"] = isbn_in_doi.group(1)
            sources["isbn"] = "doi_derived"

    # --- Step 2: External lookups ---
    # Try CrossRef first (most reliable for papers with DOIs)
    crossref_isbn_print: Optional[str] = None
    crossref_isbn_electronic: Optional[str] = None
    if ids["doi"]:
        cr_result = await lookup_crossref(ids["doi"])
        if cr_result:
            # Strip the private ISBN variants before merge so they don't
            # pollute persisted metadata; keep them for the OL fallback.
            crossref_isbn_print = cr_result.pop("_isbn_print", None)
            crossref_isbn_electronic = cr_result.pop("_isbn_electronic", None)
            _merge_metadata(enriched, cr_result, "crossref", sources)

    # Try OpenLibrary for books with ISBNs. OL indexes print editions more
    # reliably than eBooks, so we walk a prioritized list of ISBN variants
    # and stop at the first hit.
    ol_result = None
    tried_isbns: set = set()
    isbn_candidates: List[str] = []
    # 1. Primary ISBN detected in the PDF text (usually what the cover shows)
    if ids["isbn"]:
        isbn_candidates.append(ids["isbn"])
    # 2. CrossRef's canonical print ISBN (when CrossRef came through)
    if crossref_isbn_print:
        isbn_candidates.append(crossref_isbn_print)
    # 3. CrossRef's electronic ISBN, only if print wasn't tried
    if crossref_isbn_electronic:
        isbn_candidates.append(crossref_isbn_electronic)
    # 4. ISBN embedded in the DOI (Springer-style 10.xxxx/9783xxxxxxxxx)
    if enriched.get("doi"):
        m = re.search(r'(97[89]\d{10})', enriched["doi"].replace('-', ''))
        if m:
            isbn_candidates.append(m.group(1))

    for cand in isbn_candidates:
        # Normalize for dedup: OL accepts dashed and undashed forms.
        key = cand.replace('-', '').strip()
        if not key or key in tried_isbns:
            continue
        tried_isbns.add(key)
        ol_result = await lookup_openlibrary(cand)
        if ol_result:
            _merge_metadata(enriched, ol_result, "openlibrary", sources)
            if cand != ids.get("isbn"):
                logger.info(
                    f"OpenLibrary: fallback ISBN {cand} succeeded after "
                    f"primary {ids.get('isbn')} failed"
                )
            break

    # Fall back to OpenAlex title search if we still lack key fields or DOI
    needs_more = (
        not enriched.get("authors")
        or not enriched.get("publication_year")
        or not enriched.get("journal_or_source")
        or not enriched.get("doi")
    )
    if needs_more and enriched.get("title"):
        oa_result = await lookup_openalex(
            enriched["title"],
            enriched.get("authors"),
        )
        if oa_result:
            _merge_metadata(enriched, oa_result, "openalex", sources)

    # --- Step 3: Web metadata from HTML ---
    if html_content:
        web_meta = extract_web_metadata(html_content)
        if web_meta:
            _merge_metadata(enriched, web_meta, "web_html", sources)

    # --- Step 3b: URL-based metadata (domain → org/website, path → year) ---
    url = enriched.get("url", "")
    if url:
        url_meta = extract_metadata_from_url(url)
        if url_meta:
            _merge_metadata(enriched, url_meta, "url_pattern", sources)

    # --- Step 3c: Institutional author fallback ---
    # For Bundestag Wissenschaftliche Dienste and similar, use org as author
    authors = enriched.get("authors")
    if not authors or authors == [] or authors == "[]":
        bundestag_author = extract_author_from_bundestag(
            enriched.get("title", ""), enriched
        )
        if bundestag_author:
            enriched["authors"] = [bundestag_author]
            sources["authors"] = "org_fallback"
        elif enriched.get("organization"):
            # Generic: use organization as author for institutional/web sources
            enriched["authors"] = [enriched["organization"]]
            sources["authors"] = "org_fallback"

    # --- Step 4 & 5: Document type, completeness scoring, and source tracking ---
    enriched["document_type"] = classify_document_type(enriched, filename)
    enriched["metadata_completeness"] = calculate_completeness(enriched, filename)
    enriched["metadata_sources"] = list(set(sources.values()))

    logger.info(
        f"Metadata enrichment complete: completeness={enriched['metadata_completeness']}, "
        f"sources={enriched['metadata_sources']}"
    )

    return enriched


# ---------------------------------------------------------------------------
# Lightweight cached lookup for writing-time enrichment
# ---------------------------------------------------------------------------

# Module-level cache keyed by source_id → enriched metadata snippet
_writing_cache: Dict[str, Optional[dict]] = {}


async def quick_enrich_for_writing(
    source_id: str,
    title: Optional[str],
    existing_authors: Any = None,
    existing_year: Any = None,
) -> Optional[dict]:
    """Lightweight enrichment for writing-time citation formatting.

    Only fires an OpenAlex lookup if authors or year are missing and we have a title.
    Results are cached per source_id for the lifetime of the process.

    Returns a dict with ``authors`` and ``publication_year`` keys, or ``None``.
    """
    if source_id in _writing_cache:
        return _writing_cache[source_id]

    # Check if we actually need enrichment
    has_authors = (
        existing_authors
        and existing_authors != "Unknown Authors"
        and existing_authors != "N/A"
    )
    has_year = existing_year and existing_year != "N/A"

    if has_authors and has_year:
        _writing_cache[source_id] = None
        return None

    if not title or title == "Unknown Title":
        _writing_cache[source_id] = None
        return None

    logger.info(f"Quick enrichment for source {source_id}: looking up title '{title[:60]}...'")

    try:
        result = await lookup_openalex(title)
        if result:
            snippet = {}
            if not has_authors and result.get("authors"):
                snippet["authors"] = result["authors"]
            if not has_year and result.get("publication_year"):
                snippet["publication_year"] = result["publication_year"]
            if snippet:
                _writing_cache[source_id] = snippet
                logger.info(f"Quick enrichment for source {source_id}: found {list(snippet.keys())}")
                return snippet
    except Exception as exc:
        logger.warning(f"Quick enrichment for source {source_id}: error – {exc}")

    _writing_cache[source_id] = None
    return None


def clear_writing_cache() -> None:
    """Clear the writing-time enrichment cache (call between missions)."""
    _writing_cache.clear()
