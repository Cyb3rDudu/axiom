# L8-Durchstichs-Analyse

**Berichtstyp:** Messbericht (datiert) · **Datum:** 2026-08-15 · **Kontext:**
Epic #109 Closure-Gate · **Datenbasis:** Produktions-DBs (`axiom_db`, TC2-Stand;
TC1-Backup), reproduzierbar. Original: `axiom_ng/docs/L8_DURCHSTICHS_ANALYSE.md`.

> Dieser Bericht dokumentiert den **Systemzustand zum 2026-08-15**. Die
> Zahlen sind reale Messungen; die Lehren (Transport, Fencing, GPU-Pinning)
> sind verallgemeinert in den Betriebskapiteln konserviert.

## Die gestellte Frage

> Liefert das System den horizontalen Durchstich — zuverlässig, beobachtbar, zu
> welchen Kosten?

**Antwort in einem Satz:** Ja — 16/16 Bücher fehlerfrei auf 3 heterogenen GPUs
mit 1,71× Durchsatz, verteilt per SQL belegbar, Live-Stage-Sichtbarkeit pro Buch,
zu ~6 GPU-Minuten pro Buch auf einer RTX-3090-Klasse; der Durchstich skaliert,
solange die GPUs gleich schnell sind.

## Die Messkette (alle Läufe gegen dieselbe 16-Bücher-Bibliothek)

| Lauf | Setup | Wand-Clock | Ergebnis |
| --- | --- | --- | --- |
| Benchmark (Vortag) | 1× 3090 seriell | 2.759 s (~46 min, warm) | 16/16, 4.810 Chunks — erste Voll-Extraktion |
| TC1 (L8) | 1× 3090 seriell | 72 min (12 Bücher im Endlauf) | 16/16, 0 failed nach Täterketten-Fixes |
| TC2 (L8) | 2× 3090 + A3000, 3 Dispatcher | **56 min / 16 Bücher** | 16/16, 0 failed, **0 Doppel-Processing** |

- **TC2-Verteilung** (per `ingest_jobs.runner_name`, reines SQL): gpu0 6
  Bücher/34,1 Compute-min · gpu1 7/43,2 · **A3000 3/53,0 (Ø 17,7 min/Buch)** — work-
  conserving ohne Load-Balancer: schnelle Karten nehmen mehr, `SKIP LOCKED` +
  Claim-Fencing exklusiv.
- **Skalierungsfaktor:** 1,71× Durchsatz (6,0 → 3,5 min/Buch) mit einer
  heterogenen dritten Karte; homogen projiziert 2,9× (3× 3090 ≈ 32 min). Die
  Laptop-Karte verlängerte die Wand-Clock nicht durch Rechenkraftmangel, sondern
  als Straggler-Tail (74 % busy vs. 34 % auf den 3090ern).
- **GPU-Zeit pro Buch:** 3090-Klasse ≈ 6 min Vollprofil (Marker + BGE-M3 + GLiNER +
  mREBEL), Laptop-Klasse ≈ 17,7 min. Stage-Zerlegung via `manifest.stage_timings`:
  mREBEL dominiert (~104 s/Buch), GLiNER ~34 s, Embedding ~57 s.
- **Konsistenz unter Concurrent-Writern:** Outbox 16/16 done, OpenSearch-Doc-Count
  == aktive Chunk-Anzahl.

## Datenqualität (Quality-Gate-Vorläufer) — GO

- **Chunk-Lagen:** Median 382 Token, 0 Monster-Chunks; 9,2 % Heading-Anker
  (strukturell korrekt, retrieval-wertarm — Merge-Kandidat, kein Fehler).
- **Locators:** 100 % Abdeckung (4.524 page_span + 286 CFI); Gegenprobe seiten-exakt
  gegen Original-PDFs.
- **Retrieval:** 5/5 realistische DE+EN-Queries on-topic in den richtigen Büchern
  und Abschnitten, cross-lingual; null semantischer Müll in 25 Treffern.
- **Determinismus:** 13/16 Bücher byte-identisch über unabhängige Läufe; 1 weiteres
  nach Tempdir-Leak-Fix pfad-normalisiert identisch; **2/16 Marker-Grenzfälle**
  (Tabellen-Spaltenzahl, Heading-Level — GPU-Float-Nondeterminismus im Layout-
  Modell). **Embeddings bit-exakt** (Cosinus 1.000000 über GPU-Grenzen).
- **Graph:** 26.353 Entities, 55.537 Mentions, 10.382 Relations — 100 % mit
  Evidence-Chunk; 31 % stabile Kanten.

## Die Täterkette — zwölf Fallen und ihre Behebung

Historisch ein gestaffeltes Debugging: Drei Täter maskierten sich gegenseitig;
jeder Fix legte den nächsten frei. **Heute sind sie Code, Test oder Checkliste.**

**Pipeline (stille Ausfälle):**

1. **Silent Exits im Dispatcher-Poll-Loop** — Jobs verrotteten unmarkiert. Fix:
   jede Exit-Fläche markiert Retry/Terminal + entkoppelte Renewal-Goroutine.
2. **Replay ohne Fence-Complete** — der Identity-Replay-Zweig kompletierte die
   Job-Zeile nie; die Lease lief ab. Fix sauberes Terminal-Verhalten nach ACK.
3. **force_rebuild-Doppelaktivierung** — Fix: latest-persist-wins pro Attachment +
   DB-Level-Unique-Index.
4. **OS-Index servierte abgelöste Generationen** — kein Tombstone → verwaiste Docs.
   Fix: Outbox-delete-Ops in derselben Persist-TX + Obsolete-Guards + Self-Heal.

**Performance (14–38-min-Job-Gaps):**
5. **Serielles Artifact-Staging + Shared-Timeout** — Fix: begrenzte Parallelität +
   Per-Call-Budgets; Nebeneffekt: Submit-Floor `max(30 s, resultBudget)` bewahrt
   die Remote-Source-Delivery.
6. **Transport-Decke 1: Tunnel-Bulk-Kollaps** — Fix: Direkt-LAN.
7. **Transport-Decke 2: Port-Weiterleitung im Container-Runner** — Fix:
   `--network=host` ist Pflicht; Bulk nie über den Tunnel, Tunnel nur
   Kontrollebene.
8. **GLiNER-CPU-Default** — `DEVICE_GLINER=cuda` muss explizit: ~1 h/Buch vs. 5 min.
9. **defaultProfile-Falle** — der Profilname allein schaltet nichts; Sync
   materialisiert alle Feature-Booleans explizit.

**Betrieb (TC2-Runde 1 verworfen):**
10. **dockerenv-Falle** — Container-Erkennung versagte im rootless-Runner
    → Override trampelte das GPU-Pinning nieder (alle Runner auf Karte 0, Marker-
    OOM). Fix: `RUN touch /.dockerenv` + Start-Gate (Torch-Device-Count/Name pro
    Container + Test-Allokation auf jeder Karte — der 30-s-Check, der die ganze
    Fehlrunde verhindert hätte).
11. **Migrations-Rennen** — 3 Dispatcher migrieren eine frische DB gleichzeitig →
    2 crashen am `pg_type`-Konflikt (fail-fast, Restart reicht). Lehre: Clean
    Slate → erst EINE Instanz.
12. **EPUB-Tempdir-Leak** — pandoc-`<img src="/tmp/epub_media_<random>/…">` in
    Chunk-Texten machte Re-Runs byte-verschieden. Fix: Basename-Normalisierung vor
    dem Chunking.

**Prozess-Lektionen (nicht Code):** Orphan-Binaries unterdrücken (`pkill -f
axiom-ng`, nie nur den Parent) · Jobs nie „wegballern" vor Reset · Requeue-Regel:
Zombies attempt-unverändert, nach Erschöpfung `attempt=0` · un-abgebaute
Mutations-Sonden im Worktree sind Build-Gefahren.

## Beobachtbarkeit — was der Betrieb sieht

- **Wer bearbeitet was:** `ingest_jobs.runner_name` (Claim-Zeitpunkt) + `runner=`
  in jeder Phasen-Log-Zeile → Verteilung, Durchsatz, Lastgleichheit als SQL.
- **Wo steht ein Buch:** Live-Stage über `GET /v1/jobs/{id}` + `manifest.stage_timings`.
- **Wann war der Dispatcher wo:** Phasen-Zeilen claim→submit→completed→resultFetched→
  staged→persisted→acked.
- **GPU-Zeit:** gelabelte nvidia-smi-Sampler pro Runner (30-s-Takt), eindeutig
  zuordenbar nach Log-Merge.
- **Fehler-Kommunikation:** Terminal-Codes statt 404-Hammering (`ARTIFACTS_EXPIRED`
  als Muster: der Runner ist die Wahrheitsquelle über seine Artefakte).

Details: [Monitoring](../../operations/monitoring.md).

## Geprüfte Grenzen + Anforderungskatalog fürs RAG-Retrieval-Epic

**Bewusst akzeptierte Grenzen:**

| Grenze | Quantifizierung | Konsequenz |
| --- | --- | --- |
| Laptop-Karte als Straggler | Ø 17,7 vs. 6 min/Buch; Critical Path | Heterogenität wird toleriert, beschleunigt aber nicht — Skalierung rechnet nur mit gleichen Karten |
| Marker-Nondeterminismus | 2/16 Bücher (Tabellen-/Heading-Grenzfälle) | Für Retrieval irrelevant; byte-identische Re-Runs bräuchten deterministische Torch-Algorithmen (Performanz-Preis) |
| Entity-Rauschen | 71,6 % One-Hit-Entities; generische Nomen (`companies`, `world`); Relations-Strength konstant | Graph ist Kandidatenraum: Querying MUSS filtern, nicht roh trusten |
| Sparse fehlt im OS-Index | Dense-only Retrieval bewiesen | Hybrid braucht Index-Feld + Befüllung |

**Anforderungskatalog (aus den Befunden abgeleitet):**

1. **Sparse-Embeddings in den OS-Index** (Hybrid-Retrieval).
2. **Relationship-Strength-Diskriminierung** (oder Ersatz); bis dahin
   Mention-Stabilitäts-Filter als Query-Pflicht.
3. **Entity-Nomen-Filter:** Stoplist generischer Nomen + Mindest-Mentions.
4. **(Optional)** Heading-Chunk-Merge + Literaturverzeichnis-Downweighting.

## Fazit

Die Pipeline war **mechanisch bewiesen** (zwei unabhängige Volläufe, 0 failed),
**horizontal skalierend** (1,71× heterogen gemessen, 2,9× homogen projiziert,
Exklusivität unter 3 Workern DB-erzwungen), **beobachtbar auf drei Ebenen** und
**deterministisch um den einen nichtdeterministischen Baustein** (Marker) herum.
Die Betriebsfallen sind tot (Code), gepinnt (Tests) oder als Checkliste
konserviert — die dreischichtige Täterkette kann sich in dieser Form nicht
wiederholen, ohne rot zu werden.

Weiter: [Messberichte](../benchmarks.md) · [TC2-Parallel-Test](tc2-parallel.md)
