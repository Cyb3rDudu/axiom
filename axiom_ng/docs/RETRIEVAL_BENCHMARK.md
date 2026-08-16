# Retrieval Benchmark (R7, #137)

Gold suite: 25 Queries (DE+EN; Konzept/Fakt/Norm/Autor), **0 von dudu bestätigt — PROVISORISCH** (Implementor-abgeleitet aus den 16 Buchtiteln der Bibliothek; die 5 Quality-Assessment-Queries sind der Startbestand). Suite: `cmd/retrieval-bench/gold_suite.json` (`confirmed`-Flag pro Query).

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

## Offen

- **dudu: Gold-Bestätigung** (25 × `confirmed`-Flip oder Korrektur in `gold_suite.json`) — dann Neulauf; erst danach ist die Reranker-These final beantwortet.
- Rare-Token-Sub-Suite (Normnummern/Akronyme) für das Sparse-Profil.
- GraphCandidates-SQL-Tuning, falls der Arm produktiv werden soll.
