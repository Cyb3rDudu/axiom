# Mass-Chunking-Benchmark

**Berichtstyp:** Messbericht (datiert) · **Datum:** 2026-08-14 · **Kontext:**
Produktions-DB-Aufbau (komplette Zotero-Lib, 16 Dokumente) über einen externen
GPU-Runner · Original: `axiom_ng/docs/MASS_CHUNKING_BENCHMARK.md`.

> **Env-Namens-Hinweis:** Zur Laufzeit hießen die Dispatcher-Variablen noch
> `AXIOMNG_*`; seit dem Rename sind es `AXIOM_*` (z. B. `AXIOMNG_PROCESSOR_URL` →
> `AXIOM_PROCESSOR_URL`). Historische Befehle sind hier bewusst nicht umgeschrieben.

> Dieser Bericht dokumentiert den **Systemzustand zum 2026-08-14**. Zahlen bleiben
> als Messungen gültig; Setup-Details sind auf Rollen reduziert.

## Setup

- Dispatcher am zentralen Host, Runner-Container auf einem externen GPU-Host
  (RTX-3090-Klasse, CUDA-Container), Concurrency=1, Lease 5m (Defaults unverändert).
- Quell-Lieferung damals über eine rsync-Brücke (Staging der Zotero-KEY-Ordner auf
  den Runner-Host). **Heute abgelöst** durch die `source_url`-Mechanik
  (HMAC-signierte Download-URL, siehe
  [Operations → Deployment](../../operations/deployment.md), §5) — keine Zotero-Kopien
  mehr auf dem GPU-Host.
- Reset vor dem Lauf: DB `DROP SCHEMA public CASCADE`, OS-Index gelöscht,
  ArtifactRoot geleert, Runner-Workroot frisch.

## Ergebnisse

**16/16 completed, 0 fehlgeschlagen, 0 Wiederholungen** (alle `attempt=1`).

### Job-Tabelle (Reihenfolge nach Abschluss; Zeiten aus der DB)

| # | Dokument | Typ | Größe | Dauer (s) |
| --- | --- | --- | --- | --- |
| 1 | nachhaltiges-management-nachhaltigkeit-… | PDF | 19 MB | **323** (Kaltstart: Modell-Load + Triton-JIT) |
| 2 | demystifying-environmental-social-governance-esg | EPUB | 11 MB | 108 |
| 3–15 | diverses (Springer-PDFs, ESG-Investing, Nachhaltigkeit, Life Cycle …) | PDF/EPUB | 2,5–9,3 MB | 84–235 |
| 16 | ganzheitliches-life-cycle-management | PDF | 17 MB | **403** (letzter Job; gespeichertes `started_at` veraltet → s. Hygiene) |

### Gesamt

| Metrik | Wert |
| --- | --- |
| Batch-Gesamtzeit (Wall) | **2.759 s ≈ 46 min** |
| Durchsatz | **~20,9 Dokumente/Stunde** (Concurrency=1) |
| Kalt/Warm | Erster Job 323 s inkl. Modell-Load; warm 84–403 s, Median ~114 s |
| Dispatcher-Overhead | ~4 s Summe über 16 Jobs (started_at(n+1) == completed_at(n)) |
| Größen↔Zeit | grob korrelierend, aber seitenzahl-/bildlastig dominiert |

### Horizontaler Durchstich (nach dem Lauf)

| Ebene | Anzahl | Konsistenz |
| --- | --- | --- |
| ingest_jobs completed | 16 | 0 Fehler, 0 Retries |
| aktive Snapshots | 16 | 1 pro Attachment |
| Chunks | 4.810 | |
| Outbox | 16 done, 0 sonst | Follow-Delta ≤ 1 Poll-Tick nach Job-Completion |
| OpenSearch-Index (`axiom-ng-chunks-v1`) | 4.810 Docs == Chunks | knn_vector-Mapping (1024) |

GPU: VRAM-Fußabdruck ~2,8 GB bei Marker+GLiNER+mREBEL. Unbeaufsichtigter Lauf →
keine nvidia-smi-Serie (Doku-Lücke, bewusst).

## Profil-Befund (wichtig)

Der Zotero-Sync enqueue-d mit dem Dispatcher-Default-Profil
`{"profile":"full-rag-v1"}` — der Claim materialisiert **alle Feature-Booleans als
`false`** (extract_entities/relationships, compute_dense/sparse). Der
Contract-Name „full-rag-v1" schaltet die Features NICHT: Der Runner liest die
expliziten Booleans, nicht den Profilnamen. Folge für diesen Lauf: reine
Marker→Markdown→Chunk→Locator-Pipeline (inkl. Bild-Artifacts und OS-Indexierung
der Texte), **ohne** L4-Embeddings und **ohne** L6-Entities/Relationship.

Für den Voll-RAG-Lauf sind die Booleans explizit zu setzen (entweder
Dispatcher-Profil mit true-Booleans beim Sync oder SQL-Update von
`processing_profile`+`input_snapshot.processing` vor dem Claim).

## Hygiene-/Messbefunde

1. **Job-Reset-SQL vergaß `started_at`:** der vor dem Batch abgebrochene
   Erstversuch hinterließ ein stale `started_at` (Job 16). Reset-Rezept um
   `started_at=NULL` erweitern.
2. `claimed_at` existiert nicht — Messgröße ist `started_at`/`completed_at`.
3. nvidia-smi-Mitschnitt fehlt (unbeaufsichtigter Lauf) — beim Voll-RAG-Lauf
   nachholen.

## Vergleichswerte

| Umgebung | 3-Seiten-PDF | Anmerkung |
| --- | --- | --- |
| Apple MPS (Dispatcher-Host) | 110–160 s | Gate-5/6-Smokes |
| Externer GPU-Host kalt | 150 s | inkl. Downloads+JIT |
| Externer GPU-Host warm | 30 s | POC |
| Bibliotheks-Schnitt | **0,58 s/Seite** | 4.787 Seiten gesamt, 2.759 s Batch |

> Der 3-Seiten-POC (30 s warm) skaliert nicht linear auf ganze Bücher:
> Marker-Layout-Recognition dominiert bei bildlastigen Seiten.

Weiter: [TC2-Parallel-Test](tc2-parallel.md) · [L8-Analyse](l8-durchstich.md) ·
[Messberichte](../benchmarks.md)
