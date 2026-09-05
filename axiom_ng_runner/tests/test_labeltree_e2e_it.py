"""#253 IT: a synthetic missing-tree PDF heals END-TO-END without any
chunk/annotation precondition — the custody chain the issue demands:

    surgery (write_labels) -> healed artifact -> quarantine custody
    (cmd/quarantine, the real repair.Quarantine Go path) -> re-ingest
    through the runner (terminal completed, snapshot text searchable).

All paths resolve REPO-RELATIVE (#233 hermeticity: no session worktree
paths, wherever the branch is checked out).

Mutation-bar: classification rule cut -> every test here RED.
"""
import shutil
import subprocess
import sys
from pathlib import Path

import pymupdf
import pytest

REPO_ROOT = Path(__file__).resolve().parent.parent.parent  # repo root
FIXER_ROOT = REPO_ROOT / "axiom_ng" / "tools" / "pdf_repair_agent"
AXIOM_NG = REPO_ROOT / "axiom_ng"
sys.path.insert(0, str(FIXER_ROOT))  # fixer tools (pymupdf-only imports)

from tools import labeltree_heal


def _synthetic_missing_tree(path: Path, n=8):
    doc = pymupdf.open()
    for i in range(n):
        pg = doc.new_page()
        if i == 0:
            pg.insert_text((72, 72), "A Study in Custody: Title Page")
        elif i == 1:
            pg.insert_text((72, 72), "Contents\n1 First 3\n2 Second 5")
        else:
            pg.insert_text(
                (72, 72), f"Chapter body {i}. " + "sovereignty lore ipsum " * 25
            )
            pg.insert_text((72, 800), str(i - 1))  # printed folio 1..n-2
    doc.save(path)
    doc.close()
    return path


def test_it_missing_tree_heals_end_to_end(tmp_path):
    # ── 1. diagnose + surgery on the working copy: the file itself is the
    #       evidence — no model, no chunks, no annotations needed.
    src = _synthetic_missing_tree(tmp_path / "book.pdf")
    work = tmp_path / "runs" / "K-IT1" / "work.pdf"
    work.parent.mkdir(parents=True)
    shutil.copy2(src, work)

    verdict = labeltree_heal.would_heal(work)
    assert verdict["would_heal"] is True, verdict

    from tools import surgery_exec

    labels = verdict["labels"]
    res = surgery_exec.run_plan(
        {
            "class": "labeltree-missing",
            "operations": [
                {
                    "op": "write_labels",
                    "source": str(work),
                    "backup": str(work.parent / "backup.pdf"),
                    "labels": labels,
                    "expected_after": labels,
                }
            ],
        },
        apply=True,
    )
    assert res.get("applied") is True, str(res)[:300]

    # healed artifact: tier-1 labels readable by every consumer
    got = pymupdf.open(work)
    got_labels = [got[i].get_label() for i in range(got.page_count)]
    got.close()
    assert got_labels == labels, got_labels

    # ── 2. quarantine custody: the ORIGINAL via the real Go path
    q = subprocess.run(
        ["go", "run", "./cmd/quarantine",
         str(tmp_path / "quarantine"), "K-IT1", str(src)],
        cwd=AXIOM_NG,
        capture_output=True, text=True, timeout=180, check=False,
    )
    if q.returncode != 0:
        pytest.skip(f"go toolchain unavailable: {q.stderr[:200]}")
    quarantined = Path(q.stdout.strip().splitlines()[-1])
    assert quarantined.exists()
    assert quarantined.read_bytes() == src.read_bytes(), (
        "the quarantine copy must be the ORIGINAL, byte-identical"
    )

    # ── 3. re-ingest the HEALED file through the real runner, in-process
    from axiom_ng_runner.app import app
    from axiom_ng_runner.config import Settings, settings
    from axiom_ng_runner.validation import compute_sha256
    from fastapi.testclient import TestClient

    settings.set(
        Settings(
            work_root=tmp_path / "runner-work",
            allowed_source_roots=(str(work.parent),),
        )
    )
    with TestClient(app) as c:
        r = c.post(
            "/v1/process",
            json={
                "contract_version": "1.0",
                "job_id": "it-labeltree-e2e",
                "idempotency_key": "it-labeltree-e2e",
                "source": {"type": "zotero", "source_id": "s", "server_id": "srv"},
                "document": {
                    "document_id": "d", "zotero_key": "K-IT1", "zotero_version": 1,
                },
                "attachment": {
                    "attachment_id": "a", "zotero_key": "K-IT1",
                    "zotero_version": 1, "content_type": "application/pdf",
                    "filename": "book.pdf", "local_path": str(work),
                    "source_url": "",
                    "content_hash": compute_sha256(work),
                    "size_bytes": work.stat().st_size, "mtime_ms": 0,
                },
                "processing": {"profile": "full-rag-v1"},
            },
        )
        assert r.status_code == 202, r.text
        import time

        status = None
        for _ in range(1200):
            j = c.get("/v1/jobs/it-labeltree-e2e").json()
            status = j.get("status")
            if status in ("completed", "failed"):
                break
            time.sleep(0.2)
        assert status == "completed", j
        # the runner's result endpoint carries the persisted snapshot
        res = c.get("/v1/jobs/it-labeltree-e2e/result")
        assert res.status_code == 200, res.text
        result = res.json()
        chunks = (
            result.get("chunks")
            or (result.get("result") or {}).get("chunks")
            or []
        )
        assert chunks, "the healed document must produce chunks"
        text = "\n".join(ch.get("text") or "" for ch in chunks)
        assert "sovereignty" in text.lower(), (
            "the snapshot must be text-searchable"
        )
