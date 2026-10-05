#!/usr/bin/env python3
"""Probe the ci.yml #354 path-gate classifier against synthetic file lists.

Extracts the `classify changed files` step verbatim from the workflow and
runs it with a stubbed `git` binary (no checkout needed), so the case
patterns that gate go-db-it / the topology jobs are asserted, not
trusted. A regression here silently skips verification legs.
"""

import os
import stat
import subprocess
import sys
import tempfile

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
WORKFLOW = os.path.join(HERE, "..", ".github", "workflows", "ci.yml")


def classify(files):
    """Run the real classifier step with `git` stubbed to emit `files`."""
    doc = yaml.safe_load(open(WORKFLOW))
    step = next(s for s in doc["jobs"]["changes"]["steps"] if s.get("id") == "diff")
    with tempfile.TemporaryDirectory() as tmp:
        stub = os.path.join(tmp, "git")
        with open(stub, "w") as fh:
            fh.write("#!/bin/sh\nprintf '%s\\n' $AXIOM_STUB_FILES\n")
        os.chmod(stub, os.stat(stub).st_mode | stat.S_IXUSR)
        out = os.path.join(tmp, "out")
        env = dict(
            os.environ,
            PATH=tmp + os.pathsep + os.environ.get("PATH", "/usr/bin:/bin"),
            AXIOM_STUB_FILES="\n".join(files),
            GITHUB_EVENT_NAME="push",
            BEFORE="0" * 40,
            GITHUB_OUTPUT=out,
        )
        open(out, "w").close()
        subprocess.run(["bash", "-c", step["run"]], env=env, check=True)
        result = dict(line.split("=", 1) for line in open(out).read().splitlines())
    return result["go_sql"], result["topology"]


CASES = [
    # (label, files, expected (go_sql, topology))
    ("docs-only", ["docs/operations/backup.md", "README.md"], ("false", "false")),
    ("script-only", ["scripts/dev/env.sh"], ("false", "false")),
    ("go source", ["axiom/internal/db/store.go"], ("true", "true")),
    ("go.mod", ["axiom/go.mod"], ("true", "true")),
    ("pg schema sql", ["axiom/internal/db/schema/0018_ingest_jobs_quality.sql"], ("true", "true")),
    ("role drill sql", ["deploy/postgres/roles.sql"], ("true", "true")),
    ("compose change", ["deploy/container/compose.topology.yml"], ("false", "true")),
    ("worker change", ["axiom-compute-worker/main.py"], ("false", "true")),
    ("workflow self", [".github/workflows/ci.yml"], ("true", "true")),
    ("mixed docs+go", ["docs/index.md", "axiom/cmd/axiom-ng/main.go"], ("true", "true")),
]


def main():
    failed = 0
    for label, files, expected in CASES:
        got = classify(files)
        ok = got == expected
        failed += not ok
        print(f"{'ok  ' if ok else 'FAIL'} {label:16} files={files} -> {got} (want {expected})")
    if failed:
        sys.exit(f"{failed} classifier case(s) red")
    print("all classifier cases green")


if __name__ == "__main__":
    main()
