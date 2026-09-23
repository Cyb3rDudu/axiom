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
# Usage: scripts/install_release.sh <rag|runner|fixer> <tag-version> [--skip-pull]
#   --skip-pull: reuse an already-downloaded dist/ artifact (offline verify).
set -eu

component="${1:?usage: install_release.sh <rag|runner|fixer> <version> [--skip-pull]}"
version="${2:?usage: install_release.sh <rag|runner|fixer> <version> [--skip-pull]}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$HERE/dist"
cd "$HERE" # install_dist.sh works relative to the repo root — run from any cwd
REPO="${AXIOM_RELEASE_REPO:-Cyb3rDudu/axiom}"

case "$component" in
rag)
    # F05 (#299): alias + canonical binary, same release generation.
    patterns="axiom-ng-$version-* axiom-$version-*"
    ;;
runner) patterns="axiom-runner-$version-*.tar.zst" ;;
fixer) patterns="axiom-fixer-$version-*.tar.zst" ;;
*)
    echo "unknown component '$component' (rag|runner|fixer)"
    exit 2
    ;;
esac

if [ "${3:-}" != "--skip-pull" ]; then
    mkdir -p "$DIST"
    # shellcheck disable=SC2086 # patterns are word-split by design
    echo "release: fetching$patterns from $REPO release $version"
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
    echo "no artifact matching$patterns in $DIST/"
    exit 1
}

exec "$HERE/scripts/install_dist.sh" "$component" "$version"
