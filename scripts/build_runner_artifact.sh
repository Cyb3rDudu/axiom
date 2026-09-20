#!/bin/sh
# build_runner_artifact.sh — G2 of #205 (supersedes #204 packaging goals).
# Builds a relocatable macOS arm64 runner artifact with micromamba+conda-pack:
#   dist/axiom-runner-<version>-macos-arm64.tar.zst
# Layout inside the tar: runner-<version>/{env/,app/}
#   env/  conda-forge python 3.11 + requirements(-heavy) + installed runner
#   app/  runner sources (reference; the executable is env/bin/axiom-runner)
# Install-time fixup: run env/bin/conda-unpack ONCE after extracting (#204).
# No .venv trees are created in the Git workspace — everything stages under
# dist/ (repo-ignored).
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
BUILD="$DIST/build/runner"
# shared plumbing (micromamba bootstrap, conda-pack staging, drift guard):
. "$ROOT/scripts/lib/artifact_common.sh"
VERSION="${1:?usage: build_runner_artifact.sh <version>}"
ARTIFACT="$DIST/axiom-runner-$VERSION-macos-arm64.tar.zst"
PREFIX="$BUILD/env"

cd "$ROOT"

artifact_mm_bootstrap
artifact_assert_bundled_env_identical

# --- conda env with python 3.11 + pandoc (#224) ----------------------------
# ENV REUSE (unified policy): create only when missing. Changing a conda
# spec line requires ONE manual `rm -rf dist/build/runner/env`.
# pandoc ships IN the artifact: epub_worker shells out to it for EPUB→Markdown
# and a host-provided pandoc is a hidden dependency (E2E finding: production
# runner died with "pandoc binary not found" once the carrier — which had it —
# went to sleep; mirrors the #211 zstd lesson). conda-forge's pandoc is
# self-contained next to the bundled interpreter; #286: the worker resolves
# it env-relatively (bundled_env.bundled_bin) — service PATHs never include
# env/bin, PATH is only the dev fallback.
if [ ! -x "$PREFIX/bin/python" ]; then
    "$MM" create -y -p "$PREFIX" -c conda-forge 'python=3.11' 'pandoc' pip
fi
PY="$PREFIX/bin/python"

# --- deps + runner package (lockfile preference: unified policy — a lock
# wins when one exists; the runner has none today, the fixer does) ---------
RUNNER_REQS="axiom_ng_runner/requirements.txt"
[ -f axiom_ng_runner/requirements.lock.txt ] && \
    RUNNER_REQS="axiom_ng_runner/requirements.lock.txt"
"$PY" -m pip install -q --disable-pip-version-check \
    -r "$RUNNER_REQS" -r axiom_ng_runner/requirements-heavy.txt
"$PY" -m pip install -q --disable-pip-version-check --no-deps ./axiom_ng_runner
"$PY" -m pip install -q --disable-pip-version-check conda-pack

# --- app/ sources ------------------------------------------------------------
STAGE="$BUILD/runner-$VERSION"
rm -rf "$STAGE"
mkdir -p "$STAGE"
rsync -a --delete \
    --exclude '.venv' --exclude '__pycache__' --exclude 'tests' \
    --exclude 'scripts' --exclude 'shell.nix' \
    --exclude 'build' --exclude '*.egg-info' \
    --exclude '.ruff_cache' --exclude '.pytest_cache' --exclude '.mypy_cache' \
    axiom_ng_runner/ "$STAGE/app/"

# --- pack env (relocatable; conda-unpack fixes prefixes at install) ---------
artifact_pack_env "$PREFIX" "$STAGE"

# --- verify entry point from the STAGED env, from a NEUTRAL cwd (#209: a
# source-dir cwd masks a missing install — `import axiom_ng_runner` would hit
# ../axiom_ng_runner on sys.path even when the env shipped 0 module files). ---
(
    cd / && "$STAGE/env/bin/python" -c 'import axiom_ng_runner, torch; print("staged import ok (neutral cwd), torch", torch.__version__, "mps", torch.backends.mps.is_available())'
# marker font (production finding 2026-09-06): marker downloads its GoNoto
# font into site-packages/static/fonts on FIRST use at runtime — a
# read-only nix store makes that a PermissionError that kills every PDF
# conversion. Bake the font into the artifact (#224 pattern): download
# once at build time, verify presence, and marker's download_font()
# short-circuits on the existing file.
FONT_URL="https://models.datalab.to/artifacts/GoNotoCurrent-Regular.ttf"
FONT_DST="$STAGE/env/lib/python3.11/site-packages/static/fonts/GoNotoCurrent-Regular.ttf"
mkdir -p "$(dirname "$FONT_DST")"
curl -fsSL "$FONT_URL" -o "$FONT_DST" || {
    echo "FATAL: marker font download failed — artifact would crash on first PDF (read-only store)" >&2
    exit 1
}
test -s "$FONT_DST" && echo "staged marker font ok ($(wc -c < "$FONT_DST") bytes)"
# #224/#286: the EPUB path needs a bundled pandoc — staged check from the
# PACKED env, sanitized PATH (carrier scenario): the runtime resolution
# (bundled_env) must find it env-relatively AND a real EPUB→GFM conversion
# must succeed with no host contribution.
(
    cd "$STAGE/app" && PATH="/usr/bin:/bin" "$STAGE/env/bin/python" - <<'PDOC'
import subprocess, sys, tempfile, zipfile
from pathlib import Path
sys.path.insert(0, str(Path(".").resolve()))
from axiom_ng_runner.compute_core import bundled_env

pandoc = bundled_env.bundled_bin("pandoc")
assert pandoc and pandoc.startswith(sys.prefix), f"pandoc not env-relative: {pandoc!r}"

tmp = Path(tempfile.mkdtemp())
epub = tmp / "probe.epub"
xhtml = "<html><body><h1>Kapitel</h1><p>Pandoc Carrier Smoke.</p></body></html>"
with zipfile.ZipFile(epub, "w") as z:
    z.writestr("mimetype", "application/epub+zip")
    z.writestr("OEBPS/content.xhtml", xhtml)
    z.writestr("META-INF/container.xml",
               '<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">'
               '<rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>')
    z.writestr("OEBPS/content.opf",
               '<?xml version="1.0"?>'
               '<package xmlns="http://www.idpf.org/2007/opf" xmlns:dc="http://purl.org/dc/elements/1.1/" version="2.0">'
               '<metadata>'
               '<dc:title>Carrier Smoke</dc:title>'
               '<dc:identifier id="id">urn:uuid:0286smoke</dc:identifier>'
               '<dc:language>en</dc:language>'
               '</metadata>'
               '<manifest><item id="c" href="content.xhtml" media-type="application/xhtml+xml"/></manifest>'
               '<spine><itemref idref="c"/></spine></package>')
out = tmp / "probe.md"
proc = subprocess.run(
    [pandoc, "--from", "epub", "--to", "gfm", "--wrap", "none",
     "-o", str(out), str(epub)],
    capture_output=True, text=True, env=bundled_env.child_env(with_tessdata=False),
)
assert proc.returncode == 0, proc.stderr
assert "Pandoc Carrier Smoke" in out.read_text(), out.read_text()
print("staged pandoc ok — env-relative resolution + EPUB conversion, sanitized PATH")
PDOC
) || { echo "runner-artifact: staged pandoc check FAILED" >&2; exit 1; }
)

# --- artifact -----------------------------------------------------------------
artifact_strip_pycache "$STAGE"

tar --zstd -C "$BUILD" -cf "$ARTIFACT" "runner-$VERSION"
(cd "$DIST" && shasum -a 256 "${ARTIFACT##*/}" >"${ARTIFACT##*/}.sha256")
echo "runner-artifact: $ARTIFACT"
echo "install: extract to /opt/axiom/runner/$VERSION, then run env/bin/conda-unpack once"
