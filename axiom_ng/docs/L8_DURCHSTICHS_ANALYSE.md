# L8 Durchstichs-Analyse — Epic #109 Abschluss

**Datum:** 2026-08-15 · **Issue:** #117 (Epic-Closure-Gate) · **Autoren:** Implementor (Messung/Text), Hivemind (Verifikation), dudu (Betrieb + Go-Entscheidungen)
**Zweck:** Dieses Dokument beantwortet die Epic-Frage in einem lesbaren Durchgang und konserviert die
Wahrheit über das System für ein Jahr später. Alle Zahlen sind gegen die Produktions-DBs
`axiom_db` (TC2-Stand) und `axiom_db_tc1_ref` (TC1-Backup) reproduzierbar; die Beweisketten
liegen in MASS_CHUNKING_BENCHMARK.md, TC2_PARALLEL_BENCHMARK.md, CHUNK_QUALITY_ASSESSMENT.md
sowie den Issue-Kommentaren #109–#127.

---

## 1. Die Epic-Frage

> Liefert das System den horizontalen Durchstich — zuverlässig, beobachtbar, zu welchen Kosten?

**Antwort in einem Satz:** Ja — 16/16 Bücher fehlerfrei auf 3 heterogenen GPUs mit 1,71×
Durchsatz, verteilt per SQL belegbar, Live-Stage-Sichtbarkeit pro Buch, zu ~8 GPU-Minuten
pro Buch auf einer RTX 3090; der Durchstich skaliert solange die GPUs gleich schnell sind,
und jede Betriebsfläche dieses Ziels ist jetzt Code, Test oder Checkliste.

## 2. Die Messkette (alle Läufe gegen dieselbe 16-Bücher-Bibliothek)

| Lauf | Setup | Wand-Clock | Ergebnis |
| --- | --- | --- | --- |
| Benchmark (Vortag) | 1× 3090 seriell | 2.759 s (~46 min, warm) | 16/16, 4.810 Chunks — erste Voll-Extraktion |
| TC1 (L8) | 1× 3090 seriell | 72 min (12 Bücher im Endlauf) | 16/16, 0 failed nach Täterketten-Fixes |
| TC2 (L8) | 2× 3090 + A3000, 3 Dispatcher | **56 min / 16 Bücher** | 16/16, 0 failed, **0 Doppel-Processing** |

- **TC2-Verteilung** (per `ingest_jobs.runner_name`, reines SQL): gpu0 6 Bücher/34,1
  Compute-min · gpu1 7/43,2 · **A3000 3/53,0 (Ø 17,7 min/Buch)** — work-conserving ohne
  Load-Balancer: die schnellen Karten nehmen mehr, SKIP LOCKED + Claim-Fencing exklusiv.
- **Skalierungsfaktor:** 1,71× Durchsatz (6,0 → 3,5 min/Buch) bei heterogener dritter Karte;
  homogen projiziert 2,9× (3× 3090 ≈ 32 min). Die A3000 verlängerte die Wand-Clock nicht
  durch Rechenkraftmangel, sondern als Straggler-Tail (74 % busy vs 34 % auf den 3090ern —
  sie war der Critical Path).
- **GPU-Zeit pro Buch:** 3090 ≈ 6 min Vollprofil (Marker + BGE-M3 + GLiNER + mREBEL),
  A3000 ≈ 17,7 min. Stage-Zerlegung via `manifest.stage_timings`: mREBEL dominiert
  (~104 s/Buch), GLiNER ~34 s, Embedding ~57 s — der Retrieval-Ausbau beginnt also beim
  Relationsextraktor, falls je nötig.
- **Konsistenz unter Concurrent-Writern:** Outbox 16/16 done, OpenSearch-Doc-Count ==
  aktive Chunk-Anzahl (seit #127 auch bei force_rebuild durch Tombstones garantiert).

## 3. Datenqualität (Quality Gate, #120-Vorläufer) — GO

- **Chunk-Lagen:** Median 382 Token, 0 Monster-Chunks; 9,2 % Heading-Anker (strukturell
  korrekt, retrieval-wertarm — Merge-Kandidat, kein Fehler).
- **Locators:** 100 % Abdeckung (4.524 page_span + 286 CFI); Gegenprobe 3/3 **seiten-exakt**
  gegen Original-PDFs.
- **Retrieval (der Härtest):** 5/5 realistische DE+EN-Queries landeten on-topic in den
  richtigen Büchern UND Abschnitten, cross-lingual; null semantischer Müll in 25 Treffern.
- **Determinismus:** 13/16 Bücher byte-identisch über unabhängige Läufe; 1 further
  (Sonko) nach Tempdir-Leak-Fix pfad-normalisiert identisch; **2/16 Marker-Grenzfälle**
  (Tabellen-Spaltenzahl, Heading-Level — GPU-Float-Nondeterminismus im Layout-Modell).
  **Embeddings bit-exakt** (Cosinus 1.000000 über GPU-Grenzen) — alles außer Marker ist
  deterministisch.
- **Graph:** 26.353 Entities, 55.537 Mentions, 10.382 Relations — 100 % mit Evidence-Chunk;
  31 % stabile Kanten (beide Enden >1 Mention); siehe §6 (Querying-Filter Pflicht).

## 4. Die Täterkette — zwölf Fallen und wie sie starben

Die L8-Geschichte ist eine gestaffelte Debugging-Kette: drei Täter maskierten sich
gegenseitig; jeder Fix legte den nächsten frei. In einem Jahr wird das die wertvollste
Sektion sein.

**Pipeline-Täter (stille Ausfälle):**

1. **Silent Exits im Dispatcher-Poll-Loop** — Jobs verrotteten unmarkiert (`processing`
   ohneWorker). Fix: jede Exit-Fläche markiert Retry/Terminal + entkoppelte
   Renewal-Goroutine über die gesamte Job-Lebensdauer (`05f7ddc`, `f55c8de`).
2. **Replay ohne Fence-Complete** — der Identity-Replay-Zweig von PersistResult
   kompletierte die Job-Zeile nie; die Lease lief ab, der Re-Claim resubmittierte auf
   einen geACKten Runner (erst 404-Wand, dann sauber `ARTIFACTS_EXPIRED`). Diese Kante
   klärte rückwirkend auch das #126-Rätsel (`befa516`).
3. **force_rebuild-Doppelaktivierung** — Deaktivierung war per profile_hash gescoped,
   der Force-Flag ändert ihn → zwei aktive Generationen (zählte ESGBS doppelt). Fix:
   latest-persist-wins pro Attachment + DB-Level-Unique-Index 0011 (`a63b5eb`).
4. **OS-Index served abgelöste Generationen** — kein Tombstone → 252 verwaiste Docs nach
   Force-Rebuild. Fix: Outbox-delete-Ops in derselben Persist-TX + Obsolete-Guards +
   Self-Heal (`2fe453e`, `1d4dc25`).

**Performance-Täter (14–38 min Job-Gaps):**
5. **Serielles Artifact-Staging + Shared-Timeout** — 6er-Parallelität + Per-Call-Budgets
   (`f3b00fb`+`6fc17a7`); Nebeneffekt: der Submit-Floor `max(30 s, resultBudget)` bewahrt
   die Remote-Source-Delivery.
6. **Transport-Decke 1: Tailscale utun10** — Bulk-Collapse ~123 KB/s bei ms-Polls →
   Direkt-LAN.
7. **Transport-Decke 2: Podman-passt-Port-Mapping** — dieselbe Signatur (Loopback im
   Container 122 MB/s, gemappter Port 123 KB/s) → `--network=host` ist Pflicht; Bulk
   NIE über Tunnel, Tailscale nur Control-Plane.
8. **GLiNER-CPU-Default** — `DEVICE_GLINER=cuda` muss explizit: ~1 h/Buch vs 5 min.
9. **defaultProfile-Falle** — Profilname allein schaltet nichts; Sync materialisiert
   jetzt alle Feature-Booleans explizit (`9aaad69`).

**Betriebs-Täter (TC2-Runde 1 verworfen):**
10. **dockerenv-Falle** — `is_running_in_docker()` versagt in rootless Podman (kein
    `/.dockerenv`, cgroup v2 nur `0::/`) → Config trampelte `CUDA_VISIBLE_DEVICES=0` über
    das Container-Pinning: alle 3 Runner auf GPU 0, ein Marker-OOM. Fix:
    `RUN touch /.dockerenv` im Image + **Start-Gate** (Torch-Device-Count/Name pro
    Container + Test-Allokation auf JEDER Karte — der 30-Sekunden-Check, der die ganze
    Fehlrunde verhindert hätte). Mit #118 ist die Override-Logik selbst tot.
11. **Migrations-Rennen** — 3 Dispatcher migrieren eine frische DB gleichzeitig → 2
    crashen an `pg_type` (fail-fast, Restart reicht). Lehre: Clean Slate → EINE Instanz
    zuerst.
12. **EPUB-Tempdir-Leak** — pandoc-`<img src="/tmp/epub_media_<random>/…">` in
    Chunk-Texten machte jeden Re-Run byte-verschieden (Sonko, 27/252 Chunks). Fix:
    Basename-Normalisierung vor dem Chunking (`a65be86`).

**Prozess-Lektionen (nicht Code):** `go run` hinterlässt Orphan-Binaries (`pkill -f
axiom-ng`, nie nur den Parent killen) · Jobs NIE „wegballern" vor Reset · Requeue-Regel:
Zombies attempt-unverändert, nach Erschöpfung `attempt=0` · un-rückgebaute
Mutations-Sonden im Worktree sind Build-Gefahren (rsync baut aus dem Worktree).

## 5. Beobachtbarkeit — was der Betrieb heute sieht

- **Wer bearbeitet was:** `ingest_jobs.runner_name` (Claim-Zeitpunkt) + `runner=` in
  jeder Phasen-Log-Zeile → Verteilung, Durchsatz, Lastgleichheit als SQL.
- **Wo steht ein Buch:** Live-Stage über `GET /v1/jobs/{id}` (validate_source → … →
  assemble) + `manifest.stage_timings` (nachträgliche Phasen-Rekonstruktion ohne
  Live-Beobachtung).
- **Wann war der Dispatcher wo:** Phasen-Zeilen claim→submit→completed→resultFetched→
  staged→persisted→acked; Job-Gaps 0 s–Poll-Intervall.
- **GPU-Zeit:** gelabelte nvidia-smi-Sampler pro Runner (30 s-Takt), eindeutig
  zuordenbar nach Log-Merge.
- **Fehler-Kommunikation:** Terminal-Codes statt 404-Hammering (`ARTIFACTS_EXPIRED` als
  Muster: Runner ist die Wahrheitsquelle über seine Artefakte).

## 6. Geprüfte Grenzen + Anforderungskatalog fürs RAG-Retrieval-Epic

**Bekannte, bewusst akzeptierte Grenzen:**

| Grenze | Quantifizierung | Konsequenz |
| --- | --- | --- |
| A3000-Straggler | Ø 17,7 vs 6 min/Buch; Critical Path in TC2 | Heterogenität wird toleriert, beschleunigt aber nichts — Skalierung rechnet nur mit gleichen Karten |
| Marker-Nondeterminismus | 2/16 Bücher (Tabellen-/Heading-Grenzfälle) | Für Retrieval irrelevant; byte-identische Re-Runs bräuchten deterministische Torch-Algorithmen (Performance-Preis — Entscheidung offen) |
| Entity-Rauschen | 71,6 % One-Hit-Entities; generische Nomen (`companies`, `world`); Relations-Strength konstant 0,7 | Graph ist Kandidatenraum: Querying MUSS filtern, nicht roh trusten |
| Sparse fehlt im OS-Index | Dense-only Retrieval bewiesen | Hybrid braucht Index-Feld + Befüllung |

**Geprüfter Anforderungskatalog (aus den Quality-Gate-Befunden abgeleitet, jeweils mit
Akzeptanzkriterium):**

1. **Sparse-Embeddings in den OS-Index** (Hybrid-Retrieval): Feld + Outbox-Befüllung +
   Query-Merge; Akzeptanz: knn- und sparse-Ergebnisse in einem Request vereinbart.
2. **Relationship-Strength-Diskriminierung** (oder Ersatz): mREBEL liefert keine
   Konfidenz — entweder Modell-seitig ableiten oder Evidence-basiert stärken; bis dahin
   **Mention-Stabilitäts-Filter** (beide Enden ≥2 Mentions ≈ stabile 31 %) als
   Query-Pflicht; Akzeptanz: Graph-Query ohne One-Hit-Rauschkanten.
3. **Entity-Nomen-Filter:** Stoplist generischer Nomen + Mindest-Anforderung
   (Kontext-Mentions) beim Entity-Onboarding; Akzeptanz: Top-Entity-Liste ohne
   `companies`/`world`.
4. **(Optional) Heading-Chunk-Merge + Literaturverzeichnis-Downweighting:**
   Retrieval-Ausbeute pro Index-Doc, kein Korrektheits-Thema.

## 7. Fazit

Die Pipeline ist **mechanisch bewiesen** (zwei unabhängige Volläufe, 0 failed),
**horizontal skalierend** (1,71× heterogen gemessen, 2,9× homogen projiziert, Exklusivität
unter 3 Workern DB-erzwungen), **beobachtbar auf drei Ebenen** (SQL/Stage/Phasen-Log) und
**deterministisch um den einen nichtdeterministischen Baustein** (Marker) herum, dessen
Restrischen quantifiziert sind. Die Datenqualität trägt Retrieval (5/5-Smoke, Locators
seiten-exakt, Embeddings bit-reproduzierbar). Die Betriebsfallen sind entweder tot (Code),
gepint (Tests) oder als Checkliste konserviert (Deployment-Doc) — die dreischichtige
Täterkette, die L8 kostete, kann sich in dieser Form nicht wiederholen, ohne rot zu werden.

**Was als Nächstes gilt, steht in §6 — nicht mehr in diesem Epic.**

— *Ende von L8 / Epic #109. Nächster Schritt: Closure + Archive-Branch.*
