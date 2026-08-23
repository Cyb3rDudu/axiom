#!/bin/sh
# install_dist.sh — `make install` backend (#205 §1/§2).
# Stages dist/ artifacts into /opt/axiom/<component>/<version>/ with an
# atomically-switched `current` symlink. Operator-confirmed before any
# mutation of /opt. G1 scope: rag only; G2 adds runner/fixer.
set -eu

DIST="dist"
ROOT="/opt/axiom"
component="${1:-}"

usage() {
    echo "usage: make install   # or: scripts/install_dist.sh rag <version>"
    exit 2
}

# discover artifacts for a component in dist/; prints the newest (version-
# sorted) non-checksum candidate, or fails when none exists. The [ -f ]
# guard matters: POSIX sh leaves an unmatched glob literal, which would
# otherwise prompt to install a phantom path.
find_artifact() {
    list=""
    for f in "$DIST"/axiom-ng-*; do
        [ -f "$f" ] || continue
        case "$f" in *.sha256) continue ;; esac
        list="$list$f
"
    done
    [ -n "$list" ] || return 1
    printf '%s' "$list" | sort | tail -n 1
}

[ -n "$component" ] || usage

if [ "$component" = "rag" ]; then
    version="${2:-}"
    [ -n "$version" ] || {
        echo "rag: version argument required (make install passes it)"
        usage
    }
    if ! bin=$(find_artifact); then
        echo "no rag artifact in $DIST/ — run: make rag"
        exit 1
    fi
    target="$ROOT/rag/$version"
    # fail fast: verify the checksum sidecar BEFORE prompting the operator
    if [ ! -f "$bin.sha256" ]; then
        echo "checksum sidecar missing: $bin.sha256"
        exit 1
    fi
    shasum -a 256 -c "$bin.sha256"
    echo "component: rag"
    echo "  artifact: $bin"
    echo "  checksum: $(cat "$bin.sha256")"
    echo "  target:   $target/axiom-ng"
    echo "  current:  $ROOT/rag/current -> $version"
    echo "  shim:     $ROOT/bin/axiom-ng"
    echo "This will create directories under $ROOT. Proceed? [yes/No]"
    read -r answer
    [ "$answer" = "yes" ] || {
        echo "aborted"
        exit 1
    }
    mkdir -p "$target" "$ROOT/bin"
    cp "$bin" "$target/axiom-ng"
    chmod 0755 "$target/axiom-ng"
    ln -sfn "$version" "$ROOT/rag/current"
    ln -sfn "$ROOT/rag/current/axiom-ng" "$ROOT/bin/axiom-ng"
    echo "installed: $ROOT/bin/axiom-ng -> $ROOT/rag/current/axiom-ng ($version)"
    echo "rollback:  ln -sfn <prev-version> $ROOT/rag/current"
else
    echo "component '$component' packaging lands in G2 (#205)"
    exit 1
fi
