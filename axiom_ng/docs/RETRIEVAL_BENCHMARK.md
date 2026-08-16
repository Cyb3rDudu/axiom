# Retrieval Benchmark (R7, #137)

Gold suite: 25 Queries (DE+EN; Konzept/Fakt/Norm/Autor), **25/25 von dudu bestätigt (2026-08-16, „alles grün“)** (implementor-abgeleitet aus den 16 Buchtiteln der Bibliothek; die 5 Quality-Assessment-Queries sind der Startbestand — die Herleitung bleibt als Kontext, der PROVISORISCH-Vermerk ist seit Schritt 0 überholt). Suite: `cmd/retrieval-bench/gold_suite.json` (`confirmed`-Flag pro Query).

Lauf: 2026-08-16, lokal (Mac, MPS fp32), echter OS-Index (4.813 Chunks), echter Query-Runner (R1/R2 warm), echte DB. Reproduzierbar:
`AXIOM_TEST_DATABASE_URL=… AXIOM_PROCESSOR_URL=http://127.0.0.1:8012 go run ./cmd/retrieval-bench -md out.md`

| Konfiguration | P@5 | MRR | R@10 | p50 | p95 | Fehler |
|---|---|---|---|---|---|---|
| dense-only | **0.680** | **0.865** | 0.967 | 71 ms | 85 ms | 0 |
| hybrid (dense+bm25) | 0.608 | 0.815 | **0.987** | 69 ms | 90 ms | 0 |
| hybrid+rerank | 0.616 | 0.818 | 0.967 | 6.38 s | 7.18 s | 0 |
| hybrid+rerank+sparse | 0.608 | 0.791 | 0.947 | 7.38 s | 8.49 s | 0 |
| hybrid+rerank+sparse+graph | 0.600 | 0.790 | 0.927 | 9.16 s | 9.50 s | 0 |

(Zweiter Lauf bestätigt die Metriken auf ±0.001; Latenzen streuen mit Modell-Wärmezustand — Rerank p50 3.9–6.4 s über Läufe.)

Definitionen: P@5 = Gold-Buch-Treffer / Top-5 (teilt per Query durch min(5, gelieferte Treffer) — relevant nur, wenn eine Konfiguration <5 Treffer liefert; keine tat es im gepinnten Lauf); MRR = 1/Rang des ersten Gold-Treffers; R@10 = gefundene Gold-Bücher / |Gold|. top_n=10, Overfetch 3×=30 Rerank-Kandidaten.

## Lesart

1. **Reranker-These (Ziel 3) — auf der PROVISORISCHEN Suite NICHT bestätigt für Präzision:** dense-only schlägt hybrid+rerank bei P@5 (0.680 vs. 0.616) und MRR (0.865 vs. 0.818). Rerank über Hybrid bringt marginal + (P@5 +0.008, MRR +0.003) — der alte Beweis „Hybrid+Rerank schlägt deutlich" reproduziert sich SO nicht. **Wichtigster Caveat:** Die Gold-Menge ist titel-abgeleitet — Queries, deren Gold praktisch der Buchtitel ist (v.a. Autor-/Norm-Queries), begünstigen exakte Titel-Signale. dudus Bestätigung/Korrektur der Gold-Annotationen kann das Bild drehen; die Suite ist dafür gebaut.
2. **Recall-Story hält:** hybrid R@10 0.987 vs. dense-only 0.967 — der Grund, warum Hybrid existiert (BM25 fängt ab, was Dense verpasst).
3. **Sparse-Arm (R5): kein Gewinn, hohe Kosten.** MRR −0.027 vs. hybrid+rerank, R@10 −0.020; dafür +~1.3 s p95 (7.18→8.49 s im gepinnten Lauf) — die 64-Clauses-`rank_feature`-bool-should ist auf diesem Index teuer. Erwartetes Einsatzprofil bleibt rare Tokens (Normnummern/Akronyme über Sprachgrenzen) — dafür gibt es in der Suite bisher zu wenige solcher Queries (n18–n21 messen es nicht isoliert).
4. **Graph-Arm (R6): leicht schädlich + teuer.** MRR −0.001, R@10 −0.020, +1.0 s p95 (8.49→9.50) bzw. +1.8 s p50 (7.38→9.16; die GraphCandidates-SQL über 55k Mentions ist ungetunt). Default-OFF bestätigt.
5. **Latenz-Budget (top_n=10):** rerank kostet lokal ~4–6.4 s p50 über die Läufe (30 Kandidaten × ~128–213 ms/Paar MPS fp32, warm-variiert — der ~130-ms-Satz erklärt nur das untere Ende). Für das 2-s-Budget: Remote-Runner (CUDA, R4-gemessen 0.95 s p95 bei top_n=5), Overfetch 2× (~20 Kandidaten ≈ −33 %), oder top_n=5. Der R3-SLA gilt für top_n=5.

## Empfohlene Produktions-Defaults (umgesetzt)

| Hebel | Wert | Begründung |
|---|---|---|
| Arme | dense+bm25 (hybrid) | beste R@10, Latenz <100 ms |
| Rerank | **an** (`AXIOM_SEARCH_RERANK`) | marginal aber konsistent + über Hybrid; Latenz via Remote-Runner/Overfetch steuerbar |
| Sparse | **aus** (`AXIOM_SEARCH_SPARSE_ARM`, umgestellt mit diesem Benchmark) | kein Qualitäts­gewinn, +~1.3 s p95 (7.18→8.49); Hebel für rare-Token-Profile nach Tuning (sparseTopK, Shards) |
| Graph | aus (`AXIOM_SEARCH_GRAPH_ARM`) | leicht negativ, Expansion ungetunt |
| rrfK | 60 (Standard) | nicht variiert — Candidates-Pool-Größe dominanter; Tuning erst nach Gold-Bestätigung sinnvoll |
| Overfetch | 3× bei top_n≤5; 2× erwägen bei top_n=10 lokal | Latenz-Hebel, Qualitäts­differenz <0.01 (nicht separat gemessen — nach Gold-Bestätigung) |

Defaults sind in R3 gesetzt (`config.go`), Sparse-Default mit diesem Benchmark von an→aus gedreht (Fähigkeits-Tests beweisen den Arm unabhängig davon weiter).

## Schritt 0 (2026-08-16)

dudu hat alle 25 Gold-Einträge bestätigt („alles grün“; `confirmed`-Flip ohne Annotation — Schritt 0 von #155). Neulauf der Matrix: **Metriken byte-identisch zum gepinnten Lauf** (alle fünf Konfigurationen unverändert; Latenzen in der bekannten Warm-Streuung). Die Lesart-Punkte 1–5 gelten unverändert. V2-Hinweis für die kommende Passagen-Matrix: der Graph-Arm ignoriert den Dokument-Scope (ungetunte Expansion ist produktionsgetreu) — die Graph-Zeile misst dort Scope-Pollution, nicht In-Scope-Graphqualität.

## Offen

- Rare-Token-Sub-Suite (Normnummern/Akronyme) für das Sparse-Profil.
- GraphCandidates-SQL-Tuning, falls der Arm produktiv werden soll.

## v2 — Scoped Gold, Passagen-Ebene (#155, 2026-08-16)

Suite: `gold_suite_v2.json` — **32 Queries, alle gescopet** (filters.document_ids): 25
dudu-entschiedene Proposals (21× yes, 3× alt:0) + **7 verified** Einträge (z1–z7, aus
ESG_Quellen_und_Zitatnotizen: menschen-verifizierte Stellen, Gold-Chunk per Suchanker-SQL
aufgelöst). Status: **CONFIRMED — kein Titel-Zirkel, kein Maschinen-Schätzgold.**
Metriken: P@1/hit@5/MRR/hit@10 auf Chunk-Ebene. 2 Läufe: **identisch auf 3 Dezimalstellen**
(deterministisch).

| Konfiguration | P@1 | hit@5 | MRR | hit@10 | p50 | p95 |
|---|---|---|---|---|---|---|
| dense-only | 0.188 | 0.656 | 0.390 | 0.844 | 73 ms | 102 ms |
| hybrid | 0.219 | 0.781 | 0.461 | 0.875 | 66 ms | 86 ms |
| **hybrid+rerank** | **0.750** | **0.938** | **0.842** | **0.969** | 6.3–6.9 s | 6.3–7.3 s |
| +sparse | 0.750 | 0.938 | 0.834 | 0.969 | 6.2 s | 6.3–7.1 s |
| +sparse+graph | 0.719 | 0.938 | 0.818 | 0.969 | 6.2 s | 6.4 s |

### Reranker-Urteil (Passagen-Ebene, Realfall) — **JA, deutlich**

- **P@1 verdreifacht** (0.219 → 0.750 vs. hybrid; 0.188 → 0.750 vs. dense-only), MRR
  verdoppelt (0.461 → 0.842), hit@10 0.969. Die v1-Überraschung (dense-only vorn) war
  tatsächlich das Titel-Zirkel-Artefakt — am echten Arbeitsfluss (Buch vorgegeben, beste
  Stelle finden) gewinnt der Cross-Encoder exakt dort, wo seine Kernaufgabe liegt:
  Chunk-Ranking im bekannten Buch.
- **Verified-Teilmenge (z1–z7, n=7, härteste Messlatte):** P@1 0.571 / MRR 0.659 mit
  Rerank (vs. 0.429/0.583 dense, 0.286/0.548 hybrid) — aber hit@5 0.714 vs. 0.857: ein
  Fall, wo Rerank den Gold-Chunk aus den Top-5 drängte. Kleines n, ehrlich notiert.
- Sparse: im Scope gleichauf (leicht MRR-drunter) — Default OFF bleibt. Graph: minimal
  drunter (und im Scope prinzipbedingt ungetunt/unscoped) — Default OFF bleibt.
- **Latenz:** Rerank kostet lokal (MPS) 6–7 s bei top_n=10/Overfetch 3× (30 Kandidaten à
  ~210 ms). Hebel: 2× Overfetch (~4 s), Remote-CUDA (R4-Referenz: 0.95 s p95 bei
  top_n=5), top_n=5. Für dudus Agenten-Fluss (zitiert Stellen): **hybrid+rerank,
  top_n 5–10, Overfetch 2×** ist die empfohlene Produktionsform; Remote-Runner empfohlen.

Reproduzieren: `AXIOM_TEST_DATABASE_URL=… AXIOM_PROCESSOR_URL=http://127.0.0.1:8012 go
run ./cmd/retrieval-bench -suite cmd/retrieval-bench/gold_suite_v2.json -md out.md`

## v2.1 — Vollbibliothek + VWL/ORG_HA-Traces (#155, 2026-08-16, nach Flutgate)

Suite: `gold_suite_v21.json` — **52 Entries** (v2: 32 + **20 neue verified** aus dudus
Trace-Dateien: 12 VWL aus `quellen_freihandel.txt` — Topic-Keyword-Qualitätsgate über die
alten OpenSearch-Snippets, §17 als thematisch schief verworfen — und 8 ORG_HA aus
`quellennachweise_originalstellen_iteration3.md` — literale verifizierte Blockquotes.
Anker global aufgelöst, Scope = aufgelöstes Dokument; Citation-Familien bestätigt
(Heine/Herr→Paradigmenorientierte, Bofinger→Eine Einführung, Hungenberg→Strategisches
Management, Mankiw/Taylor→Grundzüge …); o9 (Umweltsphären) ehrlich übersprungen (Anker
nicht im Korpus). 2 Läufe: identisch auf 3 Dezimalstellen.

| Konfiguration | P@1 | hit@5 | MRR | hit@10 | p50 | p95 |
|---|---|---|---|---|---|---|
| dense-only | 0.173 | 0.558 | 0.339 | 0.692 | 64 ms | 90 ms |
| hybrid | 0.308 | 0.692 | 0.469 | 0.827 | 71 ms | 150 ms |
| **hybrid+rerank** | **0.615** | **0.808** | **0.702** | **0.865** | 4.0–4.5 s | 4.4–4.8 s |
| +sparse | 0.615 | 0.808 | 0.697 | 0.846 | +1.3 s | +3.9 s |
| +sparse+graph | 0.596 | 0.788 | 0.677 | 0.827 | +0.7 s | +0.9 s |

### Lesart
- **Reranker-Urteil bestätigt sich auf der härteren Vollbibliothek**: P@1 verdoppelt
  gegen Hybrid (0.308→0.615), verdreifacht gegen dense-only; MRR 0.469→0.702. Die
  Absolutwerte liegen unter v2 (0.750) — erwartbar: VWL/Technik-Texte mit teils
  Fuzzy-Ankern aus dem alten System sind die härtere Messlatte.
- **Finale Konfiguration (unverändert bestätigt)**: hybrid+rerank, top_n 5–10,
  2× Overfetch, Remote-CUDA für Latenz; Sparse/Graph OFF.
- Die Produktions-Entscheidung aus v2 trägt unverändert.

Reproduzieren: `go run ./cmd/retrieval-bench -suite cmd/retrieval-bench/gold_suite_v21.json -md out.md`
