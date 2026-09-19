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
# PATH is NOT required (GPU-carrier scenario). deu+eng both ship with the
# conda tesseract package (no separate tessdata download since 2a0823f).
# The OCR staged check below answers from the packed env (tesseract
# --list-langs must show deu+eng) -- the import_audit guard pattern
# applied to bins.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
# macOS-only build: the explicit conda lock (osx-arm64 URLs) and the
# stat -f%z size witness below are Darwin-shaped; refuse anything else.
[ "$(uname -s)" = "Darwin" ] || { echo "fixer-artifact: macos-arm64 build only (lock + stat are Darwin-specific); uname says $(uname -s)" >&2; exit 1; }
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
# #286 pinning: fixtures/storage and fixtures/difficult are GITIGNORED,
# GENERATED test data (sandbox storage + difficult books, ~0.5 GB after a
# test round on the build host). Shipping them made the artifact size
# depend on build-host residue — the 3× swing (720 MB vs 220 MB). Neither
# is needed at runtime: the artifact ships the small COMMITTED fixtures
# only (OCR smoke evidence + operator reproduction). The staged gate
# (tests/test_import_guard.py) does not touch them.
rsync -a \
    --exclude '.venv' --exclude '__pycache__' --exclude 'runs' \
    --exclude 'fixtures/storage' --exclude 'fixtures/difficult' \
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
# #286 pinning: the EXPLICIT LOCK is the ONLY solve path — the exact
# package URLs of the rc3-proven solve, so conda-forge drift cannot swing
# the env size or ship an unverified binary combination. A missing lock
# is FATAL: determinism is this build's whole point, there is no silent
# fresh-solve fallback.
FIXER_LOCK="$ROOT/scripts/lib/fixer-conda-osx-arm64.lock"
if [ ! -x "$PREFIX/bin/python" ]; then
    [ -f "$FIXER_LOCK" ] || {
        echo "fixer-artifact: explicit lock missing: $FIXER_LOCK" >&2
        echo "  regeneration recipe: rm -rf dist/build/fixer/env &&" >&2
        echo "  micromamba create -y -p dist/build/fixer/env -c conda-forge 'python=3.11' 'tesseract=5.*' 'ghostscript' pip" >&2
        echo "  (run the staged gates on it) && micromamba list --explicit -p dist/build/fixer/env" >&2
        echo "  | grep '^https://' > scripts/lib/fixer-conda-osx-arm64.lock" >&2
        echo "  — then re-add the header + @EXPLICIT marker line (see the lock's own header)" >&2
        exit 1
    }
    echo "fixer-artifact: conda install from explicit lock ($FIXER_LOCK)"
    "$MM" create -y -p "$PREFIX" -f "$FIXER_LOCK"
fi
# #286 review: a CACHED env bypasses the lock above entirely — verify the
# prefix against the lock on EVERY build so artifact determinism never
# hangs on build-host cache state (the env-reuse hole). URL sets must
# match exactly; a drift is a hard error naming the reset.
"$MM" list --explicit -p "$PREFIX" 2>/dev/null | grep '^https://' | sort > "$BUILD/env-actual.txt"
grep '^https://' "$FIXER_LOCK" | sort > "$BUILD/env-lock.txt"
if ! cmp -s "$BUILD/env-actual.txt" "$BUILD/env-lock.txt"; then
    echo "fixer-artifact: cached env does not match the explicit lock — reset required:" >&2
    echo "  rm -rf dist/build/fixer/env && rebuild (lock-diff: $(diff "$BUILD/env-lock.txt" "$BUILD/env-actual.txt" | head -4 | tr '\n' ' '))" >&2
    exit 1
fi
LOCK_N=$(wc -l < "$BUILD/env-lock.txt" | tr -d ' ')
rm -f "$BUILD/env-actual.txt" "$BUILD/env-lock.txt"
echo "fixer-artifact: env matches the explicit lock ($LOCK_N packages)"
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
# intact). The allowlist is scripts/lib/ocr_languages.txt — ONE source
# shared with tesseractLang() in the invoker (pinned by a Go test reading
# the same file); osd (orientation detection) is always kept on top.
# Guarded: with tesseract entirely absent (the mutation probe), the prune
# must not kill the build here — the staged assert below names the cause.
STAGE_TESSDATA="$STAGE/env/share/tessdata"
if [ -d "$STAGE_TESSDATA" ]; then
    OCR_LANGS="$(grep -v '^#' "$ROOT/scripts/lib/ocr_languages.txt" | tr '\n' ' ' | tr -s ' ')"
    find "$STAGE_TESSDATA" -name '*.traineddata' | while read -r m; do
        base="$(basename "$m" .traineddata)"
        case " $OCR_LANGS osd " in
            *" $base "*) ;;          # keep: mapped language or osd
            *) rm -f "$m" ;;
        esac
    done
    echo "fixer-artifact: tessdata pruned to:$OCR_LANGS osd ($(find "$STAGE_TESSDATA" -name '*.traineddata' | wc -l | tr -d ' ') models left)"
fi

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
(cd "$STAGE/app" && "$STAGE/env/bin/python" -m pytest tests/test_import_guard.py --noconftest -q)

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
artifact_strip_pycache "$STAGE"

# The staged pytest gate runs with --noconftest: the conftest's autouse
# fixture would REGENERATE fixtures/storage in the staged tree (sandbox
# storage for the full suite — irrelevant to the import guard, which sets
# up its own sys.path). With conftest off, NOTHING legitimate creates the
# generated dirs during staging — the leak check below is therefore REAL
# fail-closed: any appearance (rsync residue, conftest, a future staged
# test) fails the build instead of silently shipping host residue (the
# original 3× size swing).
for _leak in fixtures/storage fixtures/difficult; do
    if [ -e "$STAGE/app/$_leak" ]; then
        echo "fixer-artifact: staging leak — $STAGE/app/$_leak exists (generated test data must not ship)" >&2
        exit 1
    fi
done

tar --zstd -C "$BUILD" -cf "$ARTIFACT" "fixer-$VERSION"
# #286 pinning size witness: the artifact must stay DETERMINISTICALLY
# slim — the lock bounds the env, the rsync excludes bound the app side.
# A stray directory (or an unpinned solve explosion) fails the build
# instead of silently shipping 3× the transfer size — BEFORE the sha256
# sidecar is written, and removing the artifact on failure so dist/ never
# carries an over-ceiling pair.
# ponytail: fixed 450 MB ceiling — revisit only when a DELIBERATE bundling
# decision (new toolchain in the env) outgrows it.
ART_SIZE=$(stat -f%z "$ARTIFACT")
if [ "$ART_SIZE" -gt 471859200 ]; then
    echo "fixer-artifact: $((ART_SIZE / 1048576)) MB exceeds the 450 MB ceiling — check for stray staging residue or solve drift" >&2
    rm -f "$ARTIFACT"
    exit 1
fi
echo "fixer-artifact: size ok — $((ART_SIZE / 1048576)) MB (ceiling 450 MB)"
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
