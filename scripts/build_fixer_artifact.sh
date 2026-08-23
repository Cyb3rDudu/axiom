#!/bin/sh
# build_fixer_artifact.sh — G2 of #205.
# Builds the autarkic pdf_repair_agent artifact:
#   dist/axiom-fixer-<version>-macos-arm64.tar.zst
# Layout: fixer-<version>/{env/,app/} — env/ is the package's OWN venv
# (built from its bootstrap.sh, pinned reqs), app/ is the package source.
# Isolation is proven AGAINST THE ARTIFACT: the import_audit guard runs
# with the staged env before tarring.
#
# Host dependencies NOT bundled (documented in docs/operations/services.md):
#   tesseract5 (+deu) and ghostscript must be on PATH for the OCR lane.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
BUILD="$DIST/build/fixer"
VERSION="${1:?usage: build_fixer_artifact.sh <version>}"
ARTIFACT="$DIST/axiom-fixer-$VERSION-macos-arm64.tar.zst"
STAGE="$BUILD/fixer-$VERSION"

cd "$ROOT"
rm -rf "$STAGE"
mkdir -p "$STAGE"

# --- app/: sources without runtime residue -----------------------------------
rsync -a \
    --exclude '.venv' --exclude '__pycache__' --exclude 'runs' \
    axiom_ng/tools/pdf_repair_agent/ "$STAGE/app/"

# --- env/: build the package's own venv inside the STAGE (not the repo tree) --
/bin/bash "$STAGE/app/bootstrap.sh"
mv "$STAGE/app/.venv" "$STAGE/env"

# --- venv relocation fixup (one-time at install): shebangs ------------------
# The venv is BUILT at $STAGE/app/.venv (pip writes that prefix into every
# shebang) and then moved to $STAGE/env; .build-prefix records the BUILD-time
# path so fix-env can rewrite it at the install location.
cat >"$STAGE/env/bin/fix-env" <<'EOF'
#!/bin/sh
# One-time relocation fixup after extracting the fixer artifact (#204/#205).
# Rewrites venv shebangs to the current prefix.
set -eu
export LC_ALL=C # byte-wise sed: venv shebang rewrites must not trip on binary matches
HERE="$(cd "$(dirname "$0")/.." && pwd)"
OLD="${1:?usage: fix-env <build-time prefix>}"
[ "$OLD" != "$HERE" ] || exit 0
grep -rl "$OLD" "$HERE/bin" 2>/dev/null | while read -r f; do
    sed -i '' "s|$OLD|$HERE|g" "$f"
done
echo "fix-env: relocated $OLD -> $HERE"
EOF
chmod +x "$STAGE/env/bin/fix-env"
printf '%s\n' "$STAGE/app/.venv" >"$STAGE/env/.build-prefix"
BUILD_PREFIX="$STAGE/env"

# --- isolation proof against the ARTIFACT --------------------------------------
(
    cd "$STAGE/app" && "$BUILD_PREFIX/bin/python" - <<'EOF'
import sys
from pathlib import Path
sys.path.insert(0, str(Path(".").resolve()))
from tools import import_audit
r = import_audit.audit()
assert r["clean"], f"isolation violated in artifact: {r['violations']}"
print("fixer-artifact: import_audit clean against artifact env")
EOF
)
(cd "$STAGE/app" && "$BUILD_PREFIX/bin/python" -m pytest tests/test_import_guard.py -q)

# --- artifact --------------------------------------------------------------------
tar --zstd -C "$BUILD" -cf "$ARTIFACT" "fixer-$VERSION"
shasum -a 256 "$ARTIFACT" >"$ARTIFACT.sha256"
echo "fixer-artifact: $ARTIFACT"
echo "install: extract to /opt/axiom/fixer/$VERSION — install_dist.sh runs env/bin/fix-env automatically (build prefix in env/.build-prefix)"
