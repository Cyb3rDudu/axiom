# Mass-Chunking Benchmark — 2026-08-14

Produktions-DB-Aufbau: komplette Zotero-Lib (16 Dokumente) durch den
Carrier-Runner (RTX 3090, CUDA-Container), Dispatcher auf dem Mac,
Concurrency=1, Lease 5m (Defaults unverändert).

## Setup & Infra-Befunde

| Punkt | Zustand |
| --- | --- |
| Runner | `runner-poc` Container, Podman rootless, CDI (`--device nvidia.com/gpu=all`), RTX 3090 #0 |
| Route Mac→Carrier | **Tailscale** `100.99.105.103:8012`. `192.168.1.2:8012` wird von der NixOS-Firewall (`nixos-fw`, nur 22/3389/5353/53 offen) gedroppt |
| Quell-Lieferung | **rsync-Bridge (Abweichung!)**: 16 Zotero-KEY-Dirs (127 MB) → Carrier `~/Code/runner-poc/zotero-storage/`, Container-Mount **pfad-erhaltend** `-v …:/Users/dudu/Zotero/storage:ro` + `ALLOWED_SOURCE_ROOTS=/Users/dudu/Zotero/storage` — Dispatcher-`local_path` unverändert gültig, Hash-Gate aktiv. sshfs-Read-Mount scheiterte an macOS-sshd-Pubkey-Ablehnung vom Carrier (Debug ausgeblieben, später klären). Staging-Kopie **nach Review löschen** |
| Reset | axiom_db: `DROP SCHEMA public CASCADE` (20 Objekte), Migration beim Start; OS-Index gelöscht; ArtifactRoot geleert; Runner-Workroot frisch (Container-Neustart) |

## Ergebnisse

**16/16 completed, 0 fehlgeschlagen, 0 Wiederholungen** (alle `attempt=1`).

### Job-Tabelle (Reihenfolge nach Abschluss; Zeiten aus der DB)

| # | Dokument | Typ | Größe | Dauer (s) |
| --- | --- | --- | --- | --- |
| 1 | nachhaltiges-management-nachhaltigkeit-… | PDF | 19 MB | **323** (Kaltstart: Modell-Load + Triton-JIT) |
| 2 | demystifying-environmental-social-governance-esg | EPUB | 11 MB | 108 |
| 3 | 978-3-642-39889-6 | PDF | 9,1 MB | 97 |
| 4 | 978-3-642-40015-5 | PDF | 6,7 MB | 185 |
| 5 | 978-3-642-53893-3 | PDF | 4,9 MB | 84 |
| 6 | 978-3-642-54882-6 | PDF | 7,6 MB | 203 |
| 7 | 978-3-642-54917-5 | PDF | 3,2 MB | 95 |
| 8 | 978-3-658-02842-8 | PDF | 3,0 MB | 114 |
| 9 | 978-3-658-04426-8 | PDF | 8,0 MB | 222 |
| 10 | 978-3-658-03600-3 | PDF | 7,2 MB | 208 |
| 11 | nachhaltige-nicht-nachhaltigkeit | PDF | 2,8 MB | 99 |
| 12 | esgbs-the-false-narrative | EPUB | 163 kB | 97 |
| 13 | environmental-social-governance-investing | PDF | 2,5 MB | 187 |
| 14 | environmental-social-and-governance-and-sustainable-development-in-healthcare | PDF | 7,7 MB | 235 |
| 15 | the-adventure-of-sustainable-performance | PDF | 9,3 MB | 95 |
| 16 | ganzheitliches-life-cycle-management | PDF | 17 MB | **403** (letzter Job; gespeichertes `started_at` ist veraltet → s. Hygiene) |

### Gesamt

| Metrik | Wert |
| --- | --- |
| Batch-Gesamtzeit (Wall) | **2.759 s ≈ 46 min** (11:56:30–12:42:29 UTC) |
| Durchsatz | **~20,9 Dokumente/Stunde** (Concurrency=1) |
| Kalt/Warm | Erster Job 323 s inkl. Modell-Load (HF-Volume warm, Triton-JIT enthalten); warm 84–403 s, Median ~114 s |
| Dispatcher-Overhead | ~4 s Summe über 16 Jobs (started_at(n+1) == completed_at(n), lückenlos sequenziell) |
| Größen↔Zeit | grob korrelierend, aber seitenzahl-/bildlastig dominiert (17 MB-Buch: 403 s; 9,3 MB: 95 s) |

### Horizontaler Durchstich (Post-Run)

| Ebene | Anzahl | Konsistenz |
| --- | --- | --- |
| ingest_jobs completed | 16 | 0 Fehler, 0 Retries |
| aktive Snapshots | 16 | 1 pro Attachment |
| Chunks | 4.810 | |
| Dense-Embeddings | **0** | s. Profil-Befund |
| Entities/Mentions/Relationships | **0** | s. Profil-Befund |
| Outbox | 16 done, 0 sonst | Follow-Delta ≤ 1 Poll-Tick (2 s) nach Job-Completion |
| OpenSearch `axiom-ng-chunks-v1` | **4.810 Docs == Chunks** | knn_vector-Mapping (1024) vom ersten Batch-Ensure |

### GPU

RTX 3090 #0 (24 GB). Lauf war unbeaufsichtigt → keine nvidia-smi-Serie
mithrinnen (Doku-Lücke, bewusst). POC-Referenz: ~2,8 GB VRAM bei
Marker+GLiNER+mREBEL; Runner-Logs zeigen Artifakt-Fetches aller Jobs
(image-0000…image-0262) + ACKs vom Dispatcher über Tailscale
(100.79.104.120).

## Profil-Befund (wichtig)

Der Zotero-Sync enqueued mit dem Dispatcher-Default-Profil
`{"profile":"full-rag-v1"}` — der Claim materialisiert **alle Feature-Booleans
als `false`** (extract_entities/relationships, compute_dense/sparse). Der
Contract-Namen „full-rag-v1" schaltet die Features NICHT: Der Runner liest die
expliziten Booleans (`ProcessingOptions`-Defaults false), nicht den
Profil-Namen. Folgen für diesen Lauf: reines Marker→Markdown→Chunk→Locator-
Pipeline (inkl. Bilder-Artifacts und OS-Indexierung der Texte), **ohne** L4-
Embeddings und **ohne** L6-Entities/Relationships.

Für den Voll-RAG-Benchmark (Hiveminds Prüfkriterien: 1024-dim-Embeddings,
Entity-Typenvielfalt) ist ein zweiter Lauf mit explizit gesetzten Booleans
nötig — entweder `AXIOMNG_DISPATCHER_PROFILE` mit true-Booleans beim Sync oder
SQL-Update von `processing_profile`+`input_snapshot.processing` vor dem Claim
(Rezept aus dem L6-Smoke). Entscheidungspunkt im Report.

## Hygiene-/Messbefunde

1. **Job-Reset-SQL vergaß `started_at`**: der vor dem Batch abgebrochene
   Erstversuch hinterließ ein stale `started_at` (Job 16: gespeicherte 3229 s
   statt realer 403 s). Korrigierte Zahl oben; Reset-Rezept um
   `started_at=NULL` erweitern.
2. `claimed_at` existiert nicht — Messgröße ist `started_at`/`completed_at`.
3. nvidia-smi-Mitschnitt fehlt (unbeaufsichtigter Lauf) — beim Voll-RAG-Lauf
   nachholen.

## Vergleichswerte

| Umgebung | 3-Seiten-PDF (ESG) | Anmerkung |
| --- | --- | --- |
| Mac MPS | 110–160 s | Gate-5/6-Smokes |
| Carrier kalt | 150 s | inkl. Downloads+JIT |
| Carrier warm | 30 s | POC |
| Carrier Bibliothek-Schnitt | **0,58 s/Seite** | 4.787 Seiten gesamt (Summe `manifest.source_page_count`), 2.759 s Batch |

*Der 3-Seiten-POC (30 s warm) skaliert nicht linear auf ganze Bücher:
Marker-Layout-Recognition dominiert bei bildlastigen Seiten.*
