# Mission 7acd5677 — Monitor-Log (laufen lassen, später fixen)

Stand: 2026-07-17 ~20:45 carrier-local. Mission läuft noch (Research + Writing/Reflection Phase).
Branch-Commits aktiv: `72d3f48` (reasoning-leak fix), `4306ae8` (V4/junk fixes).

## ✅ Was KORREKT läuft
- **Kein Reasoning-Leak mehr im Writing-Pfad**: 0 leere-Content-Cases bei `agent_mode=writing` → Writing-Calls liefern direkt echten Prosa-Content, kein force nötig, keine Promotion → kein "reasoning report generator"-Regression.
- **Forced-Retry funktioniert**: 138× angewendet, 124× danach erfolgreicher Call (Attempt 2/3 oder 3/3) → holt echten Content zurück.
- **Quick-Analysis-Failures sind HARMLOS**: "chunk sufficiency returned empty → Defaulting to fetch full document" → graceful Fallback, Notes werden trotzdem generiert (45 relevante Notes).
- **Junk-Quellen gefiltert**: keine soccerway/zhihu/langenscheidt mehr; relevante Quellen (Springer, destatis, Porter).

## ⚠️ PROBLEME für später

### P1 — Quick Analysis (chunk-sufficiency) scheitert DETERMINISTISCH auf V4
- **Symptom**: 49 Quick-Analysis-Failures, 0 Success (vorher mit Promotion: 26 Success).
- **Ursache**: Der Chunk-Sufficiency-Call liefert auf V4-flash leereren Content zurück, den auch der forced Retry oft nicht recovered → 3× Fail → graceful Fallback auf "full document fetch".
- **Impact**: Verschwendet API-Calls + Zeit (extra Fetch + Retry), aber funktional OK (Fallback holt Volltext).
- **Werten später**: Warum dieser spezifische Prompt deterministisch leer zurückkommt. evtl. `max_tokens=20` für diesen Call (DEBUG sah "forcing max_tokens=65536 (was 20)") — ein Thinking-Modell mit derart kleinem Caller-Wunsch "denkt nur". -> Fix-Idee: chunk-sufficiency sollte für Thinking-Modelle KEIN LLM sein (direkt Volltext fetchen) oder größeres min-Tokens.

### P2 — Hohe Empty-Content-Rate bei V4 overall (226 Fälle / 60 min)
- **Symptom**: 226 "empty content + reasoning" Flags, viel häufiger als Stresstest (0/8 leer) vermuten ließ.
- **BESTÄTIGT (input-size-getrieben)**: alle 110 empty-Cases sind `agent_mode=research` (Note-Generierung aus 32K-Content-Chunks) + 20 `query_strategy`. **0 in `writing`** → Writing-Pfad sauber (kein Leak-Risiko). Echte Research-Prompts mit großen Chunks triggern den leeren-Content-Edge-Case deterministisch; der forced Retry recovered ~55% davon (124/138 success), Rest fällt in graceful Fallback.
- **Werten später**: Reproduzierbaren Test mit echter Chunk-Größe bauen; ggf. Reasoning-Effort-Param für V4 setzen oder kleinere Chunks / non-thinking-Modell für die Note-Generierung verwenden.

### ✅ Positiv bestätigt: Writing-Pfad sauber
- `agent_mode=writing`: 0 empty-content-Cases → Writing-Calls liefern direkt Prosa, kein force, keine Promotion → **kein Reasoning-Leak** mehr (Regression behoben).

### P3 — COST_DB_UPDATE TimeoutError / CancelledError (async DB)
- **Symptom** (20:39:59, 20:41:52): `COST_DB_UPDATE: Failed to save stats to database ... sock_connect ... TimeoutError` (async engine).
- **Ursache**: macvlan-/Netzwerk-Instabilität zum Carrier/DB (bekanntes Thema; async-Pfad nutzt nicht unsere `connect_with_retries`-Hardening).
- **Impact**: Kosten-Stat-Save fällt gelegentlich aus (cosmetisch, nicht blockierend für Mission).
- **Werten später**: `connect_with_retries` + Pool-Recycle auf die **async engine** ausweiten (aktuell nur sync/Startup gehärtet).

### P4 — Missing markdown file
- **Symptom** (20:40:10): `Constructed file path does not exist ... /app/data/processed/markdown/1c2ad53e-dada-4cdd-bdfe-34df2addc9ef.md`.
- **Ursache**: Ein referenziertes verarbeitetes Dokument fehlt auf der Platte.
- **Impact**: Eine Quelle kann nicht gelesen werden (ein Note geht verloren). Nicht blockierend.
- **Werten später**: Konsistenz-Check Dokumenten-Index vs. Dateien.

### Beobachtung (kein Bug)
- ReflectionAgent lief `darstellung_..._2` 2× (kein Loop, normale Revision-Pass-Logik).
