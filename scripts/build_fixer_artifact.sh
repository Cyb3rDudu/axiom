#!/bin/sh
# build_fixer_artifact.sh — G2 of #205.
# Builds the autarkic pdf_repair_agent artifact:
#   dist/axiom-fixer-<version>-macos-arm64.tar.zst
# Layout: fixer-<version>/{env/,app/}
#   env/  conda-forge python 3.11 + pinned reqs — BUNDLED INTERPRETER via
#         conda-pack (same approach as build_runner_artifact.sh). Fixes the
#         "not autarkic" finding from the v0.1.11 install (issue #208):
#         the previous venv layout symlinked to the build-host python
#         (/Library/Frameworks/...) and broke on every other host.
#   app/  package sources.
# Install-time fixup: run env/bin/conda-unpack ONCE after extracting
# (install_dist.sh does this automatically). The old venv fix-env/
# .build-prefix mechanism is gone with the venv.
# Isolation is proven AGAINST the ARTIFACT: the import_audit guard runs
# with the staged env before tarring.
#
# #286 (bundled-binaries standard #224/#211, born from the pandoc/zstd
# incidents): tesseract5, ghostscript and tessdata (deu+eng) ship IN the
# env -- everything the pipeline shells out to travels with the artifact.
# The OCR tools resolve them env-relatively (tools/ocr_tool.py:
# bundled_bin/ocr_child_env -- relocatable through conda-unpack); a host
# PATH is NOT required (GPU-carrier scenario). eng ships with the conda
# package; deu comes from the pinned tessdata release below. The OCR
# staged check below answers from the packed env (tesseract --list-langs
# must show deu+eng) -- the import_audit guard pattern applied to bins.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
BUILD="$DIST/build/fixer"
# shared plumbing (micromamba bootstrap, conda-pack staging, drift guard):
. "$ROOT/scripts/lib/artifact_common.sh"
VERSION="${1:?usage: build_fixer_artifact.sh <version>}"
ARTIFACT="$DIST/axiom-fixer-$VERSION-macos-arm64.tar.zst"
STAGE="$BUILD/fixer-$VERSION"
PREFIX="$BUILD/env"

cd "$ROOT"
rm -rf "$STAGE"
mkdir -p "$STAGE"

# --- app/: sources without runtime residue -----------------------------------
rsync -a \
    --exclude '.venv' --exclude '__pycache__' --exclude 'runs' \
    axiom_ng/tools/pdf_repair_agent/ "$STAGE/app/"

# fix.sh ships INTO the artifact (#206): the installed /opt/axiom/bin/axiom-fixer
# shim execs it, so EVERY caller (invoker, operator) runs through the same
# per-key lockdir + 30-min timeout — one source of truth, no shim duplicate.
# fix.sh's defaults (AXIOM_FIXER=/opt/axiom/fixer/current/…) match the installed
# layout via the `current` symlink.
install -m 0755 scripts/fix.sh "$STAGE/fix.sh"

artifact_mm_bootstrap
artifact_assert_bundled_env_identical

# --- conda env with python 3.11 (lockfile was frozen on 3.11) ----------------
# ENV REUSE (unified policy, runner semantics): create only when missing.
# Changing a conda spec line requires ONE manual
#   rm -rf dist/build/fixer/env
# so cache-hardlinked package files are never silently overwritten.
# #286: tesseract + ghostscript aus conda-forge INS Env (bundled-binaries
# Standard; deu/eng-tessdata bringt das conda-Paket mit).
if [ ! -x "$PREFIX/bin/python" ]; then
    "$MM" create -y -p "$PREFIX" -c conda-forge 'python=3.11' 'tesseract=5.*' 'ghostscript' pip
fi
PY="$PREFIX/bin/python"

# --- pinned deps (lock wins when present — same rule as bootstrap.sh) -------
REQS="axiom_ng/tools/pdf_repair_agent/requirements.txt"
[ -f axiom_ng/tools/pdf_repair_agent/requirements.lock.txt ] && \
    REQS="axiom_ng/tools/pdf_repair_agent/requirements.lock.txt"
"$PY" -m pip install -q --disable-pip-version-check -r "$REQS"

# #286 review: NO separate deu download. The conda tesseract package ships
# deu+eng itself (plus ~120 other languages); overwriting its
# deu.traineddata in $PREFIX previously wrote THROUGH a conda cache
# hardlink and corrupted the shared package cache (Invalid package cache
# … incorrect size), and a raw/main URL pin was a moving target. Model
# provenance now rides the conda solve (tesseract=5.* package pin) — the
# staged --list-langs gate below still fails closed when deu/eng go
# missing.

"$PY" -m pip install -q --disable-pip-version-check conda-pack

# --- pack env (relocatable; conda-unpack fixes prefixes at install) ---------
artifact_pack_env "$PREFIX" "$STAGE"

# Language PRUNE (size lever, #286 review: 353 MB tessdata) — AFTER
# packing: conda-pack verifies package completeness in $PREFIX, so the
# prune runs on the STAGED copy only (packed prefix + package cache stay
# intact). The artifact keeps what the pipeline speaks (deu+eng) plus osd
# (orientation detection).
STAGE_TESSDATA="$STAGE/env/share/tessdata"
find "$STAGE_TESSDATA" -name '*.traineddata' \
    ! -name 'eng.traineddata' ! -name 'deu.traineddata' ! -name 'osd.traineddata' \
    -delete
echo "fixer-artifact: tessdata pruned to deu+eng+osd ($(ls "$STAGE_TESSDATA"/*.traineddata | wc -l | tr -d ' ') models left)"

# --- interpreter autarky proof (#208): NO symlink may leave the artifact ----
if find "$STAGE/env/bin" -name 'python*' -type l | while read -r l; do
    tgt=$(readlink "$l")
    case "$tgt" in
        /*) case "$tgt" in "$STAGE"/*) ;; *) echo "non-bundled interpreter link: $l -> $tgt"; exit 1 ;; esac ;;
        *) : ;; # relative link inside env/ (python -> python3.11) is fine
    esac
done; then :; else
    echo "fixer-artifact: interpreter is not bundled — refusing to ship" >&2
    exit 1
fi
[ -x "$STAGE/env/bin/python3.11" ] && ! [ -L "$STAGE/env/bin/python3.11" ] || {
    echo "fixer-artifact: env/bin/python3.11 must be a real binary (conda-pack), not a symlink" >&2
    exit 1
}

# --- isolation proof against the ARTIFACT --------------------------------------
(
    cd "$STAGE/app" && "$STAGE/env/bin/python" - <<'EOF'
import sys
from pathlib import Path
sys.path.insert(0, str(Path(".").resolve()))
from tools import import_audit
r = import_audit.audit()
assert r["clean"], f"isolation violated in artifact: {r['violations']}"
print("fixer-artifact: import_audit clean against artifact env")
EOF
)
# Smoke from a NEUTRAL cwd so we test the env, not the source tree (#209
# lesson: a smoke run from a source dir masks missing installs).
(
    cd / && "$STAGE/env/bin/python" -c 'import pymupdf; print("fixer-artifact: staged pymupdf", pymupdf.__version__)'
)
(cd "$STAGE/app" && "$STAGE/env/bin/python" -m pytest tests/test_import_guard.py -q)

# --- #286: OCR staged check against the PACKED env (import_audit pattern) ----
# Mutation probe: remove tesseract/ghostscript from the conda create line
# -> THESE asserts fail the build. No host PATH: TESSDATA_PREFIX points
# at the env, PATH is minimal.
[ -x "$STAGE/env/bin/tesseract" ] || { echo "fixer-artifact: env/bin/tesseract missing" >&2; exit 1; }
[ -x "$STAGE/env/bin/gs" ] || { echo "fixer-artifact: env/bin/gs missing" >&2; exit 1; }
[ -f "$STAGE_TESSDATA/deu.traineddata" ] || { echo "fixer-artifact: tessdata deu missing" >&2; exit 1; }
[ -f "$STAGE_TESSDATA/eng.traineddata" ] || { echo "fixer-artifact: tessdata eng missing (conda package changed?)" >&2; exit 1; }
STAGE_LANGS=$(TESSDATA_PREFIX="$STAGE_TESSDATA" "$STAGE/env/bin/tesseract" --list-langs 2>/dev/null || true)
echo "$STAGE_LANGS" | grep -qx 'deu' || { echo "fixer-artifact: staged tesseract lacks deu: $STAGE_LANGS" >&2; exit 1; }
echo "$STAGE_LANGS" | grep -qx 'eng' || { echo "fixer-artifact: staged tesseract lacks eng: $STAGE_LANGS" >&2; exit 1; }
TESSDATA_PREFIX="$STAGE_TESSDATA" "$STAGE/env/bin/tesseract" --version >/dev/null
TESSDATA_PREFIX="$STAGE_TESSDATA" "$STAGE/env/bin/gs" --version >/dev/null
echo "fixer-artifact: staged OCR ok — tesseract+gs+deu/eng from the packed env"

# #286 DoD: sanitized-PATH rebuild smoke — the carrier scenario. Kein Host-
# tesseract/gs im PATH; der Rebuild muss ALLEIN aus dem Env laufen.
(
    cd "$STAGE/app"
    PATH="/usr/bin:/bin" TESSDATA_PREFIX="$STAGE_TESSDATA" \
        "$STAGE/env/bin/python" - <<'SMOKE'
import sys, tempfile
from pathlib import Path
sys.path.insert(0, str(Path(".").resolve()))
from tools import scan_ocr_rebuild
src = Path("fixtures/scan_mit_folios.pdf")
out = Path(tempfile.mkdtemp()) / "rebuilt.pdf"
res = scan_ocr_rebuild.run_rebuild(src, out, lang="deu", timeout_s=600)
assert res.get("applied"), f"sanitized-PATH rebuild failed: {res.get('cause')}"
print("fixer-artifact: sanitized-PATH OCR rebuild ok "
      f"({res['quality']['total_chars']} chars, {res['pages']} pages)")
SMOKE
) || { echo "fixer-artifact: sanitized-PATH rebuild smoke FAILED" >&2; exit 1; }

# --- artifact --------------------------------------------------------------------
# unified staging policy (runner semantics): tests ship NOT in the
# artifact — the pytest gate above ran against the staged tree, now the
# tests are stripped before tarring (fixtures stay: the OCR smoke's
# evidence + operator reproduction material).
rm -rf "$STAGE/app/tests"
tar --zstd -C "$BUILD" -cf "$ARTIFACT" "fixer-$VERSION"
(cd "$DIST" && shasum -a 256 "${ARTIFACT##*/}" >"${ARTIFACT##*/}.sha256")
echo "fixer-artifact: $ARTIFACT"
# DoD witness (#286): the listing contains the bundled OCR pieces.
for member in env/bin/tesseract env/bin/gs env/share/tessdata/deu.traineddata env/share/tessdata/eng.traineddata; do
    tar --zstd -tf "$ARTIFACT" "fixer-$VERSION/$member" >/dev/null || {
        echo "fixer-artifact: artifact listing lacks $member" >&2
        exit 1
    }
done
echo "fixer-artifact: bundled OCR verified in the listing (tesseract, gs, tessdata deu+eng)"
echo "install: extract to /opt/axiom/fixer/$VERSION — install_dist.sh runs env/bin/conda-unpack automatically"
