#!/bin/sh
# install_dist.sh — `make install` backend (#205 §1/§2).
# Stages dist/ artifacts into /opt/axiom/<component>/<version>/ with an
# atomically-switched `current` symlink. Operator-confirmed before any
# mutation of /opt; checksum verified BEFORE the prompt.
set -eu

DIST="dist"
ROOT="/opt/axiom"
component="${1:-}"
version="${2:-}"

usage() {
    echo "usage: make install   # or: scripts/install_dist.sh <rag|runner|fixer> <version>"
    exit 2
}
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

# confirm_install <component> <artifact> [plan lines...]
# Verifies the checksum FIRST, prints the install plan, then requires an
# explicit operator "yes" before anything under $ROOT is touched.
confirm_install() {
    comp="$1"
    art="$2"
    shift 2
    shasum -a 256 -c "$art.sha256" 2>/dev/null || {
        echo "checksum FAILED for $art"
        exit 1
    }
    echo "component: $comp"
    echo "  artifact: $art ($(cat "$art.sha256"))"
    for line in "$@"; do
        echo "  $line"
    done
    echo "This will create directories under $ROOT. Proceed? [yes/No]"
    read -r answer
    [ "$answer" = "yes" ] || {
        echo "aborted"
        exit 1
    }
}

case "$component" in
rag)
    bin=$(find_artifact "axiom-ng-$version-*") || {
        echo "no rag artifact for $version in $DIST/ — run: make rag"
        exit 1
    }
    target="$ROOT/rag/$version"
    confirm_install rag "$bin" \
        "target:   $target/axiom-ng" \
        "current:  $ROOT/rag/current -> $version" \
        "shim:     $ROOT/bin/axiom-ng"
    mkdir -p "$target" "$ROOT/bin"
    cp "$bin" "$target/axiom-ng"
    chmod 0755 "$target/axiom-ng"
    ln -sfn "$version" "$ROOT/rag/current"
    ln -sfn "$ROOT/rag/current/axiom-ng" "$ROOT/bin/axiom-ng"
    echo "installed: $ROOT/bin/axiom-ng ($version)"
    ;;
runner)
    art=$(find_artifact "axiom-runner-$version-*.tar.zst") || {
        echo "no runner artifact for $version in $DIST/ — run: make runner"
        exit 1
    }
    target="$ROOT/runner/$version"
    confirm_install runner "$art" \
        "target:   $target/{env,app}" \
        "current:  $ROOT/runner/current -> $version" \
        "shim:     $ROOT/bin/axiom-runner" \
        "post-install fixup: env/bin/conda-unpack (once)"
    mkdir -p "$ROOT/runner" "$ROOT/bin"
    rm -rf "$target"
    mkdir -p "$target"
    tar --zstd -xf "$art" -C "$target" --strip-components 1
    # conda-unpack invoked via the env's own python — PATH-independent
    "$target/env/bin/python" "$target/env/bin/conda-unpack"
    # smoke: import surface must resolve in the FINAL location before the
    # current symlink switches over
    "$target/env/bin/python" -c 'import axiom_ng_runner, torch'
    cat >"$ROOT/bin/axiom-runner" <<EOF
#!/bin/sh
exec "$ROOT/runner/current/env/bin/python" -m axiom_ng_runner "\$@"
EOF
    chmod +x "$ROOT/bin/axiom-runner"
    ln -sfn "$version" "$ROOT/runner/current"
    echo "installed: $ROOT/bin/axiom-runner ($version)"
    echo "rollback:  ln -sfn <prev-version> $ROOT/runner/current && launchctl kickstart -k gui/\$(id -u)/com.axiom.runner"
    ;;
fixer)
    art=$(find_artifact "axiom-fixer-$version-*.tar.zst") || {
        echo "no fixer artifact for $version in $DIST/ — run: make fixer"
        exit 1
    }
    target="$ROOT/fixer/$version"
    confirm_install fixer "$art" \
        "target:   $target/{env,app}" \
        "current:  $ROOT/fixer/current -> $version" \
        "shim:     $ROOT/bin/axiom-fixer" \
        "post-install fixup: env/bin/fix-env <build-prefix> (once)" \
        "host deps (NOT bundled): tesseract5 +deu, ghostscript on PATH"
    mkdir -p "$ROOT/fixer" "$ROOT/bin"
    rm -rf "$target"
    mkdir -p "$target"
    tar --zstd -xf "$art" -C "$target" --strip-components 1
    # one-time venv relocation fixup, build prefix recorded by the build script
    bp=$(cat "$target/env/.build-prefix")
    "$target/env/bin/fix-env" "$bp"
    "$target/env/bin/python" -c 'import pymupdf'
    cat >"$ROOT/bin/axiom-fixer" <<EOF
#!/bin/sh
exec "$ROOT/fixer/current/env/bin/python" "$ROOT/fixer/current/app/repair_agent.py" "\$@"
EOF
    chmod +x "$ROOT/bin/axiom-fixer"
    ln -sfn "$version" "$ROOT/fixer/current"
    echo "installed: $ROOT/bin/axiom-fixer ($version)"
    echo "rollback:  ln -sfn <prev-version> $ROOT/fixer/current (fixer is event-driven: scripts/fix.sh picks up current on next invocation)"
    ;;
*)
    echo "unknown component '$component' (rag|runner|fixer)"
    exit 1
    ;;
esac
