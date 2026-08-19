"""Remote source delivery (source_url) tests — contract §3 transport.

Fake source server via http.server; the runner's download path must run the
SAME integrity gates as local sources (hash gate proof), stage into the job's
work dir (dies with ACK), and 422 cleanly on unreachable sources.

Mutation-bars:
- removing ensure_hash_matches from _download_source → hash-mismatch test red
- skipping _finalize_source → staging/dedup tests red
"""

import hashlib
import http.server
import re
import shutil
import threading
import time
from pathlib import Path

import pytest
from axiom_ng_runner import app as app_mod
from axiom_ng_runner.config import Settings, settings
from axiom_ng_runner.validation import compute_sha256
from fastapi.testclient import TestClient


class _SourceServer:
    """Serves one file (or errors) at /file for the runner to pull.

    delay > 0 makes the handler sleep before responding (slow/hanging
    sender for the download-budget test)."""

    def __init__(self, root: Path, serve: bool = True, delay: float = 0.0):
        self.root = root
        self.serve = serve
        self.delay = delay
        self.srv: http.server.HTTPServer | None = None
        self.thread: threading.Thread | None = None
        self.port: int | None = None

    def __enter__(self):
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                if outer.delay:
                    import time as _t

                    _t.sleep(outer.delay)
                if not outer.serve:
                    self.send_error(503)
                    return
                f = outer.root / "book.pdf"
                if not f.exists():
                    self.send_error(404)
                    return
                data = f.read_bytes()
                self.send_response(200)
                self.send_header("Content-Type", "application/pdf")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def log_message(self, format, *args):
                pass

        self.srv = http.server.HTTPServer(("127.0.0.1", 0), H)
        self.port = self.srv.server_address[1]
        self.thread = threading.Thread(target=self.srv.serve_forever, daemon=True)
        self.thread.start()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        assert self.srv is not None
        self.srv.shutdown()
        self.srv.server_close()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}/file"


@pytest.fixture()
def dl_client(fixture_dirs):
    """TestClient with EMPTY allowed roots — local mode impossible, only
    source_url works (the integration-shaped unit setup)."""
    work = fixture_dirs["work"] / "dl-client"
    work.mkdir(parents=True, exist_ok=True)
    old = settings.get()
    settings.set(Settings(work_root=work, allowed_source_roots=()))
    try:
        with TestClient(app_mod.app) as c:
            yield c, work
    finally:
        settings.set(old)


def _pdf(root: Path) -> Path:
    f = root / "book.pdf"
    f.write_bytes(b"%PDF-1.4 source-url test bytes\n" + b"x" * 256)
    return f


def _real_pdf(root: Path, fixture_pdf: Path) -> Path:
    """Copy a PyMuPDF-parseable PDF: the reference compute backend must be
    able to complete a job on the downloaded bytes."""
    f = root / "book.pdf"
    shutil.copyfile(fixture_pdf, f)
    return f


def _payload(url: str, src: Path, job_id: str, hash_override: str | None = None, local_path: str | None = None):
    return {
        "contract_version": "1.0",
        "job_id": job_id,
        "idempotency_key": f"{job_id}-key",
        "source": {"type": "zotero", "source_id": "s", "server_id": "srv"},
        "document": {"document_id": "d", "zotero_key": "K", "zotero_version": 1},
        "attachment": {
            "attachment_id": "a",
            "zotero_key": "K",
            "zotero_version": 1,
            "content_type": "application/pdf",
            "filename": "book.pdf",
            "local_path": local_path or "/nonexistent/local/book.pdf",  # default forces download path
            "source_url": url,
            "content_hash": hash_override or compute_sha256(src),
            "size_bytes": src.stat().st_size,
            "mtime_ms": 0,
        },
        "processing": {"profile": "full-rag-v1"},
    }


def _wait_terminal(client, job_id, timeout=30.0):
    import time as _t

    deadline = _t.monotonic() + timeout
    while _t.monotonic() < deadline:
        r = client.get(f"/v1/jobs/{job_id}")
        if r.status_code == 200 and r.json()["status"] in ("completed", "failed"):
            return r.json()
        _t.sleep(0.05)
    raise AssertionError("no terminal state")


class TestSourceDownload:
    def test_download_validates_and_stages_in_work_dir(self, dl_client, tmp_path, fixture_dirs):
        client, work = dl_client
        src = _real_pdf(tmp_path, fixture_dirs["pdf"])
        with _SourceServer(tmp_path) as srv:
            r = client.post("/v1/process", json=_payload(srv.url, src, "job-dl-1"))
        assert r.status_code == 202, r.text
        # Downloaded file staged inside the job's work dir (dies with ACK's
        # remove_work) and became the request's local_path.
        staged = list((work / "job-dl-1" / "work").glob("source*"))
        assert staged, "download must land in the job work dir"
        # The job must run to TERMINAL COMPLETED on the pulled bytes, and the
        # persisted request must point at the staged file inside the work
        # dir (kills a dropped local_path repoint).
        terminal = _wait_terminal(client, "job-dl-1")
        assert terminal["status"] == "completed", terminal
        job = app_mod._store_impl().get("job-dl-1")
        assert job is not None
        lp = Path(job.request["attachment"]["local_path"])
        assert lp.parent == work / "job-dl-1" / "work", lp

    def test_hash_mismatch_of_downloaded_file_rejected(self, dl_client, fixture_dirs):
        client, work = dl_client
        src = _pdf(fixture_dirs["sources"])
        wrong = "sha256:" + hashlib.sha256(b"other").hexdigest()
        with _SourceServer(fixture_dirs["sources"]) as srv:
            r = client.post("/v1/process", json=_payload(srv.url, src, "job-dl-2", wrong))
        # THE gate proof: transport does not bypass integrity.
        assert r.status_code == 422
        assert "hash" in r.json()["detail"].lower()
        # No hash oracle: the remote-pull mismatch detail must not echo the
        # actual file hash (a 64-hex digest of the served bytes).
        assert not re.search(r"[0-9a-f]{64}", r.json()["detail"]), r.json()["detail"]
        # No temp residue.
        incoming = work / ".incoming"
        assert not any(incoming.glob("*/*")) if incoming.exists() else True

    def test_unreachable_source_url_clean_422(self, dl_client, fixture_dirs):
        client, _ = dl_client
        src = _pdf(fixture_dirs["sources"])
        p = _payload("http://127.0.0.1:1/nope", src, "job-dl-3")
        r = client.post("/v1/process", json=p)
        assert r.status_code == 422
        assert "download" in r.json()["detail"].lower() or "source" in r.json()["detail"].lower()

    def test_dedup_drops_temp_download(self, dl_client, fixture_dirs):
        client, work = dl_client
        src = _pdf(fixture_dirs["sources"])
        with _SourceServer(fixture_dirs["sources"]) as srv:
            p = _payload(srv.url, src, "job-dl-4")
            r1 = client.post("/v1/process", json=p)
            r2 = client.post("/v1/process", json=p)  # same idempotency key
        assert r1.status_code == 202 and r2.status_code == 202
        assert r2.json()["deduplicated"]
        incoming = work / ".incoming"
        if incoming.exists():
            leftovers = list(incoming.glob("*/*"))
            assert not leftovers, f"dedup must drop the temp download, found {leftovers}"

    def test_http_only_source_url_wins(self, fixture_dirs):
        """Owner ruling (HTTP-only precedence): source_url set → download,
        even when a locally valid path exists. The download fails against
        the unreachable sentinel host → the job errors loudly instead of
        silently reading the local file."""
        work = fixture_dirs["work"] / "http-first"
        work.mkdir(parents=True, exist_ok=True)
        src = _pdf(fixture_dirs["sources"])
        old = settings.get()
        settings.set(Settings(work_root=work, allowed_source_roots=(str(fixture_dirs["sources"]),)))
        try:
            with TestClient(app_mod.app) as c:
                p = _payload(
                    "http://127.0.0.1:1/must-not-be-fetched", src, "job-http-1",
                    local_path=str(src),  # valid local file — must NOT be used
                )
                r = c.post("/v1/process", json=p)
                assert r.status_code == 422, r.text
                assert "source_url download failed" in r.text, \
                    "unreachable source_url must fail LOUDLY at intake, never fall back to local_path"
        finally:
            settings.set(old)

    def test_non_http_scheme_rejected_before_open(self, dl_client, fixture_dirs):
        """file:// (or any non-http scheme) must 422 before urlopen — no
        local file is ever read through source_url."""
        client, work = dl_client
        src = _pdf(fixture_dirs["sources"])
        # Point at a file that EXISTS and would pass a naive read; only the
        # scheme guard can reject it.
        p = _payload(f"file://{src}", src, "job-scheme-1", local_path=str(src))
        r = client.post("/v1/process", json=p)
        assert r.status_code == 422, r.text
        assert "scheme" in r.json()["detail"].lower(), r.json()["detail"]
        incoming = work / ".incoming"
        assert not incoming.exists() or not any(incoming.glob("*/*")), \
            "scheme rejection must not leave temp residue"

    def test_job_id_collision_409_without_download(self, dl_client, fixture_dirs):
        """Same job_id, different idempotency key → 409 BEFORE any download
        (no .incoming residue, no bandwidth spent)."""
        client, work = dl_client
        src = _pdf(fixture_dirs["sources"])
        with _SourceServer(fixture_dirs["sources"]) as srv:
            r1 = client.post("/v1/process", json=_payload(srv.url, src, "job-coll-1"))
            assert r1.status_code == 202, r1.text
            p2 = _payload(srv.url, src, "job-coll-1")
            p2["idempotency_key"] = "different-key"  # same job_id, other key
            r2 = client.post("/v1/process", json=p2)
        assert r2.status_code == 409, r2.text
        incoming = work / ".incoming"
        assert not incoming.exists() or not any(incoming.glob("*/*")), \
            "collision must not re-download the source"

    def test_download_total_budget_timeout(self, tmp_path, fixture_dirs):
        """A hanging sender must 422 within the TOTAL download budget — the
        deadline loop (plus socket backstop) gives up, cleans up."""
        work = tmp_path / "budget-work"
        work.mkdir(parents=True, exist_ok=True)
        src = _pdf(fixture_dirs["sources"])
        old = settings.get()
        settings.set(
            Settings(
                work_root=work,
                allowed_source_roots=(),
                source_download_timeout=0.5,
            )
        )
        try:
            with TestClient(app_mod.app) as c:
                with _SourceServer(fixture_dirs["sources"], delay=10.0) as srv:
                    t0 = time.monotonic()
                    r = c.post(
                        "/v1/process", json=_payload(srv.url, src, "job-budget-1")
                    )
                    elapsed = time.monotonic() - t0
                assert r.status_code == 422, r.text
                assert elapsed < 5.0, f"gave up too slowly: {elapsed:.1f}s"
                incoming = work / ".incoming"
                assert not incoming.exists() or not any(incoming.glob("*/*")), \
                    "timeout must clean the temp download"
        finally:
            settings.set(old)
