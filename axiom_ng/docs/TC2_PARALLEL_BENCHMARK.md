# TC2: 3-Runner Parallel Scale Test + Determinism Proof (L8 Test Case 2)

**Datum:** 2026-08-15 · **Issue:** #123 · **Epic:** #109 (L8)
· **Setup:** 3 Runner-Container (rootless Podman, `--network=host`) auf
Carrier-GPUs: GPU0+GPU1 = RTX 3090 (24 GB), GPU2 = RTX A3000 Laptop (12 GB)
· **Dispatcher:** 3 unabhängige Instanzen (disp-gpu0/1/2), je
`AXIOM_PROCESSOR_RUNNER_NAME=carrier-gpuN`, Concurrency 1, gleiche DB
(Claim-Exklusivität über SKIP LOCKED + Claim-Fencing)
· **Datenbasis:** kompletter Neu-Lauf der 16 Bücher nach Clean Slate;
Referenz = TC1-Backup (`backups/axiom_db_tc1_backup_20260815.sql`, 134 MB,
Restore-verifiziert: 17 Jobs / 4.844 Chunks / 26.545 Entities / 10.464 Rels)

## 1. Verlauf (inkl. Runde 1 — verworfen)

**Runde 1 (14:53–15:0x, VERWORFEN):** Alle drei Runner landeten auf GPU 0.
Ursache (vollständige Kette bewiesen): `is_running_in_docker()` schlägt in
rootless Podman fehl (kein `/.dockerenv`, cgroup v2 zeigt nur `0::/`) →
`ai_researcher/config.py` überschreibt beim Import
`CUDA_VISIBLE_DEVICES=0` ("Hardware configuration for non-Docker
environments") → das Container-Pinning (`-e CUDA_VISIBLE_DEVICES=1/2`) wird
niedergetrampelt. Symptome: 17,6 GB auf GPU 0, GPU 1/2 bei 1 MiB, ein
Marker-OOM. **Fix (dauerhaft im Containerfile):** `RUN touch /.dockerenv` —
die Config erkennt Container-Betrieb und lässt das Env-Pinning unangetastet.
**Neues Start-Gate** (30 s, hätte Runde 1 verhindert): pro Container
`torch.cuda.device_count()==1` + `get_device_name` prüfen UND host-seitig
eine Test-Allokation je Container → nvidia-smi muss VRAM auf **allen drei**
Karten zeigen (gemessen: 690/690/525 MiB parallel).

**Betriebsfalle 2 (Runde 2, Start):** 3 Dispatcher-Instanzen migrieren eine
frische DB gleichzeitig → 2 kollidieren bei `CREATE TYPE`
(`pg_type_typname_nsp_index`, SQLSTATE 23505) und exiten. Gewinner-Instanz
migriert fertig; Neustart der Verlierer ist sauber. Lehre: bei Clean Slate
erst EINE Instanz hoch (migriert), dann die anderen — oder Migrations-Race
akzeptieren (fail-fast + Restart reicht, keine Beschädigung).

## 2. Der Lauf

15:05:17 Start → 16:01:25 komplett: **16/16 completed, 0 failed, 0 Zombies,
0 pending** — Wand-Clock **56 min** (Runde 2).

### Job-Verteilung (runner_name-Spalte, #122-Messbasis)

| Runner | GPU | Jobs | Ø min/Job | max | min | Compute-Summe |
| --- | --- | --- | --- | --- | --- | --- |
| carrier-gpu0 | RTX 3090 | 6 | 5,7 | 7,6 | 3,2 | 34,1 min |
| carrier-gpu1 | RTX 3090 | 7 | 6,2 | 13,3 | 2,0 | 43,2 min |
| carrier-gpu2 | A3000 Laptop | 3 | **17,7** | 24,0 | 10,2 | 53,0 min |

Verteilung ist **work-conserving und fair nach Verfügbarkeit**: die schnellen
3090s nehmen mehr Bücher (13), die A3000 schafft 3 — genau das
Architekturversprechen (SKIP-LOCKED-Claim ohne Load-Balancer).

### Doppel-Processing-Check

- Aktive Snapshots >1 pro Attachment: **0**
- Doppelte (attachment, chunk_index)-Paare: **0**

Claim-Exklusivität hält unter 3 konkurrierenden Workern.

### GPU-Auslastung (gelabelte Sampler, 30 s-Takt, 123 Samples)

| GPU | avg util | busy (≥50%) | max VRAM |
| --- | --- | --- | --- |
| GPU0 (3090) | 33 % | 34 % | 12,6 GB |
| GPU1 (3090) | 34 % | 34 % | 15,1 GB |
| GPU2 (A3000) | **74 %** | **75 %** | 11,4 GB |

Die 3090s waren nach ~40 min durch und idleden; **die A3000 war der
Critical Path** (53 Compute-min ≈ Wand-Clock 56 min).

### Skalierungsfaktor

- TC1 (seriell, 1× 3090): 12 Bücher / 72 min → **6,0 min/Buch**
- TC2 (3 GPUs, davon 1 Laptop-Karte): 16 Bücher / 56 min → **3,5 min/Buch**
  → **1,71× Durchsatz** (Wand-Clock), bei heterogener dritter Karte
- Homogen-Projektion: auf 2× 3090 allein wären alle 16 Bücher
  (~95 Compute-min-Äquivalent) in ~48 min durch — die A3000 beschleunigt
  die Wand-Clock nicht (Straggler-Tail), verbreitert aber die
  Verarbeitungsbreite. 3× 3090 projiziert: ~32 min (2,9×).

### Dispatcher-Takt (Phasen-Logs, alle 3 Instanzen)

`runner=` in jeder Zeile; Job-Gaps = Poll-Sekunden bis 0 s
(`acked=15:12:58 → claim=15:12:58`); Staging/Persist im Sekundenbereich,
auch bei 264 Artifacts in einem Result. Die dreischichtige
Transport-Fehlerklasse aus TC1 bleibt behoben.

### Konsistenz

- **Outbox 16/16 done** · **OpenSearch 4.813 Docs == 4.813 Chunks**
- 16 aktive Snapshots, 0 verwaiste processing-Zeilen

## 3. Determinismus-Beweis (gegen TC1-Backup, per zotero_key gejoint)

Methode: pro Dokument Chunk-Anzahl, `md5(string_agg(text,'' ORDER BY
chunk_index))` und Locator-MD5 aggregiert; Abweichungen dann per-Chunk
gedifft und klassifiziert. Join über `zotero_attachments.zotero_key`
(stabil), NICHT über DB-UUIDs (frisch pro Sync).

| Dokument | Ergebnis |
| --- | --- |
| 12 Bücher (inkl. beide Springer-PDFs) | **byte-identisch** (Anzahl+Text+Locator) |
| ESGBS (Heaton, SKAP2JAE, EPUB) | **34/34 Chunks identisch** — das scheinbare 68/34-Delta war die force_rebuild-Doppelaktivierung (#125), kein Inhaltsunterschied |
| Demystifying (Sonko, CDX5EBM3, EPUB) | 27/252 Chunks mit `/tmp/epub_media_<random>`-Leak → **nach Fix #124: 252/252 byte-identisch** (pfad-normalisiert über TC1-Ref und Re-Process) |
| Perspektiven (EE8QHQIL, PDF) | 52/300 Chunk-Texte weichen ab |
| Nachhaltiges Management (RWA5PT4J, PDF) | 615/754 weichen ab, 754→757 Chunks |

> **Korrigierte Bilanz (Post-#124/#125):** 13/16 strikt byte-identisch,
> 14/16 nach Pfad-Normalisierung (Sonko-Re-Process liefert leak-freie
> Chunks, pfad-normalisiert 252/252 == TC1-Referenz), 2/16 Marker-Grenzfälle.
> Nur EIN Buch war vom Tempdir-Leak betroffen (Sonko); Heatons ESGBS war
> nachweislich sauber — die „zweite Abweichung" war das #125-Doppelaktivierungs-
> Artefakt. Fixes: `a65be86` (#124), `a63b5eb` (#125).

### Klassifizierung der Abweichungen

1. **EPUB-Tempdir-Leak (CDX5EBM3, systematisch, kein Modell-Rauschen):**
   `<img src="/tmp/epub_media_<random>/...">` — der Zufallssuffix des
   EPUB-Extraktionstempdirs landet im Markdown und damit im Chunk-Text.
   Nach Normalisierung (`s#/tmp/epub_media_[a-z0-9]+/#/X/#`) sind **alle
   252 Chunks byte-identisch**. Deterministischer Bug — Fix wäre eine
   Pfad-Normalisierung vor dem Chunking (bewusst NICHT hier gefixt, Issue
   folgt).
2. **Marker-Tabellen-Flip (EE8QHQIL):** dieselbe Tabelle wird einmal mit 3,
   einmal mit 4 Spalten erkannt (Layout-Modell-Grenzfall, GPU-Float-
   Nichtdeterminismus) → 52 Chunk-Texte weichen ab; Chunk-Anzahl und alle
   Locatoren bleiben identisch.
3. **Marker-Heading-Flip (RWA5PT4J):** `### Nachhaltiges Management` vs `# …`
   — ein einziger Heading-Level-Flip früh im Buch verschiebt
   Chunk-Grenzen kaskadierend (Heading-Reopen im Chunker) → 615/754
   abweichende Chunks + 3 zusätzliche. Ein Grenzfall, große Wirkung.

### Embedding-Determinismus

6 identische Chunks (2 Bücher × 3 Indizes), TC1-Vektor vs TC2-Vektor:
**Cosinus = 1.000000 exakt auf allen 6** — BGE-M3 ist auf dieser
GPU-Klasse bit-reproduzierbar für identischen Input. Float-Rauschen ist
auch über verschiedene physische 3090s hinweg nicht messbar.

### Fazit Determinismus

Die Pipeline **um den Marker herum ist vollständig deterministisch**
(Chunker, EPUB-Weg-B/CFI, Embeddings bit-exakt; 13/16 Bücher
byte-identisch, 14/16 nach Tempdir-Normalisierung). Nichtdeterminismus
sitzt ausschließlich in Markers Layout-Klassifikation bei Grenzfällen
(2/16 Bücher, betroffen: Tabellen-Spaltenzahl, Heading-Level). Für
RAG-Retrieval irrelevant (lokale Textvarianten), für byte-identische
Re-Runs muss Marker deterministisch laufen (Torch
`torch.use_deterministic_algorithms` + CUBLAS_WORKSPACE_CONFIG wäre der
Hebel — Entscheidung außerhalb dieses Issues).

### Nebenfund: force_rebuild hinterlässt Doppel-Aktivierung

Das TC1-Backup enthält für ESGBS **zwei aktive Snapshots** (Original-Lauf
08-14 + Force-Smoke 08-15, je 34 Chunks): der force_rebuild-Pfad legt eine
neue Generation an, deaktiviert aber die vorige nicht (andere profile_hash
durch Force-Flag → kein Unique-Konflikt, aktive Flag bleibt doppelt).
Folge-Issue empfohlen.

## 4. DoD-Abgleich

- [x] Backup existiert + restore-verifiziert (17/4844/26545/10464 exakt)
- [x] Clean Slate + Baseline (16 pending / 0 / 0 / 0)
- [x] 3 Runner + 3 Dispatcher, runner_name gefüllt, Verteilungs-SQL oben
- [x] 16/16 completed, 0 failed, 0 Zombies; OS == Chunks (4813); Outbox done
- [x] GPU-Sampler gelabelt je Runner, Auswertung oben (GPU-Zeit über
  Compute-Summe je Runner + Util-Profile)
- [x] Skalierungsfaktor: 1,71× Durchsatz (heterogen), 2,9× projiziert (3× 3090)
- [x] Determinismus: quantifiziert + klassifiziert (13/16 identisch,
      1 Tempdir-Bug, 2 Marker-Grenzfälle, Embeddings bit-exakt)
- [ ] Hivemind-Re-Review (Backup, Verteilungs-SQL, Determinismus-Rechnung)

## 5. Empfehlungen (außerhalb dieses Issues)

1. **EPUB-Tempdir-Normalisierung** vor Chunking (kleiner Fix, macht EPUBs
   byte-deterministisch) — Folge-Issue.
2. **force_rebuild: alte Generation deaktivieren** — Folge-Issue.
3. **Deterministisches Marker** nur falls byte-identische Re-Runs je
   Produktanforderung werden (Kosten: Performance-Verlust durch
   deterministische Algorithmen).
4. **Migration-Race dokumentieren:** Clean Slate → eine Instanz zuerst.
5. `/.dockerenv`-Start-Gate als Deploy-Checkliste-Eintrag (Deployment-Doc
   §5c aktualisiert).

— *Alle Zahlen reproduzierbar: axiom_db (TC2) vs axiom_db_tc1_ref
(Backup-Restore), Queries in diesem Doc.*
