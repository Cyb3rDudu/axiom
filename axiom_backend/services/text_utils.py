"""Small text-normalisation helpers shared across the writing subsystem.

Centralises three things that were duplicated across five+ modules:

- German umlaut transliteration (ä→ae, ö→oe, ü→ue, ß→ss) because
  NFKD normalisation alone doesn't handle ß.
- ASCII slug generation for stable cross-language identifiers.
- Family-name matching form (umlauts + lowercase + punctuation stripped)
  used by the citation sync validator.

Nothing here is specific to a deliverable type or a language profile —
they're primitive utilities. Consumers should NOT reimplement these.
"""

from __future__ import annotations

import re
import unicodedata


# German umlauts get explicit transliteration because `unicodedata.normalize("NFKD")`
# only decomposes them into base + combining diaeresis; the ASCII encoding
# step then drops the diaeresis but keeps the base, yielding "u" for "ü"
# instead of the conventional academic "ue". ß has no decomposition at all.
GERMAN_UMLAUTS = str.maketrans({
    "ä": "ae", "Ä": "ae",
    "ö": "oe", "Ö": "oe",
    "ü": "ue", "Ü": "ue",
    "ß": "ss",
})


_NON_ALNUM = re.compile(r"[^a-z0-9]+")
_NON_ALNUM_NOSEP = re.compile(r"[^a-z0-9]+")


def slugify_ascii(text: str, separator: str = "-", fallback: str = "ref") -> str:
    """Turn arbitrary text into a lowercase ASCII slug.

    Steps:
      1. Transliterate German umlauts explicitly.
      2. NFKD-normalise and drop the combining marks.
      3. Lowercase, replace non-alphanumeric runs with `separator`.
      4. Strip leading/trailing separators.

    Returns `fallback` if the result is empty (e.g. input was all
    punctuation).
    """
    if not text:
        return fallback
    transliterated = text.translate(GERMAN_UMLAUTS)
    normalised = (
        unicodedata.normalize("NFKD", transliterated)
        .encode("ascii", "ignore")
        .decode("ascii")
    )
    replacement = separator or ""
    slug = _NON_ALNUM.sub(replacement, normalised.lower())
    if separator:
        slug = slug.strip(separator)
    return slug or fallback


def normalise_family_name(raw: str) -> str:
    """Collapse a family-name token to a matching form.

    Drops whitespace, punctuation, hyphens — `Hotz-Hart` → `hotzhart`,
    `de la Cruz` → `delacruz`. Used by the citation-sync validator to
    match in-text markers against registry entries regardless of how
    the writer spelled multi-part names.

    Does NOT preserve any structural information; this is deliberately
    a lossy canonicalisation for fuzzy matching, not for display.
    """
    if not raw:
        return ""
    transliterated = raw.translate(GERMAN_UMLAUTS)
    normalised = (
        unicodedata.normalize("NFKD", transliterated)
        .encode("ascii", "ignore")
        .decode("ascii")
    )
    return _NON_ALNUM_NOSEP.sub("", normalised.lower())
