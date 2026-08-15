# L8 Chunk-Qualitätsbewertung (Quality Gate vor TC2)

**Datum:** 2026-08-15 · **Datenbasis:** axiom_db nach L8 Test Case 1 (16/16 completed)
· **Scope:** 4.810 Chunks, 4.810 Dense-Embeddings (1024-dim), 26.353 Entities,
55.537 Mentions, 10.382 Relationships, OpenSearch-Index `axiom-ng-chunks-v1`
· **Charakter:** bewertend, read-only — Pipeline und Daten unverändert gelassen.

Alle Zahlen reproduzierbar gegen `axiom_db` (PSQL-Access wie in
MASS_CHUNKING_BENCHMARK.md); Kern-Queries stehen in den jeweiligen Abschnitten.

---

## Teil 1 — Quantitative Verteilungen

### 1.1 Chunk-Größen (token_count)

| Kennzahl | Wert |
| --- | --- |
| Chunks | 4.810 |
| Min / P25 / Median / P75 / Max | 1 / 175 / **382** / 730 / 1.199 |
| Mittel | 472,3 |
| Mikro-Chunks < 50 Tokens | 445 (9,2 %) |
| Monster-Chunks > 1.500 Tokens | **0** |

Pro Buch bewegen sich die Mediane zwischen 150 (Stakeholder-Management) und
965 (Nachhaltige Nicht-Nachhaltigkeit); Maxima liegen bei ~1.199 (Chunk-Limit).

**Mikro-Chunk-Autopsie** (12 zufällige der 445): ausschließlich
**Überschriften-Anker** — Marker emittiert Headings als eigene Blöcke, der
Chunker behält sie als eigenständige Chunks (z. B. `#### **1 Rolle von
Softwaresystemen für das NH-Management**`, 10 Tokens). 31 davon sind fast leer
(< 20 Zeichen), keines ohne section_titles. Sie sind strukturell korrekt
(section_titles konsistent), aber für Retrieval wertarm — ein reiner Heading
beantwortet keine Frage.

### 1.2 Chunk-Dichte pro Buch

| Buch | Seiten | Chunks | C/Seite | | Buch | Seiten | Chunks | C/Seite |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| CSR Mythen | 227 | 266 | 1,17 | | Institutionelle Anleger | 371 | 460 | 1,24 |
| CSR und Finance | 407 | 469 | 1,15 | | Nachh. Nicht-Nachhaltigkeit | 347 | 174 | 0,50 |
| CSR Innovationsmanagement | 249 | 249 | 1,00 | | Nachhaltiges Management | 773 | 754 | 0,98 |
| CSR und Reporting | 258 | 236 | 0,91 | | Perspektiven Wirtschaftswiss. | 382 | 300 | 0,79 |
| CSR Value Chain Mgmt | 308 | 282 | 0,92 | | Stakeholder-Management | 178 | 181 | 1,02 |
| Demystifying ESG (EPUB) | – | 252 | – | | Adventure of Sust. Perf. | 290 | 183 | 0,63 |
| ESG and Sustainability | 124 | 171 | 1,38 | | ESGBS (EPUB) | – | 34 | – |
| ESG Investing (EN) | 359 | 404 | 1,13 | | LCM | 498 | 395 | 0,79 |

Spanne 0,50–1,38 — kein pathologisches Over-/Under-Chunking; die Streuung
erklärt sich aus Satzdichte und Abbildungsanteil der Bücher.

### 1.3 Locator-Vollständigkeit

| Art | Anzahl | Vollständig |
| --- | --- | --- |
| PDF page_span (physical + label) | 4.524 | **100 %** |
| EPUB cfi_start/cfi_end | 286 | **100 %** |
| Gesamt | 4.810 | **100 %** |

74 Chunks liegen auf physical_page 0 mit Label `C1` (PDF-Deckblätter, korrekt).
Beide EPUBs (Demystifying: 252, ESGBS: 34) tragen CFIs.

### 1.4 Entity-Qualität

Confidence-Histogramm der 55.537 Mentions (GLiNER-Schwelle ≥ 0,5 greift):

| Bucket | 0,5 | 0,6 | 0,7 | 0,8 | 0,9 | 1,0 |
|---|---|---|---|---|---|---|
| Mentions | 5.313 | 9.666 | 11.980 | 9.135 | 10.494 | 8.949 |

Top-Entities (Auszug): `unternehmen` (ORG, 1.686), `nachhaltigkeit` (CONCEPT,
1.164), `esg` (697), `deutschland` (LOC, 407), `csr` (333), `sap` (172),
`symrise` (132), `gri` (127), `vaude` (108) — das ist die **korrekte
Domänen-Vokabular** einer CSR/ESG-Bibliothek, kein Monopol-Müll. Leichtes
Rauschen durch generische Nomen als Entities (`companies` ORG ×141, `world`
LOC ×108).

Mentions pro Entity: Ø 2,12 · One-Hit 71,6 % (18.792) · stabil ≥3 Mentions:
3.749. Der One-Hit-Anteil ist für offenes NER über Fachbücher normal; der
stabile Kern (14 %) trägt den Graphen.

Typverteilung: CONCEPT 10.135 · ORG 6.363 · PERSON 6.060 · LOCATION 1.970 ·
WORK 1.244 · METHOD 581.

### 1.5 Relationship-Qualität

10.382 Relationen, **100 % mit Evidence-Chunk(s)**, 168 mREBEL-Typen.
Top: `subclass_of` 2.548 · `part_of` 1.173 · `instance_of` 921 · `facet_of`
915 · `country` 409 · `author` 253 · … Long Tail bis `taxon_rank` ×1
(Wikipedia-Schema-Blutung aus dem mREBEL-Training).

Kanten mit **beiden** Enden >1 Mention: 3.183 / 10.236 bewertbaren = **31,1 %**.
`strength` ist über alle Zeilen konstant 0,7 — mREBEL liefert keine Konfidenz,
wir persistieren einen Default → derzeit **keine Diskriminierung** über
strength möglich.

---

## Teil 2 — Inhalts-Stichproben

Bücher: **CSR und Reporting** (DE) · **ESG Investing** (EN) · **Demystifying
ESG** (EPUB). Je 3 zufällige Chunks (token_count > 100) inspiziert — Auswahl
von 9:

- DE/EPUB/EN gemischt, 178–1.197 Tokens, Texte inhaltlich abgeschlossen
  (kein mittendrin abgehackter Satz in 9/9), section_titles-Hierarchie jeweils
  konsistent (Buch → Kapitel → Abschnitt).
- EPUB-Chunks tragen saubere CFIs (`epubcfi(/6/14!/4/66)`), PDF-Chunks
  page_span mit römischen/arabischen Labels.
- Kuriosum ohne Folgen: ein 1.197-Token-Chunk im Demystifying-EPUB ist ein
  Literaturverzeichnis-Abschnitt (References-Fließtext) — Locator und Section
  stimmen, inhaltlich Referenz-Fläche.

### Locator-Gegenprobe gegen Original-PDF (pymupdf)

| Chunk | Buch | physical | Nadel | Treffer |
| --- | --- | --- | --- | --- |
| #310 | ESG Investing | 311 | "Travel & Training Fund" | idx 310/311 ✓ |
| #214 | CSR und Reporting | 238 | "Entwicklungsbereich und Innovationstreiber" | idx 238 ✓ exakt |
| #160 | ESG Investing | 141 | PROMESA-Passage | idx 141 ✓ exakt (wortgleich) |

3/3 **seiten-exakt** (physical_page_start = 0-basierter pymupdf-Index), keine
Abweichung, auch nicht ±1. Zwei Schein-Misserfolge der ersten Suchrunde waren
Zeilenumbüche/Kommaposition in der Nadel, nicht Locator-Fehler.

### Entity-Stichprobe (5 Top + 5 Zufällige)

Top-5 sämtlich plausibel getypt (s. o.). Zufalls-5: `familienunternehmen` ORG ✓,
`esg` CONCEPT ✓, `otto` PERSON ✓ (Zitaturkontext), `ozone depletion potential`
CONCEPT ✓, `kennzahlen` CONCEPT ✓. **0 Fehlklassifikationen vom Typ
„Tabelle 3 als PERSON" in der Stichprobe.**

### Relationship-Stichprobe (5 zufällige, Evidence gelesen)

- `grundbedürfnislohn ⊂ lohn` — belegt ✓ plausibel
- `polyester ⊂ textiles` — belegt ✓ plausibel
- `datenmanagement ⊂ informationsverarbeitung` — belegt ✓ plausibel
- `human cloning ⊂ embryonic stem cell` — Rauschen ✗ (beide METHOD, keine
  Subklass-Beziehung)
- `environmental ⊂ social` — Rauschen ✗ (Geschwister, keine Subklassen)

**3/5 plausibel, 2/5 semantisch schief** — konsistent mit den 31 % stabilen
Kanten: Der Roh-Graph ist ein Kandidatenraum, kein fertiges Wissen.

---

## Teil 3 — OpenSearch

### 3.1 Index-Integrität

Mapping: `embedding: knn_vector(1024)` ✓, `text`, `token_count`, `locator`
(nested mit page_span+cfi-Feldern), `section_titles`, IDs. Doc-Count
4.810 == Chunks ✓. Spot-Check 3 zufällige Docs: text-md5 **byteweise identisch**
zur DB, token_count exakt, embedding vorhanden, locator korrekt serialisiert —
3/3.

Sparse-Embeddings liegen nur in Postgres
(`processing_chunk_sparse_embeddings`), **nicht** im Index — Hybrid-Retrieval
braucht dafür später ein zweites Indexfeld (notiert, kein Gate-Blocker).

### 3.2 kNN-Suchtest (5 Queries, DE+EN, BGE-M3 Dense)

Encoding: `BGEM3FlagModel.encode` auf die Query — BGE-M3 Dense ist symmetrisch
und instructionslos, gleicher Encoder wie für die Passages ( dokumentierte
Wahl). `k=5`, reines knn gegen `axiom-ng-chunks-v1`.

**Q1 „Was ist CSR-Reporting und welche Standards gibt es dafür?"** — Top-5
alle CSR-Bücher, 3× aus *CSR und Reporting* selbst: G4-Reportingstandard,
IDW PS 821 (Prüfstandard Nachhaltigkeitsberichte), ISO 26000, EU-Strategie.
→ **beantwortet die Frage vollständig.**

**Q2 „Grundlagen der Stakeholder-Theory nach Freeman"** — 4/5 aus
*Stakeholder-Management…* (Abschnitte 2.1–2.5, „Freemans Ansatz…"), 1× ESG-Buch
(„2.6.5 Stakeholder Theory"). → **Lehrbuch-exakt.**

**Q3 „criticism of ESG ratings and rating divergence"** — GRI-Standards-Kritik,
Third-Party-ESG-Rating-Abschnitt, Reporting-Standards-Fragmente. → **on-topic**
(Streubreite okay, Scores 0,55–0,59).

**Q4 „Beispiele für nachhaltige Lieferketten in der Textilbranche"** —
Lieferkette-/Lieferantenmanagement-Abschnitte, „38.4.2 Transparente, faire und
saubere Lieferkette" (Outdoor/Textil!). Hit #1 ist ein Literaturverzeichnis-
Chunk (wertarm, harmlos). → **beantwortet.**

**Q5 „Wie funktioniert die Methodik des Life Cycle Assessment?"** — 4/5 aus
*Ganzheitliches Life Cycle Management*: „Ökologische Lebensweganalyse",
„Zieldefinition", „Vorgehensweise". → **Bullseye.**

Score-Band 0,54–0,63 (Cosine). Cross-lingual greift: EN-Query findet DE-Bücher
und umgekehrt, wo inhaltlich geboten. **Semantischer Müll: null Treffer in 25
Ergebnissen.**

---

## Teil 4 — Fazit

| Dimension | Ampel | Begründung |
| --- | --- | --- |
| Chunk-Größen | 🟢 | Median 382, keine Monster; 9,2 % Heading-Anker (strukturell korrekt, retrieval-wertarm) |
| Locator | 🟢 | 100 % Abdeckung (page_span + CFI), 3/3 seiten-exakt gegen Original-PDF |
| Entities | 🟢/🟡 | Domänen-Vokabular korrekt, 0 Fehlklassifikationen in Stichprobe; 71,6 % One-Hits normal, generische Nomen leicht rauschig |
| Relationships | 🟡 | 100 % Evidence, aber nur 31 % stabile Kanten, Stichprobe 3/5 plausibel, strength konstant 0,7 → Kandidatenraum, filtrierbar |
| Retrieval | 🟢 | 5/5 Queries on-topic, korrekte Bücher/Abschnitte, cross-lingual, kein Müll — der Härtest besteht |

### Go/No-Go-Empfehlung: **GO für TC2**

Die Daten sind für RAG-Retrieval **gut genug**: Das Herzstück (Dense-Retrieval
über Chunks mit exakten Locators) liefert präzise, buch- und abschnittstreue
Ergebnisse. Der Knowledge-Graph ist ein brauchbarer Kandidatenraum, sobald
Stabilitäts-/Evidence-Filter beim Querying greifen — kein Pipeline-Blocker.

### Fundliste nach Schweregrad

**Blocker:** keine.

**Vor/nach TC2 angehen (nicht blockierend):**

1. `strength` ohne Diskriminierung (konstant 0,7) — mREBEL-Konfidenz oder
   Evidence-basierte Stärke ableiten; bis dahin beim Graph-Querying nach
   Mention-Stabilität der Enden filtern (31 % stabile Kanten sind der Kern).
2. Sparse-Embeddings nicht im OS-Index — Feld + Befüllung für Hybrid-Retrieval,
   wenn Suchqualität über kNN hinaus wächst.

**Nice-to-have (später):**
3. Heading-only-Mikro-Chunks (9,2 %) mit Folgestück verschmelzen — bessere
   Retrieval-Ausbeute pro Index-Doc.
4. Generische Nomen als Entities (`companies`, `world`) — Stoplist oder
   Mindestlängen-/Mentions-Schwelle beim Entity-Onboarding.
5. Literaturverzeichnis-Chunks — optional per section-Signal downweighten.

— *Generiert im L8-Quality-Gate-Arbeitspaket; Zahlen gegen axiom_db Stand
2026-08-15 verifizierbar.*
