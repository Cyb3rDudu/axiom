"""Tests for the content-block:references parser (#51/#53)."""

from __future__ import annotations

import sys
from pathlib import Path

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services.structured_bibliography import parse_references_block  # noqa: E402


VALID_BLOCK = '''Some preamble from the writer.

```content-block:document
The main draft body here...
```

```content-block:references
[
  {
    "entry_key": "destatis-2024",
    "authors": [{"family": "Destatis", "given": ""}],
    "year": 2024,
    "title": "Außenhandel 2024",
    "container_title": "Statistisches Bundesamt",
    "url": "https://www.destatis.de/DE/Home/",
    "reference_type": "web"
  },
  {
    "entry_key": "mueller-2024",
    "authors": [{"family": "Müller", "given": "Peter"}],
    "year": 2024,
    "title": "China in der Weltwirtschaft",
    "publisher": "Vahlen",
    "reference_type": "document"
  }
]
```
'''


class TestParseReferencesBlock:
    def test_no_block_found(self):
        out = parse_references_block("Just a plain response with no block.")
        assert out.block_found is False
        assert out.entries == []
        assert out.errors == []

    def test_valid_block(self):
        out = parse_references_block(VALID_BLOCK)
        assert out.block_found is True
        assert len(out.entries) == 2
        assert out.errors == []
        assert out.entries[0]["entry_key"] == "destatis-2024"
        assert out.entries[1]["entry_key"] == "mueller-2024"

    def test_empty_block(self):
        text = "```content-block:references\n\n```"
        out = parse_references_block(text)
        assert out.block_found is True
        assert out.entries == []
        assert "empty" in " ".join(out.errors).lower()

    def test_malformed_json(self):
        text = "```content-block:references\n[{not-valid-json}]\n```"
        out = parse_references_block(text)
        assert out.block_found is True
        assert out.entries == []
        assert any("malformed" in e.lower() for e in out.errors)

    def test_non_array_payload(self):
        text = '```content-block:references\n{"entry_key": "x"}\n```'
        out = parse_references_block(text)
        assert out.block_found is True
        assert out.entries == []
        assert any("array" in e.lower() for e in out.errors)

    def test_missing_entry_key_skipped(self):
        text = '''```content-block:references
[
  {"title": "No Key", "url": "https://example.com"},
  {"entry_key": "ok-2024", "title": "Valid", "url": "https://example.com"}
]
```'''
        out = parse_references_block(text)
        assert out.block_found is True
        assert len(out.entries) == 1
        assert out.entries[0]["entry_key"] == "ok-2024"
        assert any("missing entry_key" in e for e in out.errors)

    def test_missing_title_skipped(self):
        text = '''```content-block:references
[
  {"entry_key": "no-title", "url": "https://example.com"}
]
```'''
        out = parse_references_block(text)
        assert out.entries == []
        assert any("missing title" in e.lower() for e in out.errors)

    def test_missing_required_source_signal(self):
        # No url / container_title / publisher / document_id
        text = '''```content-block:references
[
  {"entry_key": "unreachable", "title": "X"}
]
```'''
        out = parse_references_block(text)
        assert out.entries == []
        assert any("url, container_title" in e for e in out.errors)

    def test_duplicate_entry_keys(self):
        text = '''```content-block:references
[
  {"entry_key": "dup", "title": "A", "url": "https://a.example"},
  {"entry_key": "dup", "title": "B", "url": "https://b.example"}
]
```'''
        out = parse_references_block(text)
        assert len(out.entries) == 1
        assert any("duplicate" in e.lower() for e in out.errors)

    def test_case_insensitive_fence(self):
        text = '''```content-block:References
[{"entry_key": "x", "title": "X", "url": "https://x"}]
```'''
        out = parse_references_block(text)
        assert out.block_found is True
        assert len(out.entries) == 1
