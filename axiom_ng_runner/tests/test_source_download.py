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
                # #250 probe: budget exhaustion must carry the BUDGET
                # reason — never a transport/retry-exhaustion shape.
                detail = r.json()["detail"].lower()
                assert "budget" in detail, detail
                assert "attempts" not in detail, detail
                incoming = work / ".incoming"
                assert not incoming.exists() or not any(incoming.glob("*/*")), \
                    "timeout must clean the temp download"
        finally:
            settings.set(old)


class TestDownloadRetries250:
    """#250: internal retries with backoff for the Errno-60 class — a
    transient starvation must not burn a full job attempt.

    Mutation-bars:
    - retry loop kappen (max_attempts=1) → retry test red
    - budget/cap/HTTP-status must stay single-shot honest deaths
    """

    def test_single_transport_failure_retried_internally(
        self, dl_client, fixture_dirs, caplog
    ):
        """First GET dies with a transport-class failure (connection
        reset mid-stream), second GET succeeds → job completes and the
        intake happens on ONE job attempt (retry lived inside the
        download, observable in the log)."""
        import logging

        client, _ = dl_client
        src = _real_pdf(fixture_dirs["work"], fixture_dirs["pdf"])
        attempts = {"n": 0}

        # Deterministic: patch _download_attempt to fail ONCE with a
        # ConnectionResetError (Errno-60 class), then serve normally.
        real_attempt = app_mod._download_attempt

        def flaky_attempt(url, dest, deadline, budget, cap):
            attempts["n"] += 1
            if attempts["n"] == 1:

                raise app_mod._TransportError(
                    ConnectionResetError(104, "Connection reset by peer")
                )
            return real_attempt(url, dest, deadline, budget, cap)

        from unittest import mock

        with (
            mock.patch.object(app_mod, "_download_attempt", flaky_attempt),
            _SourceServer(fixture_dirs["work"]) as srv,
            caplog.at_level(logging.WARNING),
        ):
            r = client.post(
                "/v1/process", json=_payload(srv.url, src, "job-retry-1")
            )
        assert r.status_code == 202, r.text
        terminal = _wait_terminal(client, "job-retry-1")
        assert terminal["status"] == "completed", terminal
        assert attempts["n"] == 2, "exactly one internal retry must occur"
        assert any(
            "attempt 1/3 failed" in rec.message for rec in caplog.records
        ), "the retry must be observable in the runner log"

    def test_retry_exhaustion_is_loud_after_3(self, dl_client, fixture_dirs):
        """All three internal attempts fail → ONE 422 carrying the attempt
        count (the job-retry ladder still exists above this; the internal
        ladder just must not die on attempt 1)."""
        client, _ = dl_client
        src = _pdf(fixture_dirs["sources"])
        attempts = {"n": 0}

        def always_transport(url, dest, deadline, budget, cap):
            attempts["n"] += 1

            raise app_mod._TransportError(
                TimeoutError(60, "Operation timed out")
            )

        from unittest import mock

        with mock.patch.object(app_mod, "_download_attempt", always_transport):
            p = _payload("http://127.0.0.1:9/x", src, "job-retry-2")
            r = client.post("/v1/process", json=p)
        assert r.status_code == 422, r.text
        assert "3 attempts" in r.json()["detail"], r.json()["detail"]
        assert attempts["n"] == 3

    def test_http_status_not_retried(self, dl_client, fixture_dirs):
        """A 503 source dies on attempt 1 — HTTP status errors are not the
        Errno-60 class; retrying them would mask a broken source."""
        client, _ = dl_client
        src = _pdf(fixture_dirs["sources"])
        attempts = {"n": 0}
        real_attempt = app_mod._download_attempt

        def counting(url, dest, deadline, budget, cap):
            attempts["n"] += 1
            return real_attempt(url, dest, deadline, budget, cap)

        from unittest import mock

        with (
            mock.patch.object(app_mod, "_download_attempt", counting),
            _SourceServer(fixture_dirs["sources"], serve=False) as srv,
        ):
            r = client.post(
                "/v1/process", json=_payload(srv.url, src, "job-retry-3")
            )
        assert r.status_code == 422, r.text
        assert attempts["n"] == 1, "HTTP status deaths must stay single-shot"
        # urllib raises HTTPError from urlopen (never reaches the status
        # read); the 503 must surface in the detail either way.
        assert "503" in r.json()["detail"], r.json()["detail"]

    def test_default_budget_is_600(self):
        """#250: the default budget is 600s (config), env override intact."""
        from axiom_ng_runner.config import Settings

        assert Settings().source_download_timeout == 600.0


class TestHardBudgetAndCounter:
    """#250 review round 2: hard budget (a hanging read at the deadline
    edge may block at most until the deadline, never budget seconds past
    it) and the REAL attempt counter in message and log.

    Mutation-bars:
    - urlopen timeout fixed to the total budget (instead of the remaining
      deadline) + no socket re-arm → hanging-read test RED (takes ~2x)
    - attempts_run replaced with max_attempts → counter test RED
    """

    def test_hanging_read_at_deadline_edge_bounded(self, tmp_path):
        """A sender that drips one chunk, then HANGS mid-stream: the total
        time from start to the honest budget death must be ≈ budget (+small
        slack), NOT ≈ 2x budget — the socket timeout is re-armed to the
        remaining deadline before every read."""
        import http.server
        import threading

        work = tmp_path / "hb-work"
        work.mkdir(parents=True)
        src = _pdf(tmp_path)
        budget = 3.0

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                # Drip chunks for ~2.5s (ALMOST the whole budget), then
                # hang mid-stream. A socket timeout opened at connect for
                # the full budget would let the final hang block ~budget
                # PAST the deadline; the re-armed timeout must fire at it.
                self.send_response(200)
                self.send_header("Content-Length", "100000")
                self.end_headers()
                for _ in range(5):
                    time.sleep(0.5)
                    self.wfile.write(b"z" * 1024)
                    self.wfile.flush()
                time.sleep(60)  # the hang at the deadline edge

            def log_message(self, *a):
                pass

        # ThreadingHTTPServer: the handler hangs by design; shutdown() of a
        # single-threaded HTTPServer would block on the hung handler.
        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        srv.daemon_threads = True
        threading.Thread(target=srv.serve_forever, daemon=True).start()

        old = settings.get()
        settings.set(
            Settings(
                work_root=work,
                allowed_source_roots=(),
                source_download_timeout=budget,
            )
        )
        try:
            import axiom_ng_runner.app as app_mod2
            from axiom_ng_runner.validation import compute_sha256 as _sha

            att = type(
                "A",
                (),
                {
                    "source_url": f"http://127.0.0.1:{srv.server_address[1]}/f",
                    "filename": "book.pdf",
                    # cap matches the declared Content-Length (100000) —
                    # the death under test is the BUDGET, not the size cap
                    "size_bytes": 100000,
                    "content_hash": _sha(src),
                },
            )()
            req = type("R", (), {"attachment": att})()
            t0 = time.monotonic()
            try:
                app_mod2._download_source(req)
                raise AssertionError("must die with the budget reason")
            except Exception as e:  # noqa: BLE001 — SourceError shape asserted
                assert "budget" in str(e).lower() or "attempts" in str(e), e
            elapsed = time.monotonic() - t0
            assert elapsed < budget + 1.5, (
                f"hanging read escaped the deadline: {elapsed:.1f}s "
                f"(budget {budget}s) — socket timeout must track the "
                f"remaining deadline"
            )
        finally:
            settings.set(old)
            srv.shutdown()

    def test_attempt_count_is_real_not_max(self, tmp_path, monkeypatch, caplog):
        """Budget ends the retry ladder after FEWER than max attempts →
        the death message and the log must carry the REAL count (1), not
        the max (3)."""
        import logging

        import axiom_ng_runner.app as app_mod2
        from axiom_ng_runner.config import Settings

        work = tmp_path / "cnt-work"
        work.mkdir(parents=True)
        old = settings.get()
        settings.set(
            Settings(
                work_root=work,
                allowed_source_roots=(),
                source_download_timeout=0.2,  # no budget for a 2nd attempt
            )
        )
        ran = {"n": 0}

        def transport_then_no_budget(url, dest, deadline, budget, cap):
            ran["n"] += 1
            raise app_mod2._TransportError(
                ConnectionResetError(104, "Connection reset by peer")
            )

        monkeypatch.setattr(app_mod2, "_download_attempt", transport_then_no_budget)
        try:
            att = type("A", (), {
                "source_url": "http://127.0.0.1:9/x",
                "filename": "book.pdf",
                "size_bytes": 10,
                "content_hash": "sha256:" + "0" * 64,
            })()
            req = type("R", (), {"attachment": att})()
            import logging

            import pytest as _pytest

            with (
                caplog.at_level(logging.WARNING, logger="axiom_ng_runner.app"),
                _pytest.raises(Exception) as ctx,
            ):
                app_mod2._download_source(req)
            detail = str(ctx.value)
            assert "after 1 attempt:" in detail, detail
            assert "3 attempts" not in detail, detail
            assert ran["n"] == 1, (
                f"budget (0.2s, backoff 1s) must end the ladder early, "
                f"ran {ran['n']}"
            )
            assert any(
                "after 1 attempt" in rec.message for rec in caplog.records
            ), "the log must carry the real attempt count too"
        finally:
            settings.set(old)


class TestBudgetFloorR3:
    """#250 review r3: the connect/open timeout floor must be tiny. With
    ~100ms of budget left, urlopen may run at most ~100ms (+small slack),
    never a 1s floor past the deadline.

    Mutation-bar: floor restored to 1.0 -> RED (elapsed ~1s for a 0.1s
    remaining budget)."""

    def test_tiny_remaining_budget_connect_bounded(self, tmp_path):
        import http.server
        import threading

        work = tmp_path / "floor-work"
        work.mkdir(parents=True)
        src = _pdf(tmp_path)

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                time.sleep(60)  # hanging CONNECT response

            def log_message(self, *a):
                pass

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        srv.daemon_threads = True
        threading.Thread(target=srv.serve_forever, daemon=True).start()

        old = settings.get()
        # 0.1s budget total: EVERYTHING (connect included) must fit in it
        settings.set(
            Settings(
                work_root=work,
                allowed_source_roots=(),
                source_download_timeout=0.1,
            )
        )
        try:
            import axiom_ng_runner.app as app_mod3
            from axiom_ng_runner.validation import compute_sha256 as _sha

            att = type("A", (), {
                "source_url": f"http://127.0.0.1:{srv.server_address[1]}/f",
                "filename": "book.pdf",
                "size_bytes": 10 ** 9,
                "content_hash": _sha(src),
            })()
            req = type("R", (), {"attachment": att})()
            t0 = time.monotonic()
            try:
                app_mod3._download_source(req)
                raise AssertionError("must die")
            except Exception as e:  # noqa: BLE001
                assert "budget" in str(e).lower() or "attempts" in str(e), e
            elapsed = time.monotonic() - t0
            assert elapsed < 0.6, (
                f"a 0.1s budget must bound the connect too: {elapsed:.1f}s "
                f"— the timeout floor must be tiny, not 1s"
            )
        finally:
            settings.set(old)
            srv.shutdown()


class TestLabelTreeE2E251:
    """#251 E2E findings: (a) a spec-legal PageLabels tree whose first
    entry starts at page > 0 must not crash the chunker (pymupdf's
    get_label util raises IndexError for uncovered leading pages — found
    re-ingesting the healed 2LMELTV5); (b) the FIXER's write_page_labels
    must emit a covering entry for leading unnamed pages so healed files
    are consumable by every reader.

    Mutation-bars:
    - chunker guard removed -> uncovered-tree test RED (IndexError)
    - fixer covering entry removed -> kernel test RED"""

    @staticmethod
    def _pdf_with_uncovered_tree(path, n=6):
        import pymupdf

        doc = pymupdf.open()
        for _ in range(n):
            doc.new_page()
        # first entry starts at page 1 (page 0 uncovered) — spec-legal
        doc.set_page_labels([{"startpage": 1, "prefix": "", "style": "D",
                              "firstpagenum": 2}])
        doc.save(path)
        doc.close()

    def test_chunker_survives_uncovered_label_tree(self, tmp_path):
        from axiom_ng_runner.chunking import extract_page_labels

        p = tmp_path / "uncovered.pdf"
        self._pdf_with_uncovered_tree(p)
        labels = extract_page_labels(str(p))  # must NOT raise
        assert labels.get(1) == "2", labels  # tier-1 for covered pages

    def test_fixer_kernel_writes_covering_entry(self, tmp_path):
        """Leading unnamed page + closed run: the written tree must give
        page 0 an explicit empty label (no crash in ANY consumer)."""
        import sys
        from pathlib import Path as _P

        # repo-relative, NEVER a session worktree path (#233 hermeticity):
        tools = (
            _P(__file__).resolve().parents[2]
            / "axiom_ng"
            / "tools"
            / "pdf_repair_agent"
            / "tools"
        )
        sys.path.insert(0, str(tools))
        import pdf_kernel

        src = tmp_path / "k.pdf"
        self._pdf_with_uncovered_tree(src, n=4)
        labels = ["", "2", "3", "4"]
        pdf_kernel.write_page_labels(src, labels)
        import pymupdf

        doc = pymupdf.open(src)
        got = [doc[i].get_label() for i in range(doc.page_count)]
        doc.close()
        assert got == labels, got


class TestRetryAtDeadlineEdge:
    """#251 r5 witness test — the review's classic case, now PINNED:

    Attempt 2 starts at deadline-eps (remaining budget far below the
    total budget). With the hard budget the connect runs at most until
    the deadline; with the MUTATION (fixed timeout=budget at open, no
    remaining-deadline open) the second attempt may block a FULL budget
    past its start — the suite stayed green there, so this witness is
    the regression pin.

    Mutation-bar: urlopen opened with timeout=budget -> RED."""

    def test_second_attempt_bounded_by_remaining_budget(self, tmp_path):
        import http.server
        import threading

        work = tmp_path / "w-work"
        work.mkdir(parents=True)
        budget = 2.0

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                time.sleep(60)  # hanging connect response

            def log_message(self, *a):
                pass

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        srv.daemon_threads = True
        threading.Thread(target=srv.serve_forever, daemon=True).start()

        old = settings.get()
        settings.set(
            Settings(
                work_root=work,
                allowed_source_roots=(),
                source_download_timeout=budget,
            )
        )
        real_attempt = app_mod._download_attempt
        calls = {"n": 0}

        def fail_first_then_real(url, dest, deadline, budget_, cap):
            calls["n"] += 1
            if calls["n"] == 1:
                # instant transport failure: retry ladder starts attempt 2
                # after the 1s backoff, i.e. at deadline-eps
                raise app_mod._TransportError(
                    ConnectionResetError(104, "Connection reset by peer")
                )
            return real_attempt(url, dest, deadline, budget_, cap)

        import axiom_ng_runner.app as app_mod2

        orig = app_mod2._download_attempt
        app_mod2._download_attempt = fail_first_then_real
        try:
            att = type("A", (), {
                "source_url": f"http://127.0.0.1:{srv.server_address[1]}/f",
                "filename": "book.pdf",
                "size_bytes": 10 ** 9,
                "content_hash": "sha256:" + "0" * 64,
            })()
            req = type("R", (), {"attachment": att})()
            t0 = time.monotonic()
            try:
                app_mod2._download_source(req)
                raise AssertionError("must die")
            except Exception as e:  # noqa: BLE001
                assert "budget" in str(e).lower() or "attempts" in str(e), e
            elapsed = time.monotonic() - t0
            assert calls["n"] == 2
            # attempt 2 starts at ~1.0s (backoff); remaining ~1.0s << budget
            # 2.0s. Hard budget: death at ~2.0s total. Mutated open
            # (timeout=budget): the hanging connect blocks until ~3.0s.
            assert elapsed < budget + 0.8, (
                f"retry-at-deadline-edge escaped: {elapsed:.1f}s total for "
                f"a {budget:.0f}s budget — attempt 2 must open with the "
                f"REMAINING budget, not the total"
            )
        finally:
            app_mod2._download_attempt = orig
            settings.set(old)
            srv.shutdown()
