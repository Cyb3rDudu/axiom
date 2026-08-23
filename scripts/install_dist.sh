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
    echo "usage: make install   # or: scripts/install_dist.sh <rag|runner|fixer>"
    exit 2
}

# discover artifacts for a component in dist/
find_artifact() {
    for f in "$DIST"/axiom-ng-*; do
        case "$f" in *.sha256) ;; *)
            printf '%s\n' "$f"
            return 0
            ;;
        esac
    done
    return 1
}

[ -n "$component" ] || usage

if [ "$component" = "rag" ]; then
    bin=$(find_artifact)
    [ -n "$bin" ] || {
        echo "no rag artifact in $DIST/ — run: make rag"
        exit 1
    }
    name=$(basename "$bin")
    version=$(printf '%s' "$name" | sed -e 's/^axiom-ng-//' -e 's/-darwin-arm64$//' -e 's/-[a-z]*-[a-z0-9]*$//')
    target="$ROOT/rag/$version"
    echo "component: rag"
    echo "  artifact: $bin"
    echo "  checksum: $bin.sha256 ($(cat "$bin.sha256" 2>/dev/null || echo MISSING))"
    echo "  target:   $target/axiom-ng"
    echo "  current:  $ROOT/rag/current -> $version"
    echo "  shim:     $ROOT/bin/axiom-ng"
    echo "This will create directories under $ROOT. Proceed? [yes/No]"
    read -r answer
    [ "$answer" = "yes" ] || {
        echo "aborted"
        exit 1
    }
    shasum -a 256 -c "$bin.sha256"
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
