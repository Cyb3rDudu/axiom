"""#220 Stage 1 — EPUB branch of /v1/pdf/preflight (#175 extension).

Same Rot→Skip→Repair policy as PDF: healthy EPUB → ok, DRM/broken → not ok.
epubcheck is expected absent on the test host → status not_available (the
gate degrades honestly to the built-in light checks).

Run: .venv/bin/python -m pytest tests/test_epub_preflight.py
"""
from __future__ import annotations

import zipfile

import pytest
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from fastapi.testclient import TestClient

_OPF = """<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
 <metadata/><manifest>
  <item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>
 </manifest>
 <spine><itemref idref="c1"/></spine>
</package>
"""
_CONTAINER = """<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
 <rootfiles><rootfile full-path="content.opf"
  media-type="application/oebps-package+xml"/></rootfiles>
</container>
"""
_CHAPTER = ('<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml">'
            "<body><p>" + "Lakehouse Kapitel mit gut extrahierbarem Text. " * 20 +
            "</p></body></html>")


def _epub_bytes(extra: dict[str, str] | None = None) -> bytes:
    import io

    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("content.opf", _OPF)
        z.writestr("c1.xhtml", _CHAPTER)
        for name, body in (extra or {}).items():
            z.writestr(name, body)
    return buf.getvalue()


@pytest.fixture()
def client(tmp_path):
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work"))
    try:
        with TestClient(app) as c:
            yield c
    finally:
        settings.set(old)


def test_preflight_healthy_epub_ok(client):
    r = client.post(
        "/v1/pdf/preflight",
        content=_epub_bytes(),
        headers={"Content-Type": "application/epub+zip"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["ok"] is True
    assert body["details"]["format"] == "epub"
    assert body["details"]["text_layer"] is True
    assert body["details"]["drm"] is False
    assert body["details"]["epubcheck"]["status"] in ("ok", "not_available")


def test_preflight_drm_epub_rejected(client):
    r = client.post(
        "/v1/pdf/preflight",
        content=_epub_bytes({"META-INF/rights.xml": "<rights/>"}),
        headers={"Content-Type": "application/epub+zip"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["ok"] is False
    assert body["verdacht"].startswith("🔴")
    assert body["details"]["drm"] is True


def test_preflight_font_obfuscation_is_not_drm(client):
    """encryption.xml with the IDPF font-obfuscation algorithm is legit."""
    r = client.post(
        "/v1/pdf/preflight",
        content=_epub_bytes({"META-INF/encryption.xml": (
            '<encryption><EncryptedData '
            'xmlns="http://www.w3.org/2001/04/xmlenc#">'
            '<EncryptionMethod Algorithm="http://www.idpf.org/2008/embedding"/>'
            "</EncryptedData></encryption>")}),
        headers={"Content-Type": "application/epub+zip"},
    )
    assert r.status_code == 200, r.text
    assert r.json()["details"]["drm"] is False


def test_preflight_broken_zip_is_structured_500(client):
    r = client.post(
        "/v1/pdf/preflight",
        content=b"not a zip at all",
        headers={"Content-Type": "application/epub+zip"},
    )
    assert r.status_code == 500
    assert r.json()["detail"]["code"] == "PREFLIGHT_PARSE"


def test_preflight_missing_spine_rejected(client):
    buf = _epub_bytes()
    # strip the OPF by rewriting without content.opf
    import io

    src = zipfile.ZipFile(io.BytesIO(buf))
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as z:
        for name in src.namelist():
            if name not in ("content.opf", "META-INF/container.xml"):
                z.writestr(name, src.read(name))
    r = client.post(
        "/v1/pdf/preflight",
        content=out.getvalue(),
        headers={"Content-Type": "application/epub+zip"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["ok"] is False
    assert body["details"]["opf_spine"] is False
