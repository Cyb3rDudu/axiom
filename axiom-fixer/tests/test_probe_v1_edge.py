"""#368 regression sonde: the RAG reachability probe must speak the
versioned edge. The probe step (h_probe) is the repair agent's only
runtime HTTP dependency on the RAG surface — when the compat routes were
removed (v0.2.4), a probe still polling the unversioned path reported
"RAG antwortet nicht 200" against a healthy v1-only RAG and silently
degraded every stelle2 measurement. This test pins the path against a
fake RAG answering ONLY /api/v1/zotero/documents.
"""

from __future__ import annotations

import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest


class _V1OnlyRAG(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802 — http.server API
        if self.path == "/api/v1/zotero/documents":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b"[]")
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, *args):  # silence the test log
        return


@pytest.fixture()
def v1_only_rag():
    srv = HTTPServer(("127.0.0.1", 0), _V1OnlyRAG)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def test_probe_reaches_v1_documents(v1_only_rag):
    import repair_agent as ra

    step = {
        "id": "S1",
        "tool": "stelle1_druckseite",
        "params": {"pdf": "x.pdf", "page_hint": None},
    }
    ctx = {"cfg": type("C", (), {"rag_api_base": v1_only_rag})()}
    res = ra.h_probe(step, ctx)
    # the fake RAG answers 200 ONLY on the versioned path — reachable
    # proves the probe polls /api/v1/zotero/documents (a unversioned
    # probe reports "RAG antwortet nicht 200" and leaves stelle2 open)
    offen = [o for o in res.get("offen", []) if "nicht erreichbar" in o or "nicht 200" in o]
    assert not offen, f"probe missed the v1 edge: {offen}"
    assert res["action"] == "probe"
