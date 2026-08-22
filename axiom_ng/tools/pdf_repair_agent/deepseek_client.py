"""DeepSeek-Client (OpenAI-kompatibler Chat-Endpoint) via httpx.

Der Client ist schlank und injizierbar: Tests fahren die Agent-Schleife mit
`MockClient` OHNE Netz. Nur dieser Client spricht mit dem Modell; er schreibt
nie PDF-Bytes.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass


@dataclass
class ChatMessage:
    role: str
    content: str


class MockClient:
    """Deterministischer Ersatz für Mock-Läufe: liefert festgelegte Antworten
    sequentiell. Kein Netz; zeichnet die gemachten Aufrufe für Assertionen."""

    def __init__(self, responses: list[str], delay: float = 0.0):
        self.responses = responses
        self.delay = delay
        self.calls: list[list[ChatMessage]] = []

    def complete(self, messages: list[ChatMessage], temperature: float = 0.0) -> str:
        self.calls.append(list(messages))
        if self.delay:
            time.sleep(self.delay)
        if not self.responses:
            raise RuntimeError("MockClient: keine Antworten mehr")
        return self.responses.pop(0)


class DeepSeekClient:
    """Echter Chat-Endpoint; Key/Basis/Modell aus der Config.

    Raises RuntimeError bei Transport-/Status-/Parse-Fehler (Beweis, kein
    stilles Leerfeld). Gibt den assistant-Content-Text zurück; der Agent
    parst daraus das JSON-`step`."""

    def __init__(
        self, api_key: str, base_url: str, model: str, *, http_timeout: float = 120.0
    ):
        if not api_key:
            raise ValueError("DEEPSEEK_API_KEY nicht gesetzt — kein Modell-Lauf")
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.http_timeout = http_timeout
        self._httpx = None

    def _client(self):
        if self._httpx is None:
            import httpx  # type: ignore[reportMissingImports]

            self._httpx = httpx
        return self._httpx

    def complete(self, messages: list[ChatMessage], temperature: float = 0.0) -> str:
        payload = {
            "model": self.model,
            "messages": [{"role": m.role, "content": m.content} for m in messages],
            "temperature": temperature,
        }
        httpx = self._client()
        try:
            resp = httpx.post(
                f"{self.base_url}/chat/completions",
                json=payload,
                headers={"Authorization": f"Bearer {self.api_key}"},
                timeout=self.http_timeout,
            )
        except Exception as exc:  # noqa: BLE001 — Transport/Socket: Beweis
            raise RuntimeError(f"DeepSeek transport error: {exc}") from exc
        if resp.status_code != 200:
            raise RuntimeError(f"DeepSeek HTTP {resp.status_code}: {resp.text[:300]}")
        try:
            data = resp.json()
            out = data["choices"][0]["message"]["content"]
        except (ValueError, KeyError, IndexError) as exc:
            raise RuntimeError(f"DeepSeek parse error: {exc}") from exc
        return str(out)


def parse_step(raw: str) -> dict:
    """Assistant-Text → JSON-`step`-Objekt. Robust gegen führende/nach-ziehende
    Prosa; ValueError bei fehlendem/ungültigem JSON (der Loop fängt das als
    fehlgeschlagene Antwort — kein weiteres Raten)."""
    s = raw.strip()
    a, b = s.find("{"), s.rfind("}")
    if a < 0 or b <= a:
        raise ValueError("kein JSON-Objekt im Antworttext")
    try:
        obj = json.loads(s[a : b + 1])
    except json.JSONDecodeError as exc:
        raise ValueError(f"ungültiges JSON im Antworttext: {exc}") from exc
    if not isinstance(obj, dict):
        raise ValueError("Antwort ist kein Objekt")
    return obj
