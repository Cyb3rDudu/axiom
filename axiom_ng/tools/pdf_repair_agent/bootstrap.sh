#!/usr/bin/env bash
# bootstrap.sh — baut das EIGENE Venv des pdf_repair_agent idempotent.
#
# Portabilität: läuft auf jedem System mit python3 + Netz. Stützt sich auf
# ein Vernunft-Interpreter (System python3, dann nix-store, dann bekannte
# Binaries). Das Venv ist STRENG paketlokal — kein Kontakt zu Projekt-Venvs.
#
# Verhalten:
#   · findet python3 >= 3.9 (bevorzugt >= 3.11)
#   · baut/repariert `<paket>/.venv` idempotent
#   · installiert die gepinnten Reqs genau einmal (Marker-Datei mit Reqs-Hash)
#   · prüft Versionen + meldet den OCR-Pfad (tesseract+gs), bricht aber NICHT
#     hart ab — nur T2 (OCR) braucht die Binaries.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENV="$HERE/.venv"
REQS="$HERE/requirements.txt"
MARKER="$VENV/.pip-installed-$(cksum <"$REQS" | awk '{print $1}')"

# --- 1) Vernunft-Interpreter finden ---------------------------------------
# Neuere pymupdf-Pins verlangen Python 3.10+; bevorzugt wird ≥3.11.
PY=""
for cand in \
    "$HERE/.venv/bin/python" \
    "$(command -v python3 || true)" \
    /usr/local/bin/python3 \
    /opt/homebrew/bin/python3 \
    /nix/store/*python3-3.1*/bin/python3.11 \
    /nix/store/*python3-3.1*/bin/python3.10; do
    [ -n "$cand" ] && [ -x "$cand" ] || continue
    v="$($cand -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")' 2>/dev/null || true)"
    [ -n "$v" ] || continue
    major="${v%.*}"
    minor="${v##*.}"
    if [ "$major" -ge 3 ] && [ "$minor" -ge 10 ]; then
        PY="$cand"
        echo "bootstrap: Interpreter $PY (Python $v)"
        break
    fi
    PY39="${PY39:-$cand}" # letzte Wahl: 3.9 (nur mit älteren Pins)
done
if [ -z "$PY" ]; then
    echo "bootstrap: KEIN python3 >=3.10 gefunden (sah 3.9: ${PY39:-n/a})." >&2
    echo "bootstrap: Neuere pymupdf-Pins verlangen 3.10+. Abbruch." >&2
    exit 1
fi

# --- 2) Venv idempotent sicherstellen -------------------------------------
if [ ! -x "$VENV/bin/python" ]; then
    echo "bootstrap: baue Venv unter $VENV"
    rm -rf "$VENV"
    "$PY" -m venv "$VENV"
fi

# --- 3) Pin-Installation nur bei geänderter Reqs ---------------------------
if [ ! -f "$MARKER" ]; then
    echo "bootstrap: installiere gepinnte Abhängigkeiten…"
    "$VENV/bin/python" -m pip install --disable-pip-version-check -q \
        --upgrade pip
    "$VENV/bin/python" -m pip install --disable-pip-version-check -q \
        -r "$REQS"
    touch "$MARKER"
else
    echo "bootstrap: Abhängigkeiten bereits installiert (Marker ok)"
fi

# --- 4) Versionsprüfung -----------------------------------------------------
VP="$($VENV/bin/python -c 'import pymupdf; print(pymupdf.__version__)' 2>/dev/null || echo '??')"
echo "bootstrap: pymupdf $VP"

# --- 5) OCR-Binary-Hinweis (kein harter Bruch) ------------------------------
if command -v tesseract >/dev/null 2>&1 && command -v gs >/dev/null 2>&1; then
    echo "bootstrap: tesseract + ghostscript OK (OCR-Pfad T2 verfügbar)"
else
    echo "bootstrap: HINWEIS — tesseract und/oder ghostscript nicht gefunden." >&2
    echo "bootstrap: Der T2-OCR-Pfad kann die Textschicht nicht bauen (wird als" >&2
    echo "bootstrap: Unbelegbarkeit gemeldet). Alle übrigen Werkzeuge laufen.  " >&2
    echo "bootstrap: Installieren: brew install tesseract ghostscript tesseract-lang " >&2
fi

echo "bootstrap: fertig. Nutzen: '$VENV/bin/python repair_agent.py --key KEY'"
