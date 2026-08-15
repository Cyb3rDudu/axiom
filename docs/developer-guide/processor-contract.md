# PROCESSOR_CONTRACT v1

!!! note "Kanonische Quelle"
    Die kanonische Quelle des Vertrags ist die Datei
    **`axiom_ng/docs/PROCESSOR_CONTRACT.md`** im Repository — sie wird nicht
    verschoben oder umgeschrieben. Diese Site-Seite ist eine
    Zusammenfassung + Verweis; alle für die Implementierung verbindlichen
    Detailregeln stehen in der kanonischen Datei. Im Zweifel zählt der
    kanonische Text, nicht diese Übersicht.

Der Processor-Contract trennt durable Datenbesitz von hardware-/bibliotheks-
spezifischer Dokument-Verarbeitung:

- **`axiom-ng`** besitzt Zotero-Synchronisation, Ingest-Jobs, persistente IDs,
  PostgreSQL/pgvector-Daten, abgeleitete Artefakte, Knowledge-Graph und
  Suchindex-Synchronisation.
- **Ein Document-Processor** führt rechenintensive Extraktion aus (erste
  Implementierung in Python, weil Marker und die ML-Bibliotheken dort die beste
  Hardware-Unterstützung haben).
- **Ein Prozessor besitzt keinen durable Anwendungszustand** und muss durch eine
  andere Implementierung desselben Vertrags ersetzbar sein.

## Vertragsversionen & Endpoints

Alle Endpoints liegen unter `/v1`; jeder Request enthält `contract_version`.
Additive optionale Felder sind innerhalb von v1 erlaubt.

```text
GET    /v1/health
GET    /v1/capabilities
POST   /v1/process
GET    /v1/jobs/{job_id}
GET    /v1/jobs/{job_id}/result
GET    /v1/jobs/{job_id}/artifacts/{artifact_ref}
POST   /v1/jobs/{job_id}/cancel
POST   /v1/jobs/{job_id}/ack
```

Verarbeitung ist asynchron. `POST /v1/process` akzeptiert oder dedupliziert einen
Job und kehrt schnell zurück; langläufige Marker-/Modell-Operationen halten die
Request-Verbindung nicht offen.

## Ownership-Grenze (kurz)

**Der Prozessor MUSS NICHT:**

- direkt Zotero lesen,
- nach PostgreSQL, pgvector, OpenSearch oder in den Knowledge-Graph schreiben,
- den Ingest-Job-Zustand in der axiom-ng-Datenbank ändern,
- vom Zotero gelieferte bibliografische Metadaten ändern,
- durable Kopien der Quell-PDF/EPUB behalten,
- fehlende Metadaten mit einer LLM erfinden.

**Der Prozessor besitzt nur Berechnung:** Quell-Datei lesen (für die Dauer des
Jobs), PDF/EPUB→Markdown, Seiten-/Quell-Lokator-Mapping, strukturbewusstes
Chunking, Dense-/Sparse-Embeddings, Entity-/Relationship-Extraktion, optionale
Bild-/Tabellen-Extraktion, temporäre Dateien bis zur Acknowledgement.

## Kernmechanik

- **Verarbeitungs-Flow:** Ingest-Job → `POST /v1/process` → Prozessor → Resultat
  - Compute-Payload + Artefakte → axiom-ng-Validierung → PostgreSQL/pgvector/
  Graph-Transaktion + durable Artefakt-Speicher + OpenSearch-Outbox → `ack`. Ein
  `ack` erlaubt dem Prozessor erst, temporäre Dateien zu entfernen; `ack` ist
  idempotent und der Default darf axiom-ng-Restart-Recovery nicht verhindern.
- **Idempotenz:** Der `idempotency_key` identifiziert äquivalente Prozessor-Arbeit;
  derselbe akzeptierte Request liefert den bestehenden Prozessor-Job statt Doppel-
  Arbeit. Re-Play nach einem `ack`ed Job antwortet `409/ARTIFACTS_EXPIRED`
  (terminal, nicht-retrybar); Neuberechnung braucht einen frischen
  Idempotency-Key (`force_rebuild`).
- **Provenienz:** Chunk-Provenienz (ref, index, text, locator, section-Hierarchie,
  Paragraph-Indexes, token_count, Embeddings) ist Pflicht, nicht optional — und
  überlebt Prozessor-Ersetzung und Re-Indexing. Für PDFs physische 0-basierte
  Seitenindizes + logische Seitentitel als Strings; für EPUB CFI-Lokator, niemals
  erfundene Seitenzahlen.
- **Validierung vor Persistenz (§14):** Quell-Identität + Hash, eindeutige
  zusammenhängende Chunk-Indizes, eindeutige lokale Refes, alle Referenzen,
  Dense-Vektor-Dimensionen/-Werte, Sparse-Key/Value-Typen, Pflicht-Locators,
  Evidence-Referenzen auf extrahierte Relationship, Results-Counts gegen die
  tatsächlichen Arrays.
- **Fehler:** Terminal-Fehler verwenden stabile maschinenlesbare Codes (z. B.
  `SOURCE_NOT_FOUND`, `SOURCE_HASH_MISMATCH`, `MODEL_UNAVAILABLE`,
  `OUT_OF_MEMORY`, `CHUNKING_FAILED`, `CANCELLED`, `INTERNAL_ERROR`), je mit
  Default-Retrybarkeit.
- **Sicherheit (§18):** Loopback-Bind `127.0.0.1` standardmäßig; erlaubte
  Quell-Roots; Pfad-Traversal-/Regular-File-Reject; niemals Zotero/DB/OS-
  Zugangsdaten an den Prozessor; keine Dokumentvolltexte/-Embeddings/-Secrets in
  Logs standardmäßig.

## Vertrags-Tests (§19)

Jede Prozessor-Implementierung muss dieselbe Black-Box-Suite bestehen:
Health/Capabilities, Idempotenz, bekanntes PDF→Markdown+Chunk, Provenienz-Round-
Trip, Hash-Mismatch-Fail, Referenz-Integrität, Embeddings vs. Capabilities, keine
PostgreSQL/OpenSearch-Writes, Cancellation, ACK-Cleanup+Idempotenz,
Restart-ohne-Fake-Success, keine durable Quell-Kopie, source_url-Delivery und
Replay-after-ACK-Semantik.

## Vollständige Referenz

Den verbindlichen, vollständigen Vertragstext liest du in der kanonischen Datei:

- Repo-Pfad: `axiom_ng/docs/PROCESSOR_CONTRACT.md`
- GitHub: [PROCESSOR_CONTRACT.md](https://github.com/Cyb3rDudu/axiom/blob/main/axiom_ng/docs/PROCESSOR_CONTRACT.md)

Weiter: [axiom_ng_runner (Python)](axiom-ng-runner.md) ·
[axiom_ng (Go)](axiom-ng-go.md) · [Architektur-Übersicht](architecture.md)
