"""Import-Audit — die rot-sondierbare Isolations-Sonde des Pakets.

Beweis-Pflicht (DoD): der Service importiert NICHTS aus dem Projekt
(axiom_ng, axiom_ng_runner, axiom_fixsvc, runner). Grenze = Netzwerk-APIs
+ Konfiguration + stdlib. Der Audit parst jeden .py im Paket per AST und
schlägt fehl bei:

  · Imports aus Projekt-Modulen (jede Tiefe, top-level oder lokal),
  · dynamischen Import-Schlupflöchern (__import__/importlib mit
    Projekt-Pfad als String-Literal, sys.path-Umleitung per Zuweisung),
  · Nicht-Whitelist-Imports (erlaubt nur stdlib, gepinnte Reqs und
    paket-interne Module).

Erlaubt: stdlib (ALLOW_STDLIB), die gepinnten Reqs des eigenen Venvs
(ALLOW_THIRD) und alle Module, die im Paketverzeichnis selbst liegen
(config, tools, ...) — die Grenze ist Konfiguration + Netzwerk, nicht das
eigene Paket. Die Prüfung ist AST-basiert: Docstrings und reine
String-Literale schlagen NICHT an, nur echte Calls/Zuweisungen.

Lauf: `python tools/import_audit.py` (nur stdlib) oder via pytest.
"""

from __future__ import annotations

import ast
import sys
from pathlib import Path

PACKAGE = Path(__file__).resolve().parent.parent  # pdf_repair_agent/

ALLOW_STDLIB = {
    "__future__",
    "abc",
    "argparse",
    "ast",
    "asyncio",
    "collections",
    "contextlib",
    "copy",
    "csv",
    "dataclasses",
    "datetime",
    "functools",
    "hashlib",
    "html",
    "http",
    "io",
    "itertools",
    "json",
    "logging",
    "math",
    "os",
    "pathlib",
    "re",
    "shutil",
    "signal",
    "socket",
    "sqlite3",
    "string",
    "subprocess",
    "sys",
    "tempfile",
    "textwrap",
    "time",
    "typing",
    "unicodedata",
    "urllib",
    "uuid",
    "warnings",
}
# Gepinnte Laufzeit-Reqs + Entwicklungs-Reqs des PAKETS.
ALLOW_THIRD = {
    "httpx",
    "pymupdf",
    "fitz",
    "pikepdf",
    "PIL",
    "psycopg2",
    "ocrd",
    "ocrmypdf",
    "pytest",
}
# Projekt-Namen, die auf KEINEN Fall importiert werden dürfen.
FORBIDDEN = {
    "axiom_ng",
    "axiom_ng_runner",
    "axiom_fixsvc",
    "runner",
    "axiom_pipeline",
    "compute_core",
    "integrity_probe",
}


def _module_root(name: str) -> str:
    """Erste Top-Level-Komponente eines Modul-/Attribut-Pfads."""
    return name.split(".")[0] or name


def _package_local_roots(package: Path) -> set[str]:
    """Top-Level-Modulnamen, die im Paketverzeichnis selbst leben.

    Datei-Module (config.py → "config") und Unterpakete (tools/ mit
    __init__.py → "tools"). Nur die oberste Ebene — Import-Wurzeln sind
    per Definition top-level.
    """
    roots: set[str] = set()
    if not package.is_dir():
        return roots
    for p in package.iterdir():
        if p.is_file() and p.suffix == ".py":
            roots.add(p.stem)
        elif p.is_dir() and (p / "__init__.py").exists():
            roots.add(p.name)
    return roots


def _first_literal_arg(call: ast.Call) -> str | None:
    """Erstes Argument, wenn es ein String-Literal ist (sonst None)."""
    a = call.args[0] if call.args else None
    if isinstance(a, ast.Constant) and isinstance(a.value, str):
        return a.value
    return None


def _references_sys_path(node: ast.expr) -> bool:
    """Ziel referenziert sys.path (auch indiziert: sys.path[0], sys.path[:])."""
    if isinstance(node, ast.Attribute):
        return (
            node.attr == "path"
            and isinstance(node.value, ast.Name)
            and node.value.id == "sys"
        )
    if isinstance(node, ast.Subscript):
        return _references_sys_path(node.value)
    return False


def audit_file(path: Path, local_roots: frozenset[str] = frozenset()) -> list[str]:
    """Gibt Verstöße als strings zurück; [] = sauber."""
    findings: list[str] = []
    try:
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    except SyntaxError as e:
        return [f"{path.name}: SyntaxError {e}"]

    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                root = _module_root(alias.name)
                if root in FORBIDDEN:
                    findings.append(f"{path.name}: VERBOTENER Import '{alias.name}'")
                elif root in local_roots:
                    continue  # paket-intern (config, tools, ...) — erlaubt
                elif root not in ALLOW_STDLIB and root not in ALLOW_THIRD:
                    findings.append(f"{path.name}: WHITELIST-FEHLT '{alias.name}'")
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                continue  # relativer Import → paket-interne Grenze, erlaubt
            if node.module:
                root = _module_root(node.module)
                if root in FORBIDDEN:
                    findings.append(
                        f"{path.name}: VERBOTENER import-from '{node.module}'"
                    )
                elif root in local_roots:
                    continue
                elif root not in ALLOW_STDLIB and root not in ALLOW_THIRD:
                    findings.append(
                        f"{path.name}: WHITELIST-FEHLT 'from {node.module}'"
                    )
        elif isinstance(node, ast.Call):
            # Dynamische Schlupflöcher: __import__("axiom_ng...") bzw.
            # importlib.import_module("axiom_ng...") — nur echte Calls mit
            # String-Literal; Docstrings/bloße Namen schlagen nicht an.
            func = node.func
            fname: str | None = None
            if isinstance(func, ast.Name):
                fname = func.id
            elif isinstance(func, ast.Attribute):
                fname = func.attr
            if fname in ("__import__", "import_module"):
                arg = _first_literal_arg(node)
                if arg is not None and _module_root(arg) in FORBIDDEN:
                    findings.append(
                        f"{path.name}: dynamischer Import-Schlupf '{fname}(\"{arg}\")'"
                    )
        elif isinstance(node, (ast.Assign, ast.AugAssign)):
            # sys.path-Umleitung per Zuweisung (sys.path = / [0] = / +=).
            # Methoden-Aufrufe wie sys.path.insert(0, pkg) sind KEINE
            # Zuweisung und bleiben erlaubt (Standard-Pattern in Tests).
            targets = node.targets if isinstance(node, ast.Assign) else [node.target]
            if any(_references_sys_path(t) for t in targets):
                findings.append(f"{path.name}: sys.path-Umleitung per Zuweisung")

    return findings


def audit(package: Path = PACKAGE) -> dict:
    files = sorted(
        p
        for p in package.rglob("*.py")
        if ".venv" not in p.parts and "__pycache__" not in p.parts
    )
    local_roots = frozenset(_package_local_roots(package))
    violations: dict[str, list[str]] = {}
    for f in files:
        fv = audit_file(f, local_roots)
        if fv:
            violations[str(f.relative_to(package))] = fv
    return {
        "scanned": [str(f.relative_to(package)) for f in files],
        "violations": violations,
        "clean": not violations,
    }


def _status_line(result: dict) -> str:
    if result["clean"]:
        return f"ISOLATION GRÜN — {len(result['scanned'])} Dateien, 0 Verstöße"
    n = sum(len(v) for v in result["violations"].values())
    return f"ISOLATION ROT — {n} Verstoß(es) in {len(result['violations'])} Datei(en)"


def main() -> int:
    result = audit()
    print(_status_line(result))
    for vs in result["violations"].values():
        for v in vs:
            print(f"  ✗ {v}")
    for p in sorted(result["scanned"]):
        print(f"  ✓ {p}")
    return 0 if result["clean"] else 1


if __name__ == "__main__":
    sys.exit(main())
