"""G3 agent_loop — Mock-Läufe: Validierung, Budget, Abbruch-Pfade, Evidenz.

Regressionsschutz für die Auto-Review-Criticals:
  C1  ungültige steps terminieren (eine _fail-Pforte, kein Endloslauf)
  C2  nicht registrierte Handler werden NIE als ausgeführt gemeldet;
      Erfolgs-Feedback enthält die Roh-Evidenz
  W1  apply-Typstrenge (boolean, kein truthiness-Raten)
  W2  history enthält nur dispatchte steps (Budget-Stopp landet nicht)
"""

from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

# type: ignore: pyright indiziert paket-lokale Module am Workspace-Root nur
# träge — LSP-False-Positiv; pytest importiert real (siehe Paket-Venv).
from agent_loop import (  # noqa: E402
    DAMAGE_CLASSES,
    TOOL_HELP,
    ToolRegistry,
    run_loop,
    system_header,
    validate_step,
)
from deepseek_client import MockClient  # noqa: E402

SYS = (PKG / "prompts" / "system.txt").read_text(encoding="utf-8")


def _ok_handler(step, cfg):
    return {"ok": True, "evidence": "karte", "action": step["action"]}


def _run(client, *, registry=None, **kw):
    return run_loop(
        client=client,
        system_prompt=SYS,
        task="heile TESTKEY",
        registry=registry if registry is not None else ToolRegistry(),
        **kw,
    )


# ------------------------------------------------- happy path / halt ----


def test_happy_path_probe_surgery_stop():
    client = MockClient(
        [
            '{"action": "probe", "reason": "VORHER"}',
            '{"action": "surgery", "apply": true, "plan_class": "injection", '
            '"operations": []}',
            '{"action": "stop", "reason": "geheilt"}',
        ]
    )
    res = _run(
        client,
        registry=ToolRegistry(handlers={"probe": _ok_handler, "surgery": _ok_handler}),
    )
    assert res.reason == "halt"
    assert res.final_step["action"] == "stop"
    assert res.ops_used == 2  # stop ist terminal und kostet kein Budget
    assert [h["action"] for h in res.history] == ["probe", "surgery"]
    assert len(res.results) == 2
    assert res.results[0]["ok"]


def test_report_is_terminal():
    res = _run(MockClient(['{"action": "report", "content": "Endbericht"}']))
    assert res.reason == "halt"
    assert res.final_step["action"] == "report"
    assert res.ops_used == 0


def test_escalate_is_terminal():
    res = _run(MockClient(['{"action": "escalate", "reason": "unbeweisbar"}']))
    assert res.reason == "halt"
    assert res.final_step["action"] == "escalate"


# ------------------------------------------------ C1: invalid terminiert ----


def test_invalid_step_terminates_bounded():
    # Vor dem Fix: Endlosschleife (kein ops++, kein Budget, kein Zähler).
    client = MockClient(['{"action": "nope"}'] * 100)
    res = _run(client, budget_max_ops=3)
    assert res.reason in ("abort-invalid-step", "budget")
    assert res.ops_used >= 1
    assert len(client.calls) <= 4 < 100  # bewusst begrenzt, nie Endlos


def test_invalid_then_valid_recovers_then_halts():
    client = MockClient(['{"action": "nope"}', '{"action": "stop", "reason": "ok"}'])
    res = _run(client)
    assert res.reason == "halt"
    assert res.ops_used == 1  # der ungültige Versuch zählt


# ------------------------------------------------ C2: Evidenz & unhandled ----


def test_unhandled_is_failure_never_reported_executed():
    # Kein Handler für forensics → 2× → Eskalation; NIE "step ausgeführt".
    client = MockClient(['{"action": "forensics"}'] * 2)
    res = _run(client)
    assert res.reason == "abort-unhandled"
    assert res.final_step["action"] == "escalate"
    # Das Feedback nach dem 1. Versuch muss den Misserfolg benennen …
    fb = client.calls[1][-1].content
    assert "NICHT ausgeführt" in fb
    # … und darf nicht als Erfolg gemeldet werden (stille Falschheit).
    assert "step ausgeführt. Roh-Evidenz" not in fb


def test_success_feedback_contains_raw_evidence():
    client = MockClient(
        ['{"action": "probe"}', '{"action": "stop", "reason": "fertig"}']
    )
    res = _run(client, registry=ToolRegistry(handlers={"probe": _ok_handler}))
    assert res.reason == "halt"
    fb = client.calls[1][-1].content
    assert "Roh-Evidenz" in fb
    assert "karte" in fb  # echte Evidenz aus dem Handler, kein Adjektiv


def test_handler_status_error_counts_as_failure():
    def bad(step, cfg):
        return {"status": "error", "detail": "sonde ROT"}

    client = MockClient(['{"action": "probe"}'] * 2)
    res = _run(client, registry=ToolRegistry(handlers={"probe": bad}))
    assert res.reason == "abort-same-class"
    assert res.final_step["action"] == "escalate"


def test_empty_result_dict_is_failure():
    client = MockClient(['{"action": "probe"}'] * 2)
    res = _run(client, registry=ToolRegistry(handlers={"probe": lambda s, c: {}}))
    assert res.reason == "abort-same-class"


def test_handler_crash_is_failure_not_traceback():
    # G3-Review A: Handler-Crash (z. B. pymupdf auf korruptem PDF) wird
    # Fehlversuch MIT Roh-Evidenz — nie ein Traceback ohne Audit-Spur.
    def boom(step, cfg):
        raise RuntimeError("kaputtes pdf")

    client = MockClient(['{"action": "forensics"}'] * 2)
    res = _run(client, registry=ToolRegistry(handlers={"forensics": boom}))
    assert res.reason in ("abort-same-class", "budget")
    assert res.final_step["action"] in ("escalate", "stop")
    joined = "".join(
        r.get("error", "") for r in res.results if isinstance(r, dict)
    )
    assert "RuntimeError" in joined and "kaputtes pdf" in joined


# ------------------------------------------------ Budget / history ----


def test_budget_stop_caps_history():
    client = MockClient(['{"action": "forensics"}'] * 10)
    res = _run(
        client,
        registry=ToolRegistry(handlers={"forensics": _ok_handler}),
        budget_max_ops=2,
    )
    assert res.reason == "budget"
    assert res.ops_used == 3  # Stopp EIN Op nach dem Limit
    assert len(res.history) == 2  # W2: nur dispatchte steps
    assert len(res.results) == 2


def test_budget_covers_parse_failures():
    # W4: auch Parse-Fehlversuche zählen ins Budget → begrenzt ohne abort.
    client = MockClient(["müll"] * 10)
    res = _run(client, budget_max_ops=2, max_abort_same_class=99)
    assert res.reason == "budget"
    assert len(client.calls) == 3


# ------------------------------------------------ Parse-Pfade ----


def test_parse_abort_after_two():
    client = MockClient(["kein json", "auch nicht"])
    res = _run(client)
    assert res.reason == "abort-parse"
    assert res.final_step["action"] == "escalate"
    assert res.ops_used == 2


def test_single_parse_fail_recovers_then_halts():
    client = MockClient(
        ["ich denke, also bin ich", '{"action": "stop", "reason": "ok"}']
    )
    res = _run(client)
    assert res.reason == "halt"
    assert res.ops_used == 1


def test_parse_fail_echoes_assistant_before_correction():
    client = MockClient(["kein json", '{"action": "stop"}'])
    _run(client)
    msgs = client.calls[1]
    assert msgs[-2].role == "assistant"  # Rohtext-Echo …
    assert msgs[-2].content == "kein json"
    assert msgs[-1].role == "user"  # … dann erst die Korrektur


# ------------------------------------------------ abort-same-class ----


def test_abort_same_class_escalates_after_two():
    def fails(step, cfg):
        return {"ok": False, "failure_of": step.get("plan_class")}

    client = MockClient(
        [
            '{"action": "surgery", "apply": true, "plan_class": "injection", '
            '"operations": []}'
        ]
        * 2
    )
    res = _run(client, registry=ToolRegistry(handlers={"surgery": fails}))
    assert res.reason == "abort-same-class"
    assert "injection" in res.final_step["reason"]
    assert res.ops_used == 2


# ------------------------------------------------ Client-Fehler / cfg ----


class _ExplodingClient:
    def complete(self, messages):
        raise RuntimeError("netz weg")


def test_client_error_returns_loopresult_not_traceback():
    res = _run(_ExplodingClient())
    assert res.reason == "client-error"
    assert res.final_step["action"] == "escalate"


def test_mockclient_exhaustion_is_client_error():
    res = _run(MockClient([]))
    assert res.reason == "client-error"


def test_cfg_passed_through_to_handlers():
    seen = {}

    def spy(step, cfg):
        seen["cfg"] = cfg
        return {"ok": True}

    sentinel = object()
    res = _run(
        MockClient(['{"action": "probe"}', '{"action": "stop"}']),
        registry=ToolRegistry(handlers={"probe": spy}),
        cfg=sentinel,
    )
    assert res.reason == "halt"
    assert seen["cfg"] is sentinel


# ------------------------------------------------ validate_step / Schema ----


def test_validate_unknown_action_rejected():
    ok, err = validate_step({"action": "bogus"})
    assert not ok and "unbekannte action" in err


def test_validate_apply_must_be_boolean():
    # W1: Strings/Zahlen sind KEIN boolean — auch nicht bei Read-Actions.
    for bad_apply in ("false", "true", 1, 0):
        ok, err = validate_step({"action": "probe", "apply": bad_apply})
        assert not ok, bad_apply
        assert "boolean" in err
    ok, _ = validate_step({"action": "probe", "apply": None})  # null → Dry-Run
    assert ok


def test_validate_apply_write_gate():
    ok, err = validate_step({"action": "probe", "apply": True})
    assert not ok and "darf nicht schreiben" in err
    ok, _ = validate_step({"action": "ocr", "apply": True})
    assert ok


def test_validate_surgery_requires_plan_class_and_operations():
    ok, err = validate_step({"action": "surgery", "apply": True, "operations": []})
    assert not ok and "plan_class" in err
    ok, err = validate_step(
        {"action": "surgery", "apply": True, "plan_class": "two-range"}
    )
    assert not ok and "operations" in err


def test_validate_surgery_plan_class_restricted_to_damage_classes():
    # spread/ocr/clean sind Werkzeuge, keine Schadensklassen.
    ok, err = validate_step(
        {"action": "surgery", "apply": True, "plan_class": "spread", "operations": []}
    )
    assert not ok
    for cls in DAMAGE_CLASSES:
        ok, _ = validate_step(
            {"action": "surgery", "apply": True, "plan_class": cls, "operations": []}
        )
        assert ok, cls


def test_validate_non_dict_rejected():
    from typing import Any

    not_a_dict: Any = []  # absichtlich falscher Typ (Regressionsschutz)
    ok, err = validate_step(not_a_dict)
    assert not ok and "kein Objekt" in err


# ------------------------------------------------ Prompt-Verdrahtung ----


def test_system_prompt_documents_step_schema():
    # C3: ein Modell, das NUR system.txt liest, kann einen gültigen step
    # bauen — Schema, apply-Typ und Terminal-Semantik stehen im Prompt.
    for action in (
        "probe",
        "forensics",
        "spread",
        "ocr",
        "surgery",
        "report",
        "escalate",
        "stop",
    ):
        assert action in SYS, action
    assert "boolean" in SYS
    assert "plan_class" in SYS


def test_system_header_lists_all_tools():
    header = system_header()
    for name in TOOL_HELP:
        assert name in header
