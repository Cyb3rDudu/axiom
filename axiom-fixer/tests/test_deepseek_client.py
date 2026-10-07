"""G3 deepseek_client — parse_step-Robustheit, Client-Vertrag, MockClient.

Kein Netz: der echte Endpoint wird nur über Fakes mit bewusst
kaputtem Transport/Status/Body geprüft — RuntimeError ist der Beweis,
kein stilles Leerfeld.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

# type: ignore: pyright indiziert paket-lokale Module am Workspace-Root nur
# träge — LSP-False-Positiv; pytest importiert real (siehe Paket-Venv).
from deepseek_client import (  # noqa: E402
    ChatMessage,
    DeepSeekClient,
    MockClient,
    parse_step,
)

# ------------------------------------------------ parse_step ----


def test_parse_step_plain_json():
    assert parse_step('{"action": "probe"}') == {"action": "probe"}


def test_parse_step_prose_wrapped_json():
    raw = 'Ich denke, also bin ich: {"action": "stop"} — damit fertig.'
    assert parse_step(raw) == {"action": "stop"}


def test_parse_step_nested_object():
    raw = '{"action": "surgery", "operations": [{"seite": 1, "label": "I"}]}'
    step = parse_step(raw)
    assert step["operations"][0]["label"] == "I"


def test_parse_step_rejects_non_object_json():
    with pytest.raises(ValueError):
        parse_step("[1, 2, 3]")


def test_parse_step_rejects_empty_and_garbage():
    for raw in ("", "   ", "keine klammern", "{offen"):
        with pytest.raises(ValueError):
            parse_step(raw)


# ------------------------------------------------ MockClient ----


def test_mockclient_roundtrip_and_recording():
    client = MockClient(['{"action": "probe"}', '{"action": "stop"}'])
    first = client.complete([ChatMessage("system", "s"), ChatMessage("user", "t")])
    second = client.complete([ChatMessage("user", "nochmal")])
    assert first == '{"action": "probe"}'
    assert second == '{"action": "stop"}'
    assert len(client.calls) == 2
    assert client.calls[0][0].role == "system"
    # Kopie statt Referenz: Mutation der aufgezeichneten Liste ändert nichts
    client.calls[1].append(ChatMessage("user", "injektion"))
    assert len(client.calls) == 2


def test_mockclient_exhaustion_raises():
    client = MockClient([])
    with pytest.raises(RuntimeError):
        client.complete([ChatMessage("user", "hi")])


# ------------------------------------------------ DeepSeekClient ----


def test_deepseek_requires_api_key():
    with pytest.raises(ValueError, match="DEEPSEEK_API_KEY"):
        DeepSeekClient("", "https://api.example", "deepseek-chat")


class _FakeHttpx:
    """httpx-Stub: liefert konfigurierbaren Fehler statt Netzverkehr."""

    def __init__(self, *, raise_exc=None, status=200, body=None):
        self._raise = raise_exc
        self._status = status
        self._body = body if body is not None else {}
        self.calls = []

    class _Resp:
        def __init__(self, status, body):
            self.status_code = status
            self.text = str(body)
            self._body = body

        def json(self):
            if isinstance(self._body, Exception):
                raise self._body
            return self._body

    def post(self, url, **kw):
        self.calls.append((url, kw))
        if self._raise is not None:
            raise self._raise
        return self._Resp(self._status, self._body)


def _client_with(fake) -> DeepSeekClient:
    c = DeepSeekClient("key", "https://api.example/", "deepseek-chat")
    c._httpx = fake  # noqa: SLF001 — Test injiziert bewusst den Transport
    return c


def test_transport_error_wrapped_as_runtime_error():
    c = _client_with(_FakeHttpx(raise_exc=OSError("dns kaputt")))
    with pytest.raises(RuntimeError, match="transport error"):
        c.complete([ChatMessage("user", "hi")])


def test_http_error_wrapped_with_status():
    c = _client_with(_FakeHttpx(status=500, body={"error": "boom"}))
    with pytest.raises(RuntimeError, match="HTTP 500"):
        c.complete([ChatMessage("user", "hi")])


def test_body_parse_error_wrapped():
    c = _client_with(_FakeHttpx(body=ValueError("kein json")))
    with pytest.raises(RuntimeError, match="parse error"):
        c.complete([ChatMessage("user", "hi")])


def test_success_returns_content_text():
    fake = _FakeHttpx(body={"choices": [{"message": {"content": '{"a": 1}'}}]})
    c = _client_with(fake)
    out = c.complete([ChatMessage("user", "hi")])
    assert out == '{"a": 1}'
    # Endpoint/Payload-Vertrag: Basis-URL ohne Doppel-Slash, Bearer-Key.
    url, kw = fake.calls[0]
    assert url == "https://api.example/chat/completions"
    assert kw["headers"]["Authorization"] == "Bearer key"
    assert kw["json"]["messages"][0]["role"] == "user"
