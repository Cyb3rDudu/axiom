"""Grenze = Netzwerk-APIs + Konfiguration. Ohne gesetzte Config zeigt
ALLES auf fixtures/ (Sandbox). Produktiv-Anbindung nur durch EXPLIZITE
Config-Umgebungsvariablen.

Kein Import nach außen. Nur stdlib + dieses Paket.
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path

PACKAGE = Path(__file__).resolve().parent

# Registry der Config-Keys; jede hat eine (envar, default).
# Default-Pfade zeigen in das Paket (fixtures/ bzw. runs/) → Sandbox.
_CONFIG_SPEC = {
    # Storage-Quelle für Zotero-Attachments (Zotero-Desktop-Ordner).
    "ZOTERO_STORAGE_ROOT": ("ZOTERO_STORAGE_ROOT", PACKAGE / "fixtures" / "storage"),
    # RAG-API (Messwahrheit Chunk-Seite). Sandbox-Default: nicht erreichbar →
    # Hash-Sync wird BERICHTEnd übersprungen (nie still).
    "RAG_API_BASE": ("RAG_API_BASE", "http://127.0.0.1:9"),  # toter Port = Sandbox
    # DATABASE_URL für Hash-Sync (Rechunk-Wahrheit). Fehlt → berichtet, nie still.
    "DATABASE_URL": ("DATABASE_URL", ""),
    "DEEPSEEK_API_KEY": ("DEEPSEEK_API_KEY", ""),
    "DEEPSEEK_BASE_URL": ("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
    "MODEL": ("PDF_REPAIR_MODEL", "deepseek-chat"),
    # Backup-Ziel für die Kopie vor jeder Operation.
    # Writable-operator defaults (production finding 2026-09-06): a nix
    # store is read-only — PACKAGE-relative paths crash on first write.
    "BACKUP_ROOT": ("PDF_REPAIR_BACKUP_ROOT", Path.home() / ".local" / "state" / "axiom" / "fixer-backup"),
    # Arbeitswurzel für Pläne/Logs/Nachher-PDFs.
    "WORK_ROOT": ("PDF_REPAIR_WORK_ROOT", Path.home() / ".local" / "state" / "axiom" / "fixer-runs"),
    # Budget: maximale Operationen eines Agenten-Laufs.
    "BUDGET_MAX_OPS": ("PDF_REPAIR_BUDGET_MAX_OPS", "50"),
    # Budget (Zeit): maximale Laufzeit eines Agenten-Laufs in Sekunden
    # (Stufe-2 #203). Erzwingt „max M Minuten pro Case" INNERHALB des
    # Agenten — vor dem externen 30-min-Kill von fix.sh — abbruch-sicher.
    # 0 = kein Zeitbudget (Sandbox/Hermetik).
    "BUDGET_MAX_SECONDS": ("PDF_REPAIR_BUDGET_MAX_SECONDS", "900"),
    # Sprachprofile für OCR: kommagetrennte Sprachcodes (z. B. "de", "de,en").
    "LANG_PROFILES": ("PDF_REPAIR_LANG_PROFILES", "de"),
    # Ob Annotation-Sonden geschrieben werden dürfen. Sandbox-Default: nein.
    "PROBE_WRITE": ("PDF_REPAIR_PROBE_WRITE", "0"),
}


@dataclass
class Config:
    zotero_storage_root: Path
    rag_api_base: str
    database_url: str
    deepseek_api_key: str
    deepseek_base_url: str
    model: str
    backup_root: Path
    work_root: Path
    budget_max_ops: int
    budget_max_seconds: int
    lang_profiles: list[str]
    probe_write: bool
    # Beweis woher die Werte kamen (für Audit-Spur/Roh-Evidenz).
    provenance: dict = field(default_factory=dict)

    @property
    def sandbox(self) -> bool:
        """Wahr, wenn keine EXPLIZITE Produktiv-Config gesetzt wurde. In der
        Sandbox zeigt storage auf fixtures/ und die RAG/Zotero-Pfade sind tot."""
        return not self.provenance.get("explicit_storage")

    def ensure_dirs(self) -> None:
        self.backup_root.mkdir(parents=True, exist_ok=True)
        self.work_root.mkdir(parents=True, exist_ok=True)


HERE = Path(__file__).resolve().parent

def _env_int(raw: str, default: int) -> int:
    try:
        return int(raw)
    except (TypeError, ValueError):
        return default


def load_config(env: Mapping[str, str] | None = None) -> Config:
    env = env if env is not None else os.environ
    vals, prov = {}, {}
    for key, (envar, default) in _CONFIG_SPEC.items():
        raw = env.get(envar)
        vals[key] = raw if raw not in (None, "") else default
        if raw not in (None, ""):
            prov[key] = envar
    # Das Storage ist die Scharf-schaltung: nur eine EXPLIZITE
    # ZOTERO_STORAGE_ROOT hebt die Sandbox auf. Alles andere bleibt
    # fixtures/tot und wird im Bericht als Sandbox markiert.
    # Leer-String ist NICHT explizit (gleiche Semantik wie vals oben):
    # nur ein nicht-leerer Wert scharf-schaltet die Sandbox.
    explicit_storage = bool(env.get("ZOTERO_STORAGE_ROOT"))

    return Config(
        zotero_storage_root=Path(vals["ZOTERO_STORAGE_ROOT"]).expanduser(),
        rag_api_base=vals["RAG_API_BASE"],
        database_url=vals["DATABASE_URL"],
        deepseek_api_key=vals["DEEPSEEK_API_KEY"],
        deepseek_base_url=vals["DEEPSEEK_BASE_URL"],
        model=vals["MODEL"],
        backup_root=Path(vals["BACKUP_ROOT"]).expanduser(),
        work_root=Path(vals["WORK_ROOT"]).expanduser(),
        budget_max_ops=_env_int(vals["BUDGET_MAX_OPS"], 50),
        budget_max_seconds=_env_int(vals["BUDGET_MAX_SECONDS"], 0),
        lang_profiles=[
            p.strip() for p in vals["LANG_PROFILES"].split(",") if p.strip()
        ],
        probe_write=_env_int(vals["PROBE_WRITE"], 0) == 1,
        provenance={"explicit_storage": explicit_storage, **prov},
    )


def default_config_path() -> Path:
    """#251: der Default-Suchpfad für config.env — der Operator-Ort ist
    BEWEGLICH und beschreibbar, nie das read-only Nix-Artifact (dort kann
    die Datei niemals liegen; der alte HERE-Default machte JEDE Artifact-
    Deployment zum Exit-1, siehe die 9 gescheiterten Cases vom 2026-09-04).
    Reihenfolge: AXIOM_FIXER_CONFIG (Env) → ~/.config/axiom/fixer.config.env
    → HERE/config.env (Repo-/Dev-Betrieb)."""
    override = os.environ.get("AXIOM_FIXER_CONFIG")
    if override:
        return Path(override)
    home_cfg = Path.home() / ".config" / "axiom" / "fixer.config.env"
    if home_cfg.is_file():
        return home_cfg
    return HERE / "config.env"


def load_config_envfile(
    envfile: str | Path | None = None, env: Mapping[str, str] | None = None
) -> Config:
    """Lädt zuerst eine config.env-Datei (KEY=VALUE-Zeilen, #-Kommentare),
    überlagert dann mit echten Umgebungsvariablen. Der DEEPSEEK_API_KEY darf
    in der Umgebung fehlen und per config.env kommen.

    #251: fehlt der DEFAULT-Pfad, ist das KEIN Fehler — dann greifen die
    Umgebungsvariablen bzw. der dokumentierte Sandbox-Default (vgl.
    config.env.example: 'Ohne diese Datei läuft ALLES in der Sandbox').
    Nur ein EXPLIZIT übergebener Pfad, der nicht existiert, stirbt laut —
    der Aufrufer hat genau diese Datei verlangt."""
    env = dict(os.environ) if env is None else dict(env)
    explicit = envfile is not None
    p = Path(envfile) if envfile is not None else default_config_path()
    if p.is_file():
        for line in p.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            env.setdefault(k.strip(), v.strip())
    elif explicit:
        raise FileNotFoundError(f"config.env nicht gefunden: {p}")
    return load_config(env)


def config_status(cfg: Config) -> dict:
    """Roh-Evidenz der Konfigurationslage — jedes Werkzeug ruft das für
    den Audit-Kopf auf. In der Sandbox explizit '(SANDBOX)'."""
    mode = "SANDBOX" if cfg.sandbox else "EXPLICIT"
    return {
        "mode": mode,
        "zotero_storage_root": str(cfg.zotero_storage_root),
        "rag_api_base": cfg.rag_api_base,
        "database_url_configured": bool(cfg.database_url),
        "deepseek_configured": bool(cfg.deepseek_api_key),
        "model": cfg.model,
        "backup_root": str(cfg.backup_root),
        "work_root": str(cfg.work_root),
        "budget_max_ops": cfg.budget_max_ops,
        "budget_max_seconds": cfg.budget_max_seconds,
        "lang_profiles": cfg.lang_profiles,
        "probe_write": cfg.probe_write,
        "sources": sorted(cfg.provenance.keys()),
    }
