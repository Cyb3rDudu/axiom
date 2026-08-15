# Benchmarks & Analysen

Historische, datierte Messberichte über die Verarbeitungspipeline. Sie
dokumentieren Systemverhalten zu einem bestimmten Zeitpunkt — **keine**
anhaltende Doku des aktuellen Zustands. Kennzahlen sind reale Messungen aus den
jeweiligen Läufen und bleiben als solche gültig.

> **Rahmung:** Jeder Messbericht nennt Datum, Setup und Datenbasis. Konkrete
> Maschinen-/Netz-Details sind auf Rollen (z. B. „GPU-Host") und Platzhalter
> reduziert; die technischen Lehren (Transport, Fencing, GPU-Pinning) sind als
> Betriebsregeln verallgemeinert in [Operations → Deployment](../operations/deployment.md)
> und [Developer Guide](../developer-guide/architecture.md) dokumentiert.

## Messberichte

| Bericht | Datum | Kernaussage |
| --- | --- | --- |
| [L8-Durchstichs-Analyse](benchmarks/l8-durchstich.md) | 2026-08-15 | Horizontaler Durchstich (16/16 Bücher auf 3 GPUs, 1,71× Durchsatz), Quality-Gate-GO, Zwölf-Fallen-Täterkette |
| [TC2: 3-Runner-Parallel-Test & Determinismus](benchmarks/tc2-parallel.md) | 2026-08-15 | Work-conserving Verteilung, Single-Snapshot-Exklusivität, Determinismus um Marker herum |
| [Mass-Chunking-Benchmark](benchmarks/mass-chunking.md) | 2026-08-14 | 16/16 vollständig, 0 Fehler, Durchsatz/Kalt-Warm, Profil-Befund |
| [Chunk-Qualitätsbewertung (Quality Gate)](benchmarks/chunk-quality.md) | 2026-08-15 | Chunk-/Locator-/Entity-/Relation-Qualität, kNN-Suchtest, GO für TC2 |

Die **kanonischen Originale** liegen unter `axiom_ng/docs/`; diese Seiten sind die
für die Site aufbereitete Sicht.

## Verwandt

- [Datenmodell](data-model.md) · [FAQ](faq.md) · [Willkommen](../index.md)
