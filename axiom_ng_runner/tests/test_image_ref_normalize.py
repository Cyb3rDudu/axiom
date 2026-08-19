"""#192: Marker emits markdown image paths with ESCAPED specials
(`\\_page\\_6\\_Figure\\_5.jpeg`); the chunker regex captures the escaped
string, the artifact is stored UNESCAPED, and the persist gate correctly
rejects the unresolved reference (CHUNK_IMAGE_REF_UNRESOLVED — the W9
terminal fail on "Einführung in die Managementlehre"). The runner's
image-ref normalization must unescape markdown backslash escapes.

Red-first: without the unescape, the escaped ref passes through verbatim
and the artifact lookup misses.

Run: python3 -m pytest axiom_ng_runner/tests/test_image_ref_normalize.py
"""

import unittest

from axiom_ng_runner.runner import _normalize_image_refs


class ImageRefUnescapeTests(unittest.TestCase):
    def test_escaped_underscores_unescape(self):
        got = _normalize_image_refs(["\\_page\\_6\\_Figure\\_5.jpeg"])
        self.assertEqual(got, ["_page_6_Figure_5.jpeg"])

    def test_plain_refs_unchanged(self):
        got = _normalize_image_refs(["image-0000.png", "charts/x-1.jpg"])
        self.assertEqual(got, ["image-0000.png", "charts/x-1.jpg"])

    def test_dict_refs_take_path_and_unescape(self):
        got = _normalize_image_refs([
            {"path": "\\_page\\_2\\_Figure\\_1.jpeg", "alt_text": "chart"},
        ])
        self.assertEqual(got, ["_page_2_Figure_1.jpeg"])

    def test_double_backslash_collapses_to_literal(self):
        got = _normalize_image_refs(["a\\\\b.png"])
        self.assertEqual(got, ["a\\b.png"])


if __name__ == "__main__":
    unittest.main()
