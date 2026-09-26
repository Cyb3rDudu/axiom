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

# zstd preflight (#211): the runner/fixer tarballs are zstd-compressed and
# unpacked with `tar --zstd`. Check BEFORE the confirm prompt so an operator
# doesn't confirm an install and then hit a cryptic filter error mid-unpack.
require_zstd() {
    command -v zstd >/dev/null 2>&1 || {
        echo "zstd not found on PATH — required to unpack a .tar.zst artifact." >&2
        echo "install: brew install zstd   (or: apt-get install zstd)" >&2
        exit 1
    }
}

# confirm_install <component> <artifact> [plan lines...]
# Verifies the checksum FIRST, prints the install plan, then requires an
# explicit operator "yes" before anything under $ROOT is touched.
confirm_install() {
    comp="$1"
    art="$2"
    shift 2
    # basename-only sidecars: verify from the artifact's own directory so
    # the check works regardless of the caller's cwd (#205 G3 review).
    (cd "$(dirname "$art")" && shasum -a 256 -c "$(basename "$art").sha256") 2>/dev/null || {
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
    # F05 (#299): the canonical binary ships alongside the alias — same
    # build generation, both installed under /opt/axiom/bin.
    cbin=$(find_artifact "axiom-$version-*") || {
        echo "no axiom artifact for $version in $DIST/ — run: make rag"
        exit 1
    }
    target="$ROOT/rag/$version"
    confirm_install rag "$bin" \
        "target:   $target/axiom-ng" \
        "target:   $target/axiom (canonical, F05)" \
        "current:  $ROOT/rag/current -> $version" \
        "shim:     $ROOT/bin/axiom-ng" \
        "shim:     $ROOT/bin/axiom"
    mkdir -p "$target" "$ROOT/bin"
    cp "$bin" "$target/axiom-ng"
    cp "$cbin" "$target/axiom"
    chmod 0755 "$target/axiom-ng" "$target/axiom"
    ln -sfn "$version" "$ROOT/rag/current"
    ln -sfn "$ROOT/rag/current/axiom-ng" "$ROOT/bin/axiom-ng"
    ln -sfn "$ROOT/rag/current/axiom" "$ROOT/bin/axiom"
    echo "installed: $ROOT/bin/axiom + $ROOT/bin/axiom-ng ($version)"
    ;;
runner)
    art=$(find_artifact "axiom-runner-$version-*.tar.zst") || {
        echo "no runner artifact for $version in $DIST/ — run: make runner"
        exit 1
    }
    require_zstd
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
    # smoke from a NEUTRAL cwd (tests the env, not a source tree — #209 lesson)
    (cd / && "$target/env/bin/python" -c 'import axiom_ng_runner, torch')
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
    require_zstd
    target="$ROOT/fixer/$version"
    confirm_install fixer "$art" \
        "target:   $target/{env,app}" \
        "current:  $ROOT/fixer/current -> $version" \
        "shim:     $ROOT/bin/axiom-repair-worker (F08 #302 canonical name)" \
        "alias:    $ROOT/bin/axiom-fixer (compat wrapper, ADR 0001 §4)" \
        "post-install fixup: env/bin/conda-unpack (once, bundled interpreter)" \
        "OCR toolchain bundled (#286): tesseract+gs+tessdata(deu+eng) in env/ — no host PATH"
    mkdir -p "$ROOT/fixer" "$ROOT/bin"
    rm -rf "$target"
    mkdir -p "$target"
    tar --zstd -xf "$art" -C "$target" --strip-components 1
    # one-time prefix relocation: the env ships conda-packed with a bundled
    # interpreter (#208) — conda-unpack rewrites prefixes in place. No host
    # python required anymore.
    "$target/env/bin/python" "$target/env/bin/conda-unpack"
    # smoke from a NEUTRAL cwd (tests the env, not a source tree — #209 lesson)
    (cd / && "$target/env/bin/python" -c 'import pymupdf; print("pymupdf", __import__("pymupdf").__version__)')
    # F08 (#302, ADR 0001 §4): the canonical worker name is
    # axiom-repair-worker; the legacy axiom-fixer stays as a compat
    # wrapper (warns once, delegates). Both are locking wrappers, not
    # bare execs (#206): the invoker and manual operator runs must
    # serialize per key — the shipped fix.sh carries the per-key lockdir
    # + 30-min timeout; a bare python exec would bypass both.
    cat >"$ROOT/bin/axiom-repair-worker" <<EOF
#!/bin/sh
exec "$ROOT/fixer/current/fix.sh" "\$@"
EOF
    chmod +x "$ROOT/bin/axiom-repair-worker"
    cat >"$ROOT/bin/axiom-fixer" <<EOF
#!/bin/sh
# Compat alias (ADR 0001 §4, functional through 0.2.x): delegates to the
# canonical axiom-repair-worker. Warns exactly once per invocation.
echo "axiom: axiom-fixer is deprecated — use axiom-repair-worker (ADR 0001: docs/adr/0001-canonical-naming.md)" >&2
exec "$ROOT/bin/axiom-repair-worker" "\$@"
EOF
    chmod +x "$ROOT/bin/axiom-fixer"
    ln -sfn "$version" "$ROOT/fixer/current"
    echo "installed: $ROOT/bin/axiom-repair-worker ($version)"
    echo "alias:     $ROOT/bin/axiom-fixer warns + delegates (removal earliest in an announced major)"
    echo "rollback:  ln -sfn <prev-version> $ROOT/fixer/current (repair worker is event-driven: fix.sh picks up current on next invocation)"
    ;;
*)
    echo "unknown component '$component' (rag|runner|fixer)"
    exit 1
    ;;
esac
