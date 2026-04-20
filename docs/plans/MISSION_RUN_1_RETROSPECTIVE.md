# Mission Run 1 — Retrospektive (2026-04-20)

Kontext-Dokument für den nächsten Mission-Run. Erfasst Ergebnis der ersten
vollen China/DACH-VWL-Hausarbeit-Mission, identifizierte Bugs und Fixes, und
skizziert den Portfolio-Compliance-Railguard.

Mission-ID: `d9260a20-17e9-4a6f-8858-5cc213db9c2c`
Start: 2026-04-20 10:53:40 UTC — Ende: 13:43:10 UTC (≈ 50 min)
User: `dudu` (id 4), Chat: `750a7d36`, Doc-Group: `VWL` (7 Bücher)

## 1. Ergebnis in Zahlen

| Metric | Soll | Ist | Status |
|---|---|---|---|
| Wortumfang | 3.000 W ±10 % | **13.894 W** | ❌ 4,6× drüber |
| Sektionen | 5 | 5 ✓ | ✓ |
| Quellen total | 10–20 | **52** | ❌ 2,6× drüber |
| Wissenschaftl. Anteil | ≥ 50 % | **15 %** | ❌ KMU-Kriterium verfehlt |
| Blacklist-Treffer | 0 | 0 | ✓ |
| Recency-Warnungen | 0 | 3 (Quellen > 10 Jahre) | ⚠ |
| Writing-Passes | 3 | 1 (durch Auto-Optimize) | ❌ keine Revision |
| Compliance-Ampel | grün | **rot** | ❌ |

## 2. Qualität des Inhalts (unabhängig von den Zahlen)

### Positiv

- **Gliederung folgt dem Briefing 1:1** — Einleitung, 3× Theorie-Sektion, 3× Chinas-Rolle-Sektion, 3× DACH-Sektion, Fazit. Structured-Briefing-Passthrough funktioniert.
- **Theoretische Fundierung solide**: Heckscher-Ohlin, Stolper-Samuelson, Technologische Lücke, Produktlebenszyklus, Marshall-Lerner, Keynes-Multiplikator — alle für das Thema relevant und mit Seitenangaben belegt (Bofinger 2011, Mankiw 2018, Heine/Herr 2013, Eisenhut 2022, Bontrup 2004).
- **Aktualität der Daten**: China Gallium/Germanium-Exportkontrollen 2023, Dual-Circulation 2020, US-Tarife Entwicklung bis 2025, EU-EV-Zölle Juni 2024, DE-Handelsdefizit 58,4 Mrd € 2023, USA löst China Q1 2024 als wichtigster DE-Handelspartner ab. SearXNG hat deutlich besseren Index als Jina für diese Art Academic-Queries.
- **Wissenschaftliche Sprache**, roter Faden pro Sektion, Gegenpositionen präsent (Minsky zur DCS, keynesianisch vs. neoklassisch, Kritik an Verschuldungs-mit-Verschuldung bei Immobilienkrise-Gegenmaßnahmen).
- **Fachbegriffe definiert** (Hukou, Dual Circulation, Derisking, Glokalisierung, Friendshoring).

### Negativ

- **Überlänge ist fatal für 5 ECTS Hausarbeit** — keine Pass-1-Revision hat's gekürzt. Auto-Optimize hat `writing_passes: 3 → 1` gesetzt.
- **Zitier-Mix**: saubere APA-7-Zitate aus den Büchern neben rohen URL-Klammer-Zitaten wie `(https://www.bpb.de/...o. S.)` oder `(Branko Milanović über den Aufstieg Chinas und das Ende der liberalen Ordnung | FAZ, o. S.)`. Root-Cause: `writing_agent.py:449` nutzt bei fehlenden `authors`-Metadaten den **Artikel-Titel** oder die **URL** als „Autor"-Feld. `quick_enrich_for_writing` wird nur für Documents, nicht für Web-Quellen aufgerufen.
- **Zu viele Web-Quellen ohne Kuratierung** — Research-Agent hat 52 Quellen gesammelt, darunter `finanzkun.de`, `fibu-magazin.de`, `martinkaessler.com` (Privatblogs), Tagesschau, msn.com — alles in der „nicht-wissenschaftlichen" Hälfte.
- **Local-RAG-Bücher wurden als Tier `unknown` fehl-klassifiziert** statt Tier A, weil `documents.metadata` keine `publisher`-Felder hat und mein `classify_tier` nur auf Publisher/URL/Journal/DOI matchte. 7 Bücher mit je 500–3000 Chunks → 7 potenzielle Tier-A-Zitate, aber sie zählten in der Compliance-Berechnung als Tier C → 15 %-Quote.
- **Portfolio-Bullets komplett leer** — alle 52 Einträge zeigten Fallback-Text `"Konkreter Beitrag bitte manuell ergänzen"`. Agent-LLM-Response war 31.733 chars (DeepSeek `max_tokens=8192` Limit erreicht, String mid-content abgeschnitten, JSON-Parse schlug fehl).
- **Auto-Optimize hat benannte Settings überschrieben** trotz Structured-Briefing:
  ```
  writing_passes: 3 → 1        # fatal — kein Revision-Pass
  initial_research_max_questions: 10 → 5
  main_research_doc_results: 7 → 3
  max_total_depth: 2 → 1
  ```

## 3. Identifizierte Bugs + Fixes

### Bug 1: Portfolio-Tier für local-RAG-Docs → Tier A-Default

**Datei:** `axiom_backend/ai_researcher/agentic_layer/services/source_quality.py`

**Fix:**
- `compute_quality_signals()` bekommt für `source_type='document'` bei `tier='unknown'` einen Tier-A-Fallback.
- Tier-A-Liste in `publisher_tiers.py` erweitert: Springer Gabler/VS, Schäffer-Poeschel, Vahlen, C.H.Beck, Mohr Siebeck, Duncker & Humblot, Campus, UTB.
- Tier-B-Liste erweitert: BPB, Bundesbank, IfW Kiel, WSI Düsseldorf, KfW Research, SWP, DGAP, GIGA, Böll/Heinrich-Böll, BMBF, DFG.
- Blacklist erweitert: bwl-lexikon.de, wirtschaftslexikon24.com, scribbr.de, fibu-magazin.de, finanzkun.de, martinkaessler.com (KMU Dos-and-Don'ts).
- Neue Tests in `tests/agentic_layer/test_source_quality.py` (local-RAG-Fallback, BPB Tier B, Lexikon-Blacklist).

**Status:** ✅ implementiert lokal, nicht deployed (User: "wait with deployment until I finished the book import").

### Bug 2: Portfolio-Bullet-Parsing brach bei > ~25 Quellen

**Datei:** `axiom_backend/ai_researcher/agentic_layer/agents/literature_portfolio_agent.py`

**Fix-Strategie:**
1. **Schlankere Agent-Output-Schema.** Agent gibt nur noch die 4 generierten Felder zurück (`source_id`, `relevance_bullets`, `quality_bullets`, `contribution_type`, `sections_used_in`) — **kein Echo** von `apa_citation`, `discovery_tool`, `quality_signals`, `scientific_tier` (Manager reattached diese).
2. **Batching**: bei > 20 Quellen wird in Batches von 20 aufgeteilt, pro Batch ein eigener LLM-Call. Merged danach.
3. **`max_tokens=8192`** expliziert pro Call (war vorher LLM-Default).
4. Neuer `_SlimEntriesEnvelope` Validator ersetzt den strikten `PortfolioEntry`-Validator.

**Erwartete Wirkung:** pro Batch ~1-2k tokens output (statt 8k+ bei 52 Quellen monolithisch), kein Truncation mehr, saubere JSON.

**Status:** ✅ implementiert lokal, nicht deployed.

### Bug 3: Fehlender DELETE-Endpoint für Missions

**Datei:** `axiom_backend/api/missions.py`

Es existierte `async_crud.delete_mission()` + `delete_mission_execution_logs()`, aber **nie** ein API-Endpoint. User konnte einzelne Missions nicht löschen (Chat-Delete cascaded stattdessen alle Missions).

**Fix:** Neuer Endpoint `DELETE /api/missions/{mission_id}` mit Ownership-Check via `async_crud.get_mission(db, mission_id, user_id)`, Pre-Delete `pause_mission()` falls running, dann Logs löschen → Mission-Row löschen → In-Memory-Cache-Eviction.

**Status:** ✅ implementiert lokal, nicht deployed.

### Bug 4: Zitier-Mix bei Web-Quellen (offen)

**Datei:** `axiom_backend/ai_researcher/agentic_layer/agents/writing_agent.py` Zeile 449

**Root-Cause:**
```python
web_author = title if title and title != "Unknown Title" else url
cite_example = f"({web_author}, {no_page_abbr})"
```
Fehlt: Domain-/Institutions-Fallback bevor auf URL zurückgefallen wird. Plus `quick_enrich_for_writing` läuft nicht für Web-Quellen (nur für Documents).

**Vorgeschlagener Fix (nicht umgesetzt):**
1. Domain extrahieren (`www.bpb.de` → „BPB"), Institution aus curatierter Map (`faz.net` → „FAZ", `diw.de` → „DIW Berlin", `iwkoeln.de` → „IW Köln", `bundesbank.de` → „Deutsche Bundesbank", …).
2. Falls keine Map-Treffer: Domain als Kurzform des „Autors" (`bpb.de` statt `https://www.bpb.de/themen/.../abschnitt/`).
3. `quick_enrich_for_writing` auch für Web-Quellen aufrufen — Open-Graph-Tags, `<meta name="author">`, und CrossRef falls DOI im HTML vorhanden.
4. Writer-Prompt härten: *„Verwende niemals rohe URLs innerhalb von Klammern. Wenn weder Autor noch Institution identifizierbar sind, nutze den Sitenamen (z. B. 'BPB' statt 'https://www.bpb.de/…')."*

**Status:** Analysiert, Fix pending. Als separate Task aufnehmen.

## 4. Empfohlene Mission-Konfiguration für Run 2

```
mission_settings:
  auto_optimize_params: false       # KRITISCH — nicht deine Zielwerte überschreiben
  writing_passes: 3                 # sichert Revision + Kürzung
  initial_research_max_depth: 1
  initial_research_max_questions: 10
  structured_research_rounds: 3
  initial_exploration_doc_results: 7
  initial_exploration_web_results: 3
  main_research_doc_results: 7
  main_research_web_results: 10
  max_notes_for_assignment_reranking: 80
  max_concurrent_requests: 5
  citation_profile_id: kmu_apa6
  deliverables:
    literature_portfolio: true

search_provider: searxng            # NICHT jina (kaum Academic-Index)
language_code: de
```

**Briefing-Zusatz für Run 2:**
```
## Harte Constraints
- Maximal 3.000 Wörter (±10 %) — pro Sektion ca. 500 Wörter.
- Wissenschaftliche Quellen priorisieren: mindestens 50 % peer-reviewte Zeitschriften, wissenschaftliche Monographien oder Working Papers anerkannter Institute (IMF, World Bank, OECD, ifo, DIW, IW Köln, IfW Kiel, Bundesbank, BPB, Springer/Wiley/Elsevier, SSRN, RePEc).
- Keine Lexikon-Einträge (Gabler, BWL-Lexikon, Wirtschaftslexikon24), keine Blogs (fibu-magazin.de, finanzkun.de, martinkaessler.com), keine Scribbr-Artikel.
- Zitierstil APA 7 mit Seitenzahlen bei Büchern; für Online-Quellen: Autor, Jahr, o.S. — wenn kein Autor vorhanden, die Institution/den Sitenamen (z. B. „BPB" oder „DIW Berlin"), niemals rohe URLs.
```

## 5. Portfolio-Compliance als Mission-Railguard (Design)

**Problem:** heute läuft die Portfolio-Compliance **post-hoc** — erst nach Writing wird geprüft ob die Quellen-Mischung stimmt. Bei roter Ampel ist es zu spät: der Report ist schon geschrieben, Kürzen oder Neu-Zitieren scheidet aus.

**Ziel:** Compliance-Checks während der Mission aktiv einwirken lassen — Quellen ablehnen, mehr Research-Rounds triggern, Writing-Prompts anpassen.

### Einbaupunkte (von „leicht" nach „invasiv")

#### 5.1 Blacklist-Gate in `document_search_tool` + `web_search_tool` (leicht)

**Ort:** `tools/web_search_tool.py` + `tools/document_search.py` in der Result-Post-Processing.

**Was:** Nach jedem Search-Call durch `classify_tier(url/domain/publisher)` laufen lassen. Ist das Result `tier == "blacklist"`, komplett verwerfen (nicht in Notes aufnehmen). Pro Filter-Hit ein Execution-Log-Eintrag `Agent=Railguard, action=drop blacklist source`.

**Wirkung:** Wikipedia, BWL-Lexikon, Scribbr etc. kommen **gar nicht erst** in den Research-Notes-Pool. Sofortiger Compliance-Gewinn.

**Risk:** null — Blacklist ist heute schon kuratiert, und wir kennen die Treffer.

#### 5.2 Scientific-Share-Monitor pro Research-Round (mittel)

**Ort:** Ende jeder Runde in `research_manager._run_research_rounds()`.

**Was:** Nach jeder Round:
1. Aktuelle Notes holen, `compute_quality_signals` + `assign_scientific_tier` pro Quelle.
2. Share tier∈{A,B} berechnen.
3. Wenn Share < 40 % und es ist nicht die letzte Runde: einen zusätzlichen Goal-Pad-Eintrag einfügen:
   *„SCIENTIFIC-SHARE-WARNING: aktuell X % wissenschaftliche Quellen — in der nächsten Runde aktiv peer-reviewte Journale und Monographien wissenschaftlicher Verlage priorisieren. Quellen wie X, Y, Z sind bevorzugt."*
4. QueryPreparer erweitert dann Queries mit `site:springer.com OR site:wiley.com OR site:ssrn.com OR site:repec.org`.

**Wirkung:** mittendrin korrigierbar, ohne Research komplett neu zu starten.

**Risk:** mittel — könnte Queries zu eng machen und 0 Treffer liefern. Fallback: nach 2 Rounds die Constraint lockern wenn keine Treffer.

#### 5.3 Tier-A/B-Bias im Note-Assignment-Reranker (mittel)

**Ort:** `note_assignment_agent.py` — der Reranker vor Writing, der die Top-80 Notes auswählt.

**Was:** Scoring-Gewicht pro Note erhöhen um einen Tier-Bonus:
```
score += 0.15 if scientific_tier == "A"
score += 0.08 if scientific_tier == "B"
score -= 0.10 if publisher_tier == "blacklist"  # extra Versicherung
```

**Wirkung:** wenn der Mission ähnlich gewichtete Web-Quellen und Book-Chunks vorliegen, priorisiert der Writer die Buch-Chunks.

**Risk:** niedrig — kleine Score-Adjustments, leicht rückwärtskompatibel.

#### 5.4 Pre-Writing-Gate (invasiv, optional)

**Ort:** Zwischen Planning und Writing.

**Was:** Prüfe `scientific_share` über die ausgewählten Notes. Falls < 50 %:
- Option A: Mission pausieren und User fragen, ob weitere Research-Rounds gewünscht.
- Option B: Automatisch eine Research-Round nachlegen (mit Scientific-Bias).
- Option C: Warnung in den ersten Abschnitt des Reports schreiben.

**Wirkung:** keine red-Ampeln mehr bei Abschluss.

**Risk:** hoch — User-Interaktion mittendrin; oder Extra-Round-Kosten.

### Empfehlung

**Stufe 1 (jetzt bauen):** Blacklist-Gate in Search-Tools (5.1). Minimal invasive, hoher Gewinn.

**Stufe 2 (nach Run 2 evaluieren):** Scientific-Share-Monitor (5.2) + Note-Reranker-Bias (5.3) kombiniert. Erst messen ob 5.1 alleine schon die 50 %-Schwelle schafft.

**Stufe 3 (nur wenn nötig):** Pre-Writing-Gate (5.4). Nur wenn nach 5.1+5.2 immer noch < 50 % rauskommt.

Als Feature-Folge-Task aufgenommen: *„Portfolio Railguard — Stufe 1 (Blacklist-Gate)"*.

## 6. Offene Punkte für Run 2

1. **User importiert zusätzliche Bücher** in die VWL-Library (bereits im Gange laut User). Deploy erst danach.
2. **Auto-Optimize-Toggle im UI muss OFF** — sonst wird jede manuelle Einstellung ignoriert.
3. **Harte Wort-Cap-Instruktion** im Briefing selbst platzieren (siehe § 4).
4. **Zitier-Bug (Bug 4)** bleibt für Run 2 bestehen — muss manuell noch nach-redigiert werden oder in einem separaten Folge-Patch gefixt werden.
5. **Compare-Delta:** nach Run 2 diese Doc um eine Sektion „Run 2 Results" erweitern mit Gegenüberstellung.
