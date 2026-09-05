"""Import-Guard: beweist die Isolation (Kein Projekt-Import, Sandbox-Default).

Rot-sondierbar: führt jemand einen `from axiom_ng... import` in das Paket
ein, schlägt dieser Test fehl. Löst import_audit.audit() gegen das Paket.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from tools import import_audit


def test_isolierung_import_audit_clean():
    r = import_audit.audit()
    assert r["clean"], f"Isolationsverletzungen: {r['violations']}"
    assert r["scanned"], "Audit hat keine Dateien geprüft — Aussage leer"
    # Die Grenze muss in der Signatur der Whitelist erkennbar sein.
    assert "pymupdf" in import_audit.ALLOW_THIRD


def test_sandbox_ist_default_ohne_config():
    # Synthetische Umgebung ohne jede Repair-Variable: storage zeigt auf
    # fixtures/ (Sandbox) — unabhängig vom echten os.environ des Rechners.
    from config import config_status, load_config

    cfg = load_config({"PATH": "/usr/bin:/bin"})
    assert cfg.sandbox
    assert "fixtures" in str(cfg.zotero_storage_root)

    # Kein konfigurierter DATABASE_URL/RAG → Hash-Sync wird als übersprungen
    # markiert, nicht still verschwiegen.
    st = config_status(cfg)
    assert st["mode"] == "SANDBOX"
    assert st["database_url_configured"] is False


def test_leerer_storage_env_bleibt_sandbox():
    # Regression: ZOTERO_STORAGE_ROOT="" ist NICHT explizit — Sandbox bleibt
    # wahr, der Modus lügt nicht mit EXPLICIT über fixtures-Pfaden.
    from config import config_status, load_config

    cfg = load_config({"ZOTERO_STORAGE_ROOT": ""})
    assert cfg.sandbox is True
    assert config_status(cfg)["mode"] == "SANDBOX"
    assert "fixtures" in str(cfg.zotero_storage_root)


def test_explizite_config_hebt_sandbox_auf():
    from config import load_config

    # Paketlokaler Fake-Storage (kein /tmp-Kontakt, Sandbox bleibt feststellbar).
    fake = Path(__file__).parent.parent / "fixtures" / "fake-storage"
    env = dict(os.environ)
    env["ZOTERO_STORAGE_ROOT"] = str(fake)
    cfg = load_config(env)
    assert not cfg.sandbox
    assert cfg.zotero_storage_root == fake


def test_config_envfile_erkennung_auch_ohne_umgebungs_key():
    from config import load_config_envfile

    env = {"PATH": os.environ.get("PATH", "")}
    # DEEPSEEK nur über config.env — nicht über echte Umgebung.
    p = Path(__file__).parent.parent / "config.env"
    if not p.exists():
        from tempfile import NamedTemporaryFile

        with NamedTemporaryFile("w", suffix=".env", delete=False) as f:
            f.write("# fake\nDEEPSEEK_API_KEY=fake-key\n")
            p = Path(f.name)
        try:
            cfg = load_config_envfile(p, env=env)
            assert cfg.deepseek_api_key == "fake-key"
        finally:
            p.unlink(missing_ok=True)
    else:
        cfg = load_config_envfile(p, env=env)
        assert cfg.deepseek_api_key


def test_config_default_fehlend_ist_sandbox_nicht_tod(tmp_path, monkeypatch):
    """#251: der 9-Case-Befund — der alte Default zeigte ins read-only
    Nix-Artifact und eine FEHLENDE Datei war Exit 1. Der Default-Pfad darf
    fehlen: Env-/Sandbox-Betrieb greift (designgemäß, config.env.example)."""
    import config as config_module
    from config import load_config_envfile

    monkeypatch.delenv("AXIOM_FIXER_CONFIG", raising=False)
    (tmp_path / "artifact").mkdir(exist_ok=True)  # leer: KEINE config.env
    monkeypatch.setattr(
        config_module, "default_config_path",
        lambda: tmp_path / "artifact" / "config.env",
    )
    env = {"PATH": os.environ.get("PATH", "")}
    cfg = load_config_envfile(None, env=env)  # darf NICHT werfen
    assert cfg.sandbox, "ohne config.env und ohne Env: Sandbox-Default"


def test_config_explicit_fehlend_stirbt_laut(tmp_path):
    """Ein EXPLIZIT geforderter Pfad, der nicht existiert, bleibt ein
    lauter Fehler — der Aufrufer hat genau diese Datei verlangt."""
    import pytest

    from config import load_config_envfile

    with pytest.raises(FileNotFoundError):
        load_config_envfile(tmp_path / "nie_da.env", env={})


def test_config_default_pfad_suche(monkeypatch, tmp_path):
    """#251: AXIOM_FIXER_CONFIG gewinnt, dann ~/.config/axiom/, dann HERE."""
    import config as config_module

    monkeypatch.delenv("AXIOM_FIXER_CONFIG", raising=False)
    monkeypatch.setattr(config_module, "HERE", tmp_path)
    # Hermetizität (#253): ein realer ~/.config/axiom/fixer.config.env
    # (Akzeptanzlauf) darf diesen Test nicht umdrehen — home auf tmp_path
    # zeigen lassen, dort existiert KEINE config.
    monkeypatch.setattr(
        config_module.Path, "home", staticmethod(lambda: tmp_path)
    )
    # kein home-cfg, kein HERE-cfg → HERE-Fallback
    assert config_module.default_config_path() == tmp_path / "config.env"
    # AXIOM_FIXER_CONFIG (Env) gewinnt über alles
    monkeypatch.setenv("AXIOM_FIXER_CONFIG", "/etc/ops/fixer.env")
    assert str(config_module.default_config_path()) == "/etc/ops/fixer.env"
    monkeypatch.delenv("AXIOM_FIXER_CONFIG")


def test_config_home_pfad_gewinnt_ueber_here(monkeypatch, tmp_path):
    """~/.config/axiom/fixer.config.env (beschreibbarer Operator-Ort)
    schlägt den Artifact-HERE-Fallback — der 9-Case-Kernbefund."""

    import config as config_module

    monkeypatch.delenv("AXIOM_FIXER_CONFIG", raising=False)
    monkeypatch.setattr(config_module, "HERE", tmp_path / "artifact")
    home = tmp_path / "fakehome"
    (home / ".config" / "axiom").mkdir(parents=True, exist_ok=True)
    (home / ".config" / "axiom" / "fixer.config.env").write_text(
        "DEEPSEEK_API_KEY=home-key\n"
    )
    monkeypatch.setattr(config_module.Path, "home", staticmethod(lambda: home))
    p = config_module.default_config_path()
    assert p == home / ".config" / "axiom" / "fixer.config.env"
    # und sie wird auch GELESEN (nicht nur gefunden)
    cfg = config_module.load_config_envfile(
        None, env={"PATH": os.environ.get("PATH", "")}
    )
    assert cfg.deepseek_api_key == "home-key"
