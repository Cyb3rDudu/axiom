# TC2: 3-Runner-Parallel-Test & Determinismus-Beweis

**Berichtstyp:** Messbericht (datiert) · **Datum:** 2026-08-15 · **Kontext:**
L8 Test Case 2 · **Datenbasis:** Kompletter Neu-Lauf der 16 Bücher nach Clean
Slate; Referenz = TC1-Backup. Original: `axiom_ng/docs/TC2_PARALLEL_BENCHMARK.md`.

> Dieser Bericht dokumentiert den **Systemzustand zum 2026-08-15**. Zahlen bleiben
> als Messungen gültig; Setup-Details sind auf Rollen reduziert.

## Setup

- **3 Runner-Container** (rootless Podman, `--network=host`) auf GPU-Hosts:
  2× RTX-3090-Klasse (24 GB) + 1× RTX-A3000-Laptop-Klasse (12 GB).
- **Dispatcher:** 3 unabhängige Instanzen, je `AXIOM_PROCESSOR_RUNNER_NAME=<label>`,
  Concurrency 1, gleiche DB (Claim-Exklusivität über `SKIP LOCKED` + Claim-Fencing).

> Runde 1 wurde verworfen (alle Runner auf GPU 0 — siehe L8-Analyse, Falle 10).
> Der dokumentierte Lauf ist Runde 2.

## Der Lauf

Start → komplett: **16/16 completed, 0 failed, 0 Zombies, 0 pending** — Wand-Clock
**56 min**.

### Job-Verteilung (runner_name-Spalte)

| Runner | GPU | Jobs | Ø min/Job | max | min | Compute-Summe |
| --- | --- | --- | --- | --- | --- | --- |
| runner-a | 3090-Klasse | 6 | 5,7 | 7,6 | 3,2 | 34,1 min |
| runner-b | 3090-Klasse | 7 | 6,2 | 13,3 | 2,0 | 43,2 min |
| runner-c | A3000-Klasse | 3 | **17,7** | 24,0 | 10,2 | 53,0 min |

**Work-conserving:** die schnellen Karten nehmen mehr (13), die Laptop-Karte 3 —
genau das Architekturversprechen (`SKIP LOCKED`-Claim ohne Load-Balancer).

### Doppel-Processing-Check

- Aktive Snapshots >1 pro Attachment: **0**
- Doppelte (attachment, chunk_index)-Paare: **0**

Claim-Exklusivität hält unter 3 konkurrierenden Workern.

### GPU-Auslastung (gelabelte Sampler, 30-s-Takt, 123 Samples)

| GPU | avg util | busy (≥50 %) | max VRAM |
| --- | --- | --- | --- |
| 3090 a | 33 % | 34 % | 12,6 GB |
| 3090 b | 34 % | 34 % | 15,1 GB |
| A3000 | **74 %** | **75 %** | 11,4 GB |

Die 3090er waren nach ~40 min durch und idleden; **die Laptop-Karte war der
Critical Path** (53 Compute-min ≈ Wand-Clock 56 min).

### Skalierungsfaktor

- TC1 (seriell, 1× 3090): 12 Bücher / 72 min → **6,0 min/Buch**
- TC2 (3 GPUs, davon 1 Laptop-Karte): 16 Bücher / 56 min → **3,5 min/Buch** →
  **1,71× Durchsatz** (Wand-Clock)
- Homogen-Projektion: 3× 3090 → ~32 min (2,9×). Die Laptop-Karte beschleunigt die
  Wand-Clock nicht, verbreitert aber die Verarbeitungsbreite.

### Konsistenz

- **Outbox 16/16 done** · **OpenSearch 4.813 Docs == 4.813 Chunks**
- 16 aktive Snapshots, 0 verwaiste processing-Zeilen

## Determinismus-Beweis (gegen TC1-Backup, per zotero_key gejoint)

Methode: pro Dokument Chunk-Anzahl, `md5(string_agg(text, '' ORDER BY
chunk_index))` und Locator-MD5 aggregiert; Abweichungen per-Chunk gedifft und
klassifiziert.

| Dokument | Ergebnis |
| --- | --- |
| 12 Bücher (inkl. beide Springer-PDFs) | **byte-identisch** (Anzahl+Text+Locator) |
| ESGBS (Heaton, EPUB) | **34/34 Chunks identisch** — das scheinbare Delta war eine force_rebuild-Doppelaktivierung, nicht Inhalt |
| Demystifying (Sonko, EPUB) | Tempdir-Leak → **nach Fix #124: 252/252 byte-identisch** |
| Perspektiven (PDF) | 52/300 Chunk-Texte weichen ab |
| Nachhaltiges Management (PDF) | 615/754 weichen ab, 754→757 Chunks |

**Korrigierte Bilanz:** 13/16 strikt byte-identisch, 14/16 nach
Pfad-Normalisierung, **2/16 Marker-Grenzfälle**.

### Klassifizierung der Abweichungen

1. **EPUB-Tempdir-Leak:** zufälliger Suffix des EPUB-Extraktionstempdirs landet im
   Markdown. Nach Normalisierung sind alle 252 Chunks byte-identisch. Deterministischer
   Bug — Fix wäre eine Pfad-Normalisierung vor dem Chunking.
2. **Marker-Tabellen-Flip:** dieselbe Tabelle einmal mit 3, einmal mit 4 Spalten
   (Layout-Modell-Grenzfall) → 52 Chunk-Texte weichen ab; Chunk-Anzahl und
   Locatoren bleiben identisch.
3. **Marker-Heading-Flip:** ein Heading-Level-Flip verschiebt Chunk-Grenzen
   kaskadierend (Heading-Reopen im Chunker) → große Wirkung auf einen Grenzfall.

### Embedding-Determinismus

6 identische Chunks (2 Bücher × 3 Indizes), TC1- vs. TC2-Vektor:
**Cosinus = 1.000000 exakt auf allen 6** — BGE-M3 ist auf dieser GPU-Klasse
bit-reproduzierbar für identischen Input; Float-Rauschen über verschiedene
physische Karten nicht messbar.

### Fazit Determinismus

Die Pipeline **um den Marker herum ist vollständig deterministisch** (Chunker,
EPUB-Weg, Embeddings bit-exakt). Nichtdeterminismus sitzt ausschließlich in
Markers Layout-Klassifikation bei Grenzfällen. Für RAG-Retrieval irrelevant; für
byte-identische Re-Runs müsste Marker deterministisch laufen (Entscheidung außerhalb).

## Nebenfund: force_rebuild-Doppelaktivierung

Der force_rebuild-Pfad legt eine neue Generation an, deaktiviert aber die vorige
nicht (andere profile_hash durch Force-Flag → kein Unique-Konflikt). Folge-Issue.

## Empfehlungen (außerhalb dieses Berichts)

1. EPUB-Tempdir-Normalisierung vor dem Chunking (kleiner Fix, macht EPUBs
   byte-deterministisch).
2. force_rebuild: alte Generation deaktivieren.
3. Deterministisches Marker nur falls byte-identische Re-Runs zur
   Produktanforderung werden (Kosten: Performance-Verlust).
4. Migrations-Race dokumentieren: Clean Slate → eine Instanz zuerst.
5. `/dockerenv`-Start-Gate als Deploy-Checkliste-Eintrag.

Weiter: [Mass-Chunking-Benchmark](mass-chunking.md) · [Chunk-Qualität](chunk-quality.md) ·
[Messberichte](../benchmarks.md)
