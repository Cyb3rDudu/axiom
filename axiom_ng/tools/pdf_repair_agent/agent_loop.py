"""agent_loop — EIGENE Schleife (kein Framework).

System-Prompt (versionierte Datei prompts/system.txt) + Tool-Registry +
DeepSeek-Client (injizierbar; Mock für Tests) + Budget-Grenzen + JSON-`step`-
Validierung vor jeder Ausführung.

Die Schleife lädt den System-Prompt, schickt den Auftrag, parst jede
Modell-Antwort als JSON-`step` und validiert ihn gegen die Zulassungsregeln,
BEVOR er auf die Tool-Registry gemappt wird. Das Modell kann nur schreiben,
indem es einen zulässigen step mit tool-Aufruf liefert — nie direkt Bytes.

Fehlversuchs-Disziplin: ALLE Nicht-Erfolge (nicht parsebar, ungültiger
step, kein Handler, Handler-Misserfolg) laufen durch EINE `_fail`-Pforte —
ops zählen, Klasse zählen, Budget-Tor, Eskalation nach max_abort. Kein
Pfad ist unbegrenzt; nichts wird je als „ausgeführt" gemeldet, was nicht
ausgeführt wurde (stille Falschheit verboten).
"""

from __future__ import annotations

import json
import sys
from dataclasses import dataclass, field
from pathlib import Path

HERE = Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

from deepseek_client import (  # noqa: E402  # type: ignore[reportMissingImports]
    ChatMessage,
    parse_step,
)

# ------------------------------------------------------------------ step ----


VALID_ACTIONS = {
    "probe",
    "forensics",
    "spread",
    "ocr",
    "surgery",
    "report",
    "escalate",
    "stop",
}
ALLOWED_WRITE = {"spread", "ocr", "surgery"}  # nur diese dürfen apply=true schreiben
# Terminal-Actionen schließen den Fall (Endbericht/Eskalation) und kosten
# kein Operationsbudget.
TERMINAL_ACTIONS = ("stop", "escalate", "report")

# Die 5 Schadensklassen (Conformance Stufe-1, scripts/pdf_label_surgery.py).
# surgery-plan_class ist auf genau diese beschränkt: spread/ocr/clean sind
# Werkzeuge bzw. Ergebnisse, keine Label-Schadensklassen — sie laufen über
# ihre eigene action, nie über plan_class.
DAMAGE_CLASSES = (
    "constant-offset",
    "reprint-start",
    "two-range",
    "injection",
    "unclassifiable",
)


def validate_step(step: dict) -> tuple[bool, str]:
    """Zulassungsprüfung eines Modell-`step` VOR jeder Ausführung.

    Regeln (identisch zum Abschnitt „step-Schema" in prompts/system.txt):
    action bekannt · apply nur bei Write-Action und nur als echtes boolean
    (Strings/Zahlen werden abgewiesen, nicht truthiness-geraten) · surgery
    braucht plan_class aus DAMAGE_CLASSES + operations."""
    if not isinstance(step, dict):
        return False, "step ist kein Objekt"
    action = step.get("action")
    if not isinstance(action, str) or action not in VALID_ACTIONS:
        return False, f"unbekannte action '{action}'"
    apply = step.get("apply", False)
    if apply is not None and not isinstance(apply, bool):
        return (
            False,
            f"apply muss boolean sein (true/false), ist {type(apply).__name__}",
        )
    if apply and action not in ALLOWED_WRITE:
        return False, f"action '{action}' darf nicht schreiben (apply verboten)"
    if action == "surgery":
        pc = step.get("plan_class")
        if not isinstance(pc, str) or pc not in DAMAGE_CLASSES:
            return (
                False,
                f"surgery braucht plan_class aus den 5 Schadensklassen: {pc!r}",
            )
        if step.get("operations") is None:
            return False, "surgery-step ohne operations"
    return True, "ok"


# --------------------------------------------------------------- registry ----

TOOL_HELP = {
    "probe": "integrity_probe (Dreifach-Sonde) — Messwahrheit vor/nach",
    "forensics": "forensics_tool — DRUCK-STRUKTUR-KARTE (T3)",
    "spread": "spread_tool — 2up-Erkennung/-Trennung (T1)",
    "ocr": "ocr_tool — Textschicht (T2)",
    "surgery": "surgery_exec — EINZIGER Label-Schreibpfad (T4)",
}


def system_header() -> str:
    """Lebendige Werkzeug-Registry als Prompt-Anhang: repair_agent hängt
    diesen Block an prompts/system.txt, damit Schema und Registry nicht
    auseinanderlaufen (TOOL_HELP ist damit kein toter Code)."""
    lines = ["", "## Werkzeug-Registry (Dispatcher, lebendig)", ""]
    for name in sorted(TOOL_HELP):
        lines.append(f"  {name:<10} {TOOL_HELP[name]}")
    lines.append("")
    return "\n".join(lines)


def _is_failure(result) -> bool:
    """Ergebnis-Vertrag Handler → Schleife. Ein dispatch-Ergebnis gilt genau
    dann als Fehlversuch, wenn es

      · kein nicht-leeres dict ist, oder
      · handled=False trägt (kein Handler registriert), oder
      · ok falsy ist (Default: Erfolg), oder
      · status=='error' / error gesetzt / failure_of gesetzt ist.

    Alles andere ist Erfolg MIT Roh-Evidenz. Der Schleife ist es verboten,
    einen Fehlversuch als „ausgeführt" zu melden — stille Falschheit."""
    if not isinstance(result, dict) or not result:
        return True
    if "handled" in result and not result["handled"]:
        return True
    if not result.get("ok", True):
        return True
    return bool(
        result.get("status") == "error"
        or result.get("error")
        or result.get("failure_of")
    )


@dataclass
class ToolRegistry:
    """Map action -> Callable(step, cfg) -> dict (Roh-Evidenz).

    Handler-Rückgabe-Vertrag: siehe `_is_failure` — Fehlschlag NUR explizit
    via ok/status/error/failure_of bzw. handled=False, nie still. None als
    Handler bedeutet „nur validiert/budgetiert" (Ausführung obliegt
    repair_agent); ein solcher Dispatch gilt als NICHT ausgeführt."""

    handlers: dict = field(default_factory=dict)

    def dispatch(self, action: str, step: dict, cfg) -> dict:
        fn = self.handlers.get(action)
        if fn is None:
            return {
                "action": action,
                "handled": False,
                "why": "handler nicht registriert (Ausführung obliegt "
                "repair_agent — Schleife validiert nur)",
            }
        return fn(step, cfg)


# ------------------------------------------------------------ loop driver ----


@dataclass
class LoopResult:
    final_step: dict
    ops_used: int
    reason: str
    history: list = field(default_factory=list)  # wirklich dispatchte steps
    results: list = field(default_factory=list)  # deren Roh-Evidenz (parallel)


def run_loop(
    *,
    client,
    system_prompt: str,
    task: str,
    registry: ToolRegistry,
    cfg=None,
    budget_max_ops: int = 50,
    max_abort_same_class: int = 2,
    collect_history: bool = True,
) -> LoopResult:
    """Führt die Schleife: Modell-Antwort → step → validieren → dispatchen.

    Beendet bei: stop/report/escalate (halt) · Budget (budget) · max_abort
    Fehlversuchen derselben Klasse (abort-parse/-invalid-step/-unhandled/
    -same-class) · Client-Fehler (client-error). ops zählt jeden
    nicht-terminalen Modell-Rundflug inklusive Fehlversuche — das Budget
    deckt damit JEDE Zählstelle. cfg wird 1:1 an jeden Handler
    durchgereicht (repair_agent injiziert die Config)."""

    messages = [ChatMessage("system", system_prompt), ChatMessage("user", task)]
    ops = 0
    class_fails: dict[str, int] = {}
    history = [] if collect_history else None
    results = [] if collect_history else None
    fin = {"action": "stop", "reason": "no-iteration"}
    reason = "no-iteration"

    def _fail(
        cls: str,
        abort_reason: str,
        feedback: str,
        echo: str | None = None,
        count_op: bool = True,
    ) -> LoopResult | None:
        """DIE eine Fehlversuchs-Pforte: ops++ (außer bereits gezählt),
        Klassenzähler++, Budget-Tor, Eskalation nach max_abort. Rückgabe
        LoopResult => Schleife endet damit; None => Feedback & weiter."""
        nonlocal ops
        if count_op:
            ops += 1
        class_fails[cls] = class_fails.get(cls, 0) + 1
        if ops > budget_max_ops:
            return LoopResult(
                final_step={
                    "action": "stop",
                    "reason": f"budget überschritten ({ops}>{budget_max_ops})",
                },
                ops_used=ops,
                reason="budget",
                history=history or [],
                results=results or [],
            )
        if class_fails[cls] >= max_abort_same_class:
            return LoopResult(
                final_step={
                    "action": "escalate",
                    "reason": f"{max_abort_same_class} Fehlversuche Klasse '{cls}'",
                },
                ops_used=ops,
                reason=abort_reason,
                history=history or [],
                results=results or [],
            )
        # Rollenpaar halten: erst Assistenten-Echo, dann Korrektur (nie zwei
        # user-Nachrichten hintereinander).
        if echo is not None:
            messages.append(ChatMessage("assistant", echo))
        messages.append(ChatMessage("user", feedback))
        return None

    while True:
        try:
            raw = client.complete(messages)
        except Exception as exc:  # noqa: BLE001 — Client-Fehler ist Beweis, kein Crash
            return LoopResult(
                final_step={"action": "escalate", "reason": f"client-fehler: {exc}"},
                ops_used=ops,
                reason="client-error",
                history=history or [],
                results=results or [],
            )

        try:
            step = parse_step(raw)
        except ValueError as exc:
            done = _fail(
                "__parse",
                "abort-parse",
                f"deine letzte Antwort war kein gültiger step ({exc}) — "
                f"liefere GENAU EIN JSON-Objekt nach step-Schema.",
                echo=raw,
            )
            if done is not None:
                return done
            continue

        ok, err = validate_step(step)
        if not ok:
            done = _fail(
                "__invalid",
                "abort-invalid-step",
                f"step ungültig: {err} — liefere einen zulässigen step.",
                echo=str(step),
            )
            if done is not None:
                return done
            continue

        action = step["action"]
        if action in TERMINAL_ACTIONS:
            fin = step
            reason = "halt"
            break

        ops += 1
        if ops > budget_max_ops:
            return LoopResult(
                final_step={
                    "action": "stop",
                    "reason": f"budget überschritten ({ops}>{budget_max_ops})",
                },
                ops_used=ops,
                reason="budget",
                history=history or [],
                results=results or [],
            )

        # Ausführung — history/results NUR für wirklich dispatchte steps
        # (kein Budget-Abbruch landet in der Audit-Spur).
        result = registry.dispatch(action, step, cfg)
        if history is not None:
            history.append(step)
        if results is not None:
            results.append(result)
        evidence = json.dumps(result, ensure_ascii=False, default=str)
        if len(evidence) > 4000:
            evidence = evidence[:4000] + "…(gekürzt)"
        cls = step.get("plan_class") or action
        unhandled = (
            isinstance(result, dict) and "handled" in result and not result["handled"]
        )
        if _is_failure(result):
            done = _fail(
                f"__unhandled:{action}" if unhandled else cls,
                "abort-unhandled" if unhandled else "abort-same-class",
                (
                    f"schritt '{action}' wurde NICHT ausgeführt (kein Handler "
                    f"registriert) — behandle ihn nie als erledigt; "
                    f"diagnostiziere anders oder eskaliere."
                    if unhandled
                    else f"schritt '{action}' ist fehlgeschlagen — "
                    f"Roh-Evidenz:\n{evidence}\n"
                    f"Diagnostiziere erneut oder eskaliere."
                ),
                echo=json.dumps(step, ensure_ascii=False),
                count_op=False,  # op wurde beim Dispatch bereits gezählt
            )
            if done is not None:
                return done
            continue

        messages.append(ChatMessage("assistant", json.dumps(step, ensure_ascii=False)))
        messages.append(
            ChatMessage(
                "user",
                f"step ausgeführt. Roh-Evidenz:\n{evidence}\n"
                f"Messe, belege oder schließe — liefer den nächsten "
                f"zulässigen step (oder report/stop/escalate).",
            )
        )

    return LoopResult(
        final_step=fin,
        ops_used=ops,
        reason=reason,
        history=history or [],
        results=results or [],
    )
