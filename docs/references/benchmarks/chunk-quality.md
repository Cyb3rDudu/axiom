# Chunk-Qualitätsbewertung (Quality Gate vor TC2)

**Berichtstyp:** Messbericht (datiert) · **Datum:** 2026-08-15 · **Datenbasis:**
DB nach L8 Test Case 1 (16/16 completed) · **Scope:** 4.810 Chunks, 4.810
Dense-Embeddings (1024-dim), 26.353 Entities, 55.537 Mentions, 10.382
Relationships, OpenSearch-Index. Original:
`axiom_ng/docs/CHUNK_QUALITY_ASSESSMENT.md`.

> **Charakter:** bewertend, read-only — Pipeline und Daten unverändert gelassen.
> Dieser Bericht dokumentiert den **Systemzustand zum 2026-08-15**.

## Teil 1 — Quantitative Verteilungen

### Chunk-Größen (token_count)

| Kennzahl | Wert |
| --- | --- |
| Chunks | 4.810 |
| Min / P25 / Median / P75 / Max | 1 / 175 / **382** / 730 / 1.199 |
| Mittel | 472,3 |
| Mikro-Chunks < 50 Tokens | 445 (9,2 %) |
| Monster-Chunks > 1.500 Tokens | **0** |

Mikro-Chunk-Autopsie (12 von 445): ausschließlich **Überschriften-Anker** —
Marker emittiert Headings als eigene Blöcke, der Chunker behält sie (z. B.
`#### **1 Rolle von Softwaresystemen…**`, 10 Tokens). Sie sind strukturell
korrekt (section_titles konsistent), aber für Retrieval wertarm — ein reiner
Heading beantwortet keine Frage.

### Chunk-Dichte pro Buch

Spanne **0,50–1,38 Chunks/Seite** — kein pathologisches Over-/Under-Chunking;
die Streuung erklärt sich aus Satzdichte und Abbildungsanteil der Bücher.

### Locator-Vollständigkeit

| Art | Anzahl | Vollständig |
| --- | --- | --- |
| PDF page_span (physical + label) | 4.524 | **100 %** |
| EPUB cfi_start/cfi_end | 286 | **100 %** |
| Gesamt | 4.810 | **100 %** |

### Entity-Qualität

Confidence-Histogramm der 55.537 Mentions (GLiNER-Schwelle ≥ 0,5):

| Bucket | 0,5 | 0,6 | 0,7 | 0,8 | 0,9 | 1,0 |
| --- | --- | --- | --- | --- | --- | --- |
| Mentions | 5.313 | 9.666 | 11.980 | 9.135 | 10.494 | 8.949 |

Top-Entities (Auszug): `unternehmen` (ORG, 1.686), `nachhaltigkeit` (CONCEPT,
1.164), `esg` (697), `deutschland` (LOC, 407), `csr` (333), `sap` (172),
`symrise` (132), `gri` (127), `vaude` (108) — **korrektes Domänen-Vokabular**
einer CSR/ESG-Bibliothek, kein Monopol-Müll. Leichtes Rauschen durch generische
Nomen als Entities (`companies` ORG ×141, `world` LOC ×108).

Mentions pro Entity: Ø 2,12 · One-Hit 71,6 % (18.792) · stabil ≥3 Mentions 3.749.
Der One-Hit-Anteil ist für offenes NER über Fachbücher normal; der stabile Kern
(14 %) trägt den Graphen.

### Relationship-Qualität

10.382 Relationen, **100 % mit Evidence-Chunk(s)**, 168 mREBEL-Typen. Top:
`subclass_of` 2.548 · `part_of` 1.173 · `instance_of` 921 · `facet_of` 915 ·
`country` 409 · `author` 253 · Long Tail bis `taxon_rank` ×1 (Wikipedia-Schema-
Blutung aus dem mREBEL-Training).

Kanten mit **beiden** Enden >1 Mention: 3.183 / 10.236 bewertbaren = **31,1 %**.
`strength` ist über alle Zeilen konstant 0,7 — mREBEL liefert keine Konfidenz,
wir persistieren einen Default → derzeit **keine Diskriminierung** über strength.

## Teil 2 — Inhalts-Stichproben

3 Bücher (DE/EN/EPUB), je 3 zufällige Chunks inspiziert: Texte inhaltlich
abgeschlossen (kein mittendrin abgehackter Satz in 9/9), section_titles-Hierarchie
konsistent. EPUB-Chunks tragen saubere CFIs, PDF-Chunks page_span mit
römischen/arabischen Labels. Kuriosum ohne Folgen: ein 1.197-Token-Chunk ist ein
Literaturverzeichnis-Fließtext (Locator/Section stimmen).

### Locator-Gegenprobe gegen Original-PDF (pymupdf)

3/3 **seiten-exakt** (physical_page_start = 0-basierter pymupdf-Index), keine
Abweichung, auch nicht ±1. Zwei Schein-Misserfolge der ersten Suchrunde waren
Zeilenumbrüche/Kommaposition in der Nadel, nicht Locator-Fehler.

### Entity-Stichprobe

Top-5 sämtlich plausibel getypt. Zufalls-5: `familienunternehmen` ORG ✓, `esg`
CONCEPT ✓, `otto` PERSON ✓ (Zitaturkontext), `ozone depletion potential` CONCEPT ✓,
`kennzahlen` CONCEPT ✓. **0 Fehlklassifikationen vom Typ „Tabelle 3 als PERSON".**

### Relationship-Stichprobe (5 zufällige, Evidence gelesen)

- `grundbedürfnislohn ⊂ lohn`, `polyester ⊂ textiles`, `datenmanagement ⊂
  informationsverarbeitung` — plausibel ✓
- `human cloning ⊂ embryonic stem cell` (beide METHOD), `environmental ⊂ social`
  (Geschwister) — Rauschen ✗

**3/5 plausibel, 2/5 semantisch schief** — konsistent mit den 31 % stabilen
Kanten: Der Roh-Graph ist ein Kandidatenraum, kein fertiges Wissen.

## Teil 3 — OpenSearch

- **Index-Integrität:** Mapping `embedding: knn_vector(1024)` ✓, Doc-Count 4.810 ==
  Chunks ✓. Spot-Check 3 zufällige Docs: text-md5 **byteweise identisch** zur DB,
  token_count exakt, embedding vorhanden, locator korrekt serialisiert.
- **Sparse-Embeddings** liegen nur in Postgres, **nicht** im Index — Hybrid-
  Retrieval braucht später ein zweites Indexfeld (kein Gate-Blocker).

### kNN-Suchtest (5 Queries, DE+EN, BGE-M3 Dense, k=5)

| Query | Ergebnis |
| --- | --- |
| „Was ist CSR-Reporting und welche Standards gibt es dafür?" | Top-5 alle CSR-Bücher; **beantwortet vollständig** |
| „Grundlagen der Stakeholder-Theory nach Freeman" | 4/5 aus *Stakeholder-Management*; **lehrbuch-exakt** |
| „criticism of ESG ratings and rating divergence" | GRI-Kritik, Third-Party-Rating; **on-topic** |
| „Beispiele für nachhaltige Lieferketten in der Textilbranche" | Lieferkette-/Lieferanten-Abschnitte; **beantwortet** (ein wertarmes Literaturverzeichnis-Hit) |
| „Wie funktioniert die Methodik des Life Cycle Assessment?" | 4/5 aus *Life-Cycle-Management*; **Bullseye** |

Score-Band 0,54–0,63 (Cosine). Cross-lingual: EN-Query findet DE-Bücher und
umgekehrt, wo inhaltlich geboten. **Semantischer Müll: null Treffer in 25.**

## Teil 4 — Fazit & Empfehlung

| Dimension | Ampel | Begründung |
| --- | --- | --- |
| Chunk-Größen | 🟢 | Median 382, keine Monster; 9,2 % Heading-Anker |
| Locator | 🟢 | 100 % Abdeckung, 3/3 seiten-exakt gegen Original-PDF |
| Entities | 🟢/🟡 | Domänen-Vokabular korrekt, 0 Fehlklassifikationen; 71,6 % One-Hits normal, generische Nomen leicht rauschig |
| Relationships | 🟡 | 100 % Evidence, aber nur 31 % stabile Kanten, Stichprobe 3/5 plausibel, strength konstant → Kandidatenraum, filtrierbar |
| Retrieval | 🟢 | 5/5 Queries on-topic, cross-lingual, kein Müll — der Härtest besteht |

**Empfehlung: GO für TC2.** Das Herzstück (Dense-Retrieval über Chunks mit
exakten Locators) liefert präzise, buch- und abschnittstreue Ergebnisse; der
Knowledge-Graph ist ein brauchbarer Kandidatenraum sobald Stabilitäts-/
Evidence-Filter greifen — kein Pipeline-Blocker.

**Fundliste nach Schweregrad:** Blocker: keine.

Vor/nach TC2: (1) `strength` ohne Diskriminierung — mREBEL-Konfidenz oder
evidence-basierte Stärke, bis dahin Mention-Stabilitäts-Filter; (2) Sparse-
Embeddings nicht im Index. Nice-to-have: Heading-Mikro-Chunks mergen;
generische Nomen stoppen; Literaturverzeichnis-Chunks downweighten.

Weiter: [TC2-Parallel-Test](tc2-parallel.md) · [Messberichte](../benchmarks.md) ·
[Benchmarks-Übersicht](../benchmarks.md)
