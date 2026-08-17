"""#184 — the fix-service: DeepSeek judge + mechanical footer verification.

Standalone by design: DeepSeek credentials live HERE (service env), the RAG
never sees them; Zotero credentials live in the RAG, this service never
sees them. One direction of traffic: this service CALLS the RAG repair API.

Loop per case (every phase logs one line, dudu watches):
  preflight REJECT (same code path as the final GREEN check — pdf_health)
  → judge (DeepSeek: assessment + Zielzustands-plan in Anker-Form)
  → verify (mechanical footer truth; coverage/contradictions)
  → auto-apply via RAG (quarantine → zotero delete → create/upload with
    schema filename) or blocked_for_dudu
  → sync → wait for the rechunk → preflight GREEN + folio_verified proof

Plan format (dudu's W4 pre-decision): Zielzustands-Pläne in Anker-Form —
  {"sections": [{"from_page": 11, "to_page": 67, "start_label": 2}, ...],
   "gaps": [68], "confidence": 0.9}
sections describe the TARGET label map (1-based inclusive; arabic +1/page);
gaps document real print jumps between sections. The LLM never writes
bytes: the healed PDF is generated mechanically from the VERIFIED plan.
"""
from __future__ import annotations

import io
import json
import os
import re
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import pymupdf
import requests

from axiom_ng_runner.compute_core import page_trust as pt
from axiom_ng_runner.compute_core.pdf_health import preflight

RAG = os.environ.get("AXIOM_RAG_URL", "http://127.0.0.1:8011")
DEEPSEEK_KEY = os.environ.get("DEEPSEEK_API_KEY", "")
DEEPSEEK_URL = os.environ.get("DEEPSEEK_BASE_URL", "https://api.deepseek.com") + "/chat/completions"
MODEL = os.environ.get("DEEPSEEK_MODEL", "deepseek-chat")

ARABIC = re.compile(r"^\d{1,4}$")


def log(buch: str, phase: str, erg: str) -> None:
    print(f"[{buch}] {phase}: {erg}", flush=True)


# ── judge ────────────────────────────────────────────────────────────────

JUDGE_PROMPT = """Du bist ein PDF-Reparatur-Richter. Ein Buch hat kaputte PDF-Seitenlabels.
Du erhältst: den Analyse-Befund und die fußzeilen-/kopfzeilen-gelesenen
Seitenzahlen (physische Seite → gedruckte Zahl, 1-basiert).

Deine Aufgabe: ein ZIELZUSTANDS-PLAN in Anker-Form — zusammenhängende
Sektionen, die die NEUE Label-Belegung beschreiben:
{{"sections": {{"from_page": <int 1-basiert>, "to_page": <int>, "start_label": <int>}}, ...],
 "gaps": [<Seiten zwischen Sektionen ohne Nummer>],
 "confidence": <0..1>}}

Regeln: Sektionen aufsteigend, nicht überlappend, jede vollständig von den
gelesenen Zahlen gedeckt (start_label = erste gelesene Zahl der Sektion;
die Folge muss +1 pro Seite sein — sonst trennen!). Seiten ohne Lesung am
Anfang (Cover) bekommen KEINE Sektion. gaps dokumentieren echte Sprünge.
Antworte NUR mit dem JSON-Objekt.

Analyse: {analyse}
Gelesene Seitenzahlen (physisch 1-basiert → gedruckt): {candidates}"""


def judge(buch: str, analyse: dict, candidates: dict[int, str]) -> dict:
    """DeepSeek assessment → Zielzustands-plan (validated). LLM never writes bytes."""
    if not DEEPSEEK_KEY:
        raise RuntimeError("DEEPSEEK_API_KEY fehlt (Service-Env)")
    # candidates arrive 0-BASED (extract_folio_candidates keys) — the plan
    # world is 1-BASED pages. Convert HERE or the judge builds every section
    # one page early (live bug, Controlling: 573/575 contradictions).
    cand = {int(k) + 1: str(v).strip() for k, v in candidates.items() if ARABIC.match(str(v).strip())}
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": JUDGE_PROMPT.format(
            analyse=json.dumps(analyse, ensure_ascii=False),
            candidates=json.dumps(cand, ensure_ascii=False))}],
        "response_format": {"type": "json_object"},
        "temperature": 0,
    }
    r = requests.post(DEEPSEEK_URL, json=body, timeout=120,
                      headers={"Authorization": f"Bearer {DEEPSEEK_KEY}"})
    r.raise_for_status()
    plan = json.loads(r.json()["choices"][0]["message"]["content"])
    plan = validate_plan(plan)
    log(buch, "JUDGE", f"Plan v1: {len(plan['sections'])} Sektionen "
        f"({plan['sections'][0]['from_page']}–{plan['sections'][-1]['to_page']}), "
        f"{len(plan['gaps'])} Lücken, confidence={plan.get('confidence')}")
    return plan


def validate_plan(plan: dict) -> dict:
    secs = sorted(plan.get("sections", []), key=lambda s: s["from_page"])
    if not secs:
        raise ValueError("Plan ohne Sektionen")
    last = 0
    for s in secs:
        for k in ("from_page", "to_page", "start_label"):
            if not isinstance(s.get(k), int):
                raise ValueError(f"Sektion {s}: {k} fehlt/kein int")
        if s["from_page"] < 1 or s["to_page"] < s["from_page"] or s["from_page"] <= last:
            raise ValueError(f"Sektionen überlappen/nicht aufsteigend: {s}")
        last = s["to_page"]
    plan["sections"] = secs
    plan.setdefault("gaps", [])
    plan.setdefault("confidence", 0)
    return plan


def label_at(plan: dict, page_1b: int) -> int | None:
    for s in plan["sections"]:
        if s["from_page"] <= page_1b <= s["to_page"]:
            return s["start_label"] + (page_1b - s["from_page"])
    return None


# ── mechanical footer verification ───────────────────────────────────────

def verify(plan: dict, pdf_path: str) -> tuple[float, int, dict]:
    """Coverage/contradictions of the plan against the FOOTER truth.

    Observable pages = pages whose top line carries an arabic number. A plan
    label matching the printed number = covered; a mismatch = contradiction;
    an observable page outside all sections counts against coverage.
    """
    doc = pymupdf.open(pdf_path)
    try:
        cands = pt.extract_folio_candidates(doc)
    finally:
        doc.close()
    covered = contra = outside = 0
    mismatches: list[dict] = []
    for page0, val in cands.items():
        if not ARABIC.match(str(val).strip()):
            continue
        want = label_at(plan, page0 + 1)
        if want is None:
            outside += 1
        elif want == int(val):
            covered += 1
        else:
            contra += 1
            if len(mismatches) < 3:
                mismatches.append({"seite": page0 + 1, "plan": want, "druck": int(val)})
    observable = covered + contra + outside
    coverage = covered / observable if observable else 0.0
    stats = {"observable": observable, "covered": covered,
             "contradictions": contra, "outside_sections": outside,
             "beispiel_mismatches": mismatches}
    return coverage, contra, stats


# ── healed PDF (mechanical, from the verified plan) ─────────────────────

def build_healed_pdf(plan: dict, pdf_path: str) -> bytes:
    """Healed PDF from the VERIFIED plan (mechanical — the LLM never writes bytes).

    pymupdf label-spec facts (all live-learned, pinned):
      - key is 'firstpagenum' (a 'first' key is silently ignored)
      - 'startpage' is 0-BASED
      - a range MUST cover page 0 or get_label() IndexErrors on earlier pages
        (crashed the carrier runner)
      - save WITHOUT garbage=3: the aggressive xref rewrite corrupted the heap
        in marker's C layer (free(): chunks in smallbin corrupted, live)
    """
    doc = pymupdf.open(pdf_path)
    try:
        spec = []
        if plan["sections"][0]["from_page"] > 1:
            spec.append({"startpage": 0, "prefix": "C", "style": "D", "firstpagenum": 1})
        for s in plan["sections"]:
            spec.append({"startpage": s["from_page"] - 1, "prefix": "", "style": "D",
                         "firstpagenum": s["start_label"]})
        doc.set_page_labels(spec)
        out = io.BytesIO()
        doc.save(out, deflate=True)
        return out.getvalue()
    finally:
        doc.close()


# ── the loop ─────────────────────────────────────────────────────────────

def run_case(case: dict) -> None:
    buch = case["title"]
    pdf = case["local_path"].replace("file://", "")
    cid = case["id"]

    pf = preflight(pdf)
    log(buch, "PREFLIGHT", pf.line()[len("[x] "):])
    if pf.ok:
        requests.post(f"{RAG}/api/repair/cases/{cid}/verdict", timeout=60, data={
            "verdict": "failed", "score": 0, "contradictions": 0,
            "blocked_reason": f"preflight nicht rot: {pf.verdacht}", "plan": "{}"})
        return

    doc = pymupdf.open(pdf)
    try:
        cands = pt.extract_folio_candidates(doc)
    finally:
        doc.close()
    plan = judge(buch, pf.details, cands)

    coverage, contra, stats = verify(plan, pdf)
    log(buch, "VERIFY", f"fußzeilen-deckung {coverage:.1%} · {contra} widersprüche · {stats}")

    if coverage >= 0.95 and contra == 0:
        pdf_bytes = build_healed_pdf(plan, pdf)
        with open("/tmp/healed.pdf", "wb") as f:
            f.write(pdf_bytes)
        r = requests.post(f"{RAG}/api/repair/cases/{cid}/verdict", timeout=300,
                          data={"verdict": "auto_apply", "score": coverage,
                                "contradictions": contra, "plan": json.dumps(plan, ensure_ascii=False),
                                "plan_version": 1},
                          files={"healed_pdf": ("healed.pdf", pdf_bytes, "application/pdf")})
        r.raise_for_status()
        erg = r.json()
        log(buch, "AUTO-APPLY", f"angewendet: {erg.get('filename')} · neu {erg.get('new_attachment_key')} · quarantäne {erg.get('quarantine')}")
    else:
        r = requests.post(f"{RAG}/api/repair/cases/{cid}/verdict", timeout=60, data={
            "verdict": "blocked", "score": coverage, "contradictions": contra,
            "blocked_reason": f"deckung {coverage:.1%} / {contra} widersprüche unter schwelle",
            "plan": json.dumps(plan, ensure_ascii=False)})
        r.raise_for_status()
        log(buch, "GATE", f"blocked_for_dudu (deckung {coverage:.1%}, {contra} widersprüche)")
        return

    # sync → rechunk → proof
    n_jobs = requests.post(f"{RAG}/api/zotero/sync", timeout=300).json().get("enqueued_jobs")
    log(buch, "SYNC", f"{n_jobs} Jobs enqueued")
    for _ in range(90):
        time.sleep(20)
        jobs = requests.get(f"{RAG}/api/ingest/jobs?limit=20", timeout=30).json()
        items = jobs.get("jobs", jobs) if isinstance(jobs, dict) else jobs
        # the endpoint reports Capitalized keys (Status) — accept both
        offen = [j for j in items if isinstance(j, dict) and
                 (j.get("status") or j.get("Status")) in ("pending", "processing", "claimed")]
        if not offen:
            break
        log(buch, "RECHUNK", f"{len(offen)} job(s) offen …")
    stats = requests.get(f"{RAG}/api/repair/docs/{case['document_zotero_key']}/locator-stats", timeout=30).json()
    log(buch, "BEWEIS", json.dumps(stats, ensure_ascii=False)[:600])


def main() -> int:
    if not DEEPSEEK_KEY:
        print("DEEPSEEK_API_KEY fehlt", file=sys.stderr)
        return 2
    q = requests.get(f"{RAG}/api/repair/queue", timeout=30).json()["cases"]
    log("fixsvc", "POLL", f"{len(q)} Fall/Fälle in der Queue")
    for case in q:
        cid = case["id"]
        r = requests.post(f"{RAG}/api/repair/cases/{cid}/claim", timeout=60)
        if r.status_code != 200:
            log(case["title"], "CLAIM", f"abgelehnt: {r.text[:120]}")
            continue
        log(case["title"], "CLAIM", "in_repair (Versuch gezählt)")
        try:
            run_case(case)
        except Exception as exc:  # noqa: BLE001 — the case must not die silently
            log(case["title"], "FEHLER", str(exc)[:200])
            requests.post(f"{RAG}/api/repair/cases/{cid}/verdict", timeout=60, data={
                "verdict": "failed", "score": 0, "contradictions": 0,
                "blocked_reason": f"service-fehler: {exc}"[:300], "plan": "{}"})
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
