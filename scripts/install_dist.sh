#!/bin/sh
# install_dist.sh — `make install` backend (#205 §1/§2).
# Stages dist/ artifacts into /opt/axiom/<component>/<version>/ with an
# atomically-switched `current` symlink. Operator-confirmed before any
# mutation of /opt; checksum verified BEFORE the prompt.
# G1: rag. G2: runner (conda-pack), fixer (own venv).
set -eu

DIST="dist"
ROOT="/opt/axiom"
component="${1:-}"
version="${2:-}"

usage() { echo "usage: make install   # or: scripts/install_dist.sh <rag|runner|fixer> <version>"; exit 2; }
[ -n "$component" ] && [ -n "$version" ] || usage

# newest artifact matching a pattern (empty -> exit 1)
find_artifact() {
    best=""
    for f in "$DIST"/$1; do
        [ -f "$f" ] || continue
        case "$f" in *.sha256) continue ;; esac
        [ -z "$best" ] || [ "$f" \> "$best" ] && best="$f"
        :
    done
    [ -n "$best" ] || return 1
    printf '%s\n' "$best"
}

case "$component" in
rag)
    bin=$(find_artifact "axiom-ng-$version-*") || { echo "no rag artifact for $version in $DIST/ — run: make rag"; exit 1; }
    shasum -a 256 -c "$bin.sha256" 2>/dev/null || { echo "checksum FAILED for $bin"; exit 1; }
    target="$ROOT/rag/$version"
    echo "component: rag"
    echo "  artifact: $bin ($(cat "$bin.sha256"))"
    echo "  target:   $target/axiom-ng"
    echo "  current:  $ROOT/rag/current -> $version"
    echo "  shim:     $ROOT/bin/axiom-ng"
    echo "This will create directories under $ROOT. Proceed? [yes/No]"
    read -r answer
    [ "$answer" = "yes" ] || { echo "aborted"; exit 1; }
    mkdir -p "$target" "$ROOT/bin"
    cp "$bin" "$target/axiom-ng"
    chmod 0755 "$target/axiom-ng"
    ln -sfn "$version" "$ROOT/rag/current"
    ln -sfn "$ROOT/rag/current/axiom-ng" "$ROOT/bin/axiom-ng"
    echo "installed: $ROOT/bin/axiom-ng ($version)"
    ;;
runner)
    art=$(find_artifact "axiom-runner-$version-*.tar.zst") || { echo "no runner artifact for $version in $DIST/ — run: make runner"; exit 1; }
    shasum -a 256 -c "$art.sha256" 2>/dev/null || { echo "checksum FAILED for $art"; exit 1; }
    target="$ROOT/runner/$version"
    echo "component: runner"
    echo "  artifact: $art ($(cat "$art.sha256"))"
    echo "  target:   $target/{env,app}"
    echo "  current:  $ROOT/runner/current -> $version"
    echo "  shim:     $ROOT/bin/axiom-runner"
    echo "  post-install fixup: env/bin/conda-unpack (once)"
    echo "This will create directories under $ROOT. Proceed? [yes/No]"
    read -r answer
    [ "$answer" = "yes" ] || { echo "aborted"; exit 1; }
    mkdir -p "$ROOT/runner" "$ROOT/bin"
    rm -rf "$target"
    mkdir -p "$target"
    tar --zstd -xf "$art" -C "$target" --strip-components 1
    "$target/env/bin/conda-unpack"
    ln -sfn "$version" "$ROOT/runner/current"
    ln -sfn "$ROOT/runner/current/env/bin/axiom-runner" "$ROOT/bin/axiom-runner"
    echo "installed: $ROOT/bin/axiom-runner ($version)"
    ;;
fixer)
    art=$(find_artifact "axiom-fixer-$version-*.tar.zst") || { echo "no fixer artifact for $version in $DIST/ — run: make fixer"; exit 1; }
    shasum -a 256 -c "$art.sha256" 2>/dev/null || { echo "checksum FAILED for $art"; exit 1; }
    buildprefix=$(tar --zstd -tf "$art" "fixer-$version/env/bin/fix-env" >/dev/null 2>&1 && sed -n 's/^usage: fix-env //p' /dev/null; true)
    target="$ROOT/fixer/$version"
    echo "component: fixer"
    echo "  artifact: $art ($(cat "$art.sha256"))"
    echo "  target:   $target/{env,app}"
    echo "  current:  $ROOT/fixer/current -> $version"
    echo "  shim:     $ROOT/bin/axiom-fixer"
    echo "  post-install fixup: env/bin/fix-env <build-prefix> (once)"
    echo "  host deps (NOT bundled): tesseract5 +deu, ghostscript on PATH"
    echo "This will create directories under $ROOT. Proceed? [yes/No]"
    read -r answer
    [ "$answer" = "yes" ] || { echo "aborted"; exit 1; }
    mkdir -p "$ROOT/fixer" "$ROOT/bin"
    rm -rf "$target"
    mkdir -p "$target"
    tar --zstd -xf "$art" -C "$target" --strip-components 1
    # one-time venv relocation fixup, build prefix recorded by the build script
    bp=$(cat "$target/env/.build-prefix")
    "$target/env/bin/fix-env" "$bp"
    cat > "$ROOT/bin/axiom-fixer" <<EOF
#!/bin/sh
exec "$ROOT/fixer/current/env/bin/python" "$ROOT/fixer/current/app/repair_agent.py" "\$@"
EOF
    chmod +x "$ROOT/bin/axiom-fixer"
    ln -sfn "$version" "$ROOT/fixer/current"
    echo "installed: $ROOT/bin/axiom-fixer ($version)"
    ;;
*)
    echo "unknown component '$component' (rag|runner|fixer)"
    exit 1
    ;;
esac
echo "rollback:  ln -sfn <prev-version> $ROOT/$component/current && launchctl kickstart -k gui/\$(id -u)/com.axiom.$component"
