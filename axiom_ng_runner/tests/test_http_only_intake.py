"""Owner ruling (precision-wave post-mortem): the runner is HTTP-ONLY by
contract. source_url wins when present; without it, a local path outside
the allowed roots must die as the LOUD SOURCE_URL_MISSING policy error —
never the confusing SOURCE_NOT_FOUND that cost 130 jobs in minutes.

Red-first: on the pre-ruling code, case 1 raises SOURCE_NOT_FOUND (the
filesystem probe of a Mac path inside a carrier container).
"""

import unittest
from unittest import mock

from axiom_ng_runner import app, validation


def _req(local_path="/Users/dudu/Zotero/storage/X/book.pdf", source_url=""):
    att = mock.Mock()
    att.local_path = local_path
    att.source_url = source_url
    att.content_type = "application/pdf"
    att.content_hash = None
    req = mock.Mock()
    req.attachment = att
    return req


class HttpOnlyIntakeTests(unittest.TestCase):
    def test_mac_path_without_source_url_dies_loud(self):
        # carrier shape: Mac storage path, /data-only roots, no source_url
        s = mock.Mock(allowed_source_roots=("/data",))
        with mock.patch.object(app.settings, "get", return_value=s), \
             mock.patch.object(app, "validate_content_type"):
            with self.assertRaises(validation.SourceError) as ctx:
                app._validate_request(_req(source_url=""))
        self.assertEqual(ctx.exception.code, "SOURCE_URL_MISSING")
        self.assertIn("SOURCE_BASE_URL", ctx.exception.message)

    def test_source_url_present_downloads_even_with_local_path(self):
        # HTTP-only precedence: the download path is taken even when a
        # (would-be-valid) local path exists
        s = mock.Mock(allowed_source_roots=("/data",))
        sentinel = "/tmp/sentinel.pdf"
        with mock.patch.object(app.settings, "get", return_value=s), \
             mock.patch.object(app, "validate_content_type"), \
             mock.patch.object(app, "_download_source", return_value=sentinel) as dl:
            got = app._validate_request(_req(source_url="http://x/y.pdf"))
        self.assertEqual(got, sentinel)
        dl.assert_called_once()

    def test_local_fixture_delivery_still_works(self):
        # fixture/co-located shape: no source_url, path under allowed roots
        import tempfile, os
        with tempfile.NamedTemporaryFile(suffix=".pdf", delete=False) as f:
            f.write(b"%PDF-1.4 fixture")
            path = f.name
        try:
            s = mock.Mock(allowed_source_roots=(tempfile.gettempdir(),))
            with mock.patch.object(app.settings, "get", return_value=s), \
                 mock.patch.object(app, "validate_content_type"), \
                 mock.patch.object(app, "validate_source", return_value=path):
                got = app._validate_request(_req(local_path=path, source_url=""))
            self.assertEqual(got, path)
        finally:
            os.unlink(path)


if __name__ == "__main__":
    unittest.main()
