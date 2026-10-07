#!/usr/bin/env python3
"""Probe the ci.yml #354 path-gate classifier against synthetic file lists.

Extracts the `classify changed files` step verbatim from the workflow and
runs it with a stubbed `git` binary (no checkout needed), so the case
patterns that gate go-db-it / the topology jobs are asserted, not
trusted. A regression here silently skips verification legs — including
the fail-open fallback (diff base unresolvable => everything changed).
"""

import os
import stat
import subprocess
import sys
import tempfile

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
WORKFLOW = os.path.join(HERE, "..", ".github", "workflows", "ci.yml")

STUB = """#!/bin/sh
printf '%s\\n' "$@" >> "$AXIOM_STUB_ARGS"
case "$1" in
  diff)
    [ "$AXIOM_STUB_DIFF_OK" = 1 ] || exit 1
    printf '%s\\n' $AXIOM_STUB_FILES
    ;;
  *)
    if [ -n "$AXIOM_STUB_LSFILES" ]; then
      printf '%s\\n' $AXIOM_STUB_LSFILES
    else
      printf '%s\\n' $AXIOM_STUB_FILES
    fi
    ;;
esac
"""


def classify(files, diff_ok=True, lsfiles=None, event="push", base_ref=""):
    """Run the real classifier step with `git` stubbed to emit `files`."""
    doc = yaml.safe_load(open(WORKFLOW))
    step = next(s for s in doc["jobs"]["changes"]["steps"] if s.get("id") == "diff")
    with tempfile.TemporaryDirectory() as tmp:
        stub = os.path.join(tmp, "git")
        with open(stub, "w") as fh:
            fh.write(STUB)
        os.chmod(stub, os.stat(stub).st_mode | stat.S_IXUSR)
        out = os.path.join(tmp, "out")
        args = os.path.join(tmp, "args")
        open(out, "w").close()
        env = dict(
            os.environ,
            PATH=tmp + os.pathsep + os.environ.get("PATH", "/usr/bin:/bin"),
            AXIOM_STUB_FILES="\n".join(files),
            AXIOM_STUB_LSFILES="\n".join(lsfiles) if lsfiles else "",
            AXIOM_STUB_DIFF_OK="1" if diff_ok else "0",
            AXIOM_STUB_ARGS=args,
            GITHUB_EVENT_NAME=event,
            BEFORE="0" * 40,
            BASE_REF=base_ref,
            GITHUB_OUTPUT=out,
        )
        subprocess.run(["bash", "-c", step["run"]], env=env, check=True)
        result = dict(line.split("=", 1) for line in open(out).read().splitlines())
        called = open(args).read().splitlines()
    return result["go_sql"], result["topology"], called


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
    # the fixer's own top-level home: the always-on fixer-pytest job owns
    # it — a fixer-only change must not fire the Go heavy legs
    ("fixer-only", ["axiom-fixer/repair_agent.py"], ("false", "false")),
    ("fixer tests", ["axiom-fixer/tests/test_ocr_tool.py"], ("false", "false")),
    ("db test fixture", ["axiom/internal/backfill/testdata/poisoned.epub"], ("true", "true")),
    ("axiom docs prose (fail-open over-trigger)", ["axiom/docs/CITATION_GRANULARITY_MEMO.md"], ("true", "true")),
    ("workflow self", [".github/workflows/ci.yml"], ("true", "true")),
    ("mixed docs+go", ["docs/index.md", "axiom/cmd/axiom-ng/main.go"], ("true", "true")),
]


def main():
    failed = 0
    for label, files, expected in CASES:
        go_sql, topology, _ = classify(files)
        got = (go_sql, topology)
        ok = got == expected
        failed += not ok
        print(f"{'ok  ' if ok else 'FAIL'} {label:16} files={files} -> {got} (want {expected})")

    # Fail-open: an unresolvable diff base must classify from ls-files
    # (everything tracked => heavy legs run), never skip verification.
    # The subcommand pin matters: the stub's wildcard arm would answer
    # ANY non-diff subcommand, so only asserting the outputs would not
    # prove the fallback actually used ls-files.
    go_sql, topology, called = classify(
        ["docs/only.md"], diff_ok=False,
        lsfiles=["README.md", "axiom/cmd/x/main.go"])
    ok = (go_sql, topology) == ("true", "true") and "ls-files" in called
    failed += not ok
    print(f"{'ok  ' if ok else 'FAIL'} fail-open fallback -> "
          f"{(go_sql, topology)} subcommand={'ls-files' if 'ls-files' in called else 'MISSING'} "
          f"(want ('true', 'true') subcommand=ls-files)")

    # The push arm must diff BEFORE...HEAD (github.event.before to the
    # pushed tip) — the harness stubs BEFORE as the 40-zero SHA.
    _, _, called = classify(["axiom/internal/db/store.go"])
    want = "0" * 40 + "...HEAD"
    ok = called[-1] == want
    failed += not ok
    print(f"{'ok  ' if ok else 'FAIL'} push range -> {called[-1]} (want {want})")

    # The pull_request arm must build the merge-base range against the
    # PR base ref (a typo here fails loudly at git-diff time, but only
    # after billing a fail-open full run).
    _, _, called = classify(["docs/only.md"], event="pull_request", base_ref="main")
    ok = called[-1] == "origin/main...HEAD"
    failed += not ok
    print(f"{'ok  ' if ok else 'FAIL'} pull_request range -> {called[-1]} (want origin/main...HEAD)")

    if failed:
        sys.exit(f"{failed} classifier case(s) red")
    print("all classifier cases green")


if __name__ == "__main__":
    main()
