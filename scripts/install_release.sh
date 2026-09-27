#!/bin/sh
# install_release.sh — G3 of #205: production install path.
# Downloads an artifact from GitHub Releases, verifies the sha256 sidecar,
# and installs into /opt/axiom via the SAME operator-gated flow as
# `make install` (scripts/install_dist.sh does the /opt work).
#
# THE production path: /opt receives artifacts ONLY from GitHub releases,
# never from a developer's /tmp or an unverified dist/ (owner ruling,
# 2026-08-23 debug-build incident).
#
# Usage: scripts/install_release.sh <rag|compute-worker|runner|fixer> <tag-version> [--skip-pull]
#   --skip-pull: reuse an already-downloaded dist/ artifact (offline verify).
set -eu

component="${1:?usage: install_release.sh <rag|compute-worker|runner|fixer> <version> [--skip-pull]}"
version="${2:?usage: install_release.sh <rag|compute-worker|runner|fixer> <version> [--skip-pull]}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$HERE/dist"
cd "$HERE" # install_dist.sh works relative to the repo root — run from any cwd
REPO="${AXIOM_RELEASE_REPO:-Cyb3rDudu/axiom}"

case "$component" in
rag)
    # F05 (#299): alias + canonical binary, same release generation.
    patterns="axiom-ng-$version-* axiom-$version-*"
    ;;
compute-worker | runner)
    # canonical component name is compute-worker (F10 #304, ADR 0001
    # §4); "runner" stays accepted as the 0.1.x spelling — one
    # deprecation echo, then the identical flow (mirrors
    # scripts/install_dist.sh). Pre-F10 releases ship the artifact under
    # the legacy name; both patterns are offered, only one can match.
    case "$component" in
    runner) echo "axiom: 'runner' is deprecated — use 'compute-worker' (ADR 0001: docs/adr/0001-canonical-naming.md)" >&2 ;;
    esac
    patterns="axiom-compute-worker-$version-*.tar.zst axiom-runner-$version-*.tar.zst"
    component=compute-worker
    ;;
fixer) patterns="axiom-fixer-$version-*.tar.zst" ;;
*)
    echo "unknown component '$component' (rag|compute-worker|runner|fixer)"
    exit 2
    ;;
esac

if [ "${3:-}" != "--skip-pull" ]; then
    mkdir -p "$DIST"
    # shellcheck disable=SC2086 # patterns are word-split by design
    echo "release: fetching $patterns from $REPO release $version"
    dl_args=""
    for p in $patterns; do
        dl_args="$dl_args --pattern $p --pattern $p.sha256"
    done
    # shellcheck disable=SC2086
    gh release download "$version" --repo "$REPO" $dl_args --clobber --dir "$DIST"
fi

# checksum verify BEFORE handing off to the gated installer
found=""
# shellcheck disable=SC2086
for f in $DIST/$patterns; do
    [ -f "$f" ] || continue
    case "$f" in *.sha256) continue ;; esac
    found="$f"
    (cd "$(dirname "$f")" && shasum -a 256 -c "$(basename "$f").sha256") || {
        echo "checksum FAILED for $f — refusing to install"
        exit 1
    }
done
[ -n "$found" ] || {
    echo "no artifact matching $patterns in $DIST/"
    exit 1
}

exec "$HERE/scripts/install_dist.sh" "$component" "$version"
