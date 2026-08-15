# Architektur-Übersicht

> Diese Seite ist das Einstiegskapitel für Entwickler. D5 vertieft einzelne
> Bausteine und fügt ein mermaid-Diagramm hinzu; hier steht der Überblick.

axiom-ng ist ein **Zotero-gestütztes Wissenssystem**: eine RAG-Datenpipeline von
der Zotero-Bibliothek über Dokument-Verarbeitung zu einer durchsuchbaren
Wissensbasis aus Chunks, Entitäten und Beziehungen.

## Grundprinzip

```text
Zotero (Quellen + Metadaten)
        │ lokaler Pfad + unveränderlicher Metadaten-Snapshot
        ▼
axiom-ng (Go) — Dispatcher, Leases, Fencing, Outbox, durable Persistenz
        │ HTTP/JSON: POST /v1/process
        ▼
axiom_ng_runner (Python) — nur Compute (Konvertierung, Chunking, ML)
        │ Resultat + Artefakte zurück
        ▼
axiom-ng validiert + persistiert (PostgreSQL/pgvector/Graph, Outbox→OpenSearch)
```

Die zentrale Eigenschaft: **axiom-ng orchestriert, der Python-Runner rechnet, und
allein axiom-ng besitzt durable Zustand.**

## Die drei Ebenen

1. **Go-Orchestrierung (`axiom_ng`)** — Zotero-Sync, Ingest-Jobs, atomare
   Lease-Claims mit Fencing, Retries, Cancellation, persistente IDs, versionierte
   Verarbeitungs-Snapshots, durable abgeleitete Artefakte, Chunks/Embeddings/
   Entitäten/Beziehungen, PostgreSQL/pgvector- und Graph-Schreibpfade, OpenSearch-
   Outbox. [Weiter: axiom_ng (Go)](axiom-ng-go.md)
2. **Python-Compute (`axiom_ng_runner`)** — ein Loopback-HTTP-Prozessor nach
   `PROCESSOR_CONTRACT` v1, mit vendor-ed `compute_core` (Marker-Konvertierung,
   Editor-Kern, BGE-M3-Embedder, GLiNER/mREBEL-Extraktoren). Er besitzt nur
   Berechnung und temporäre Job-Ausgabe, nie durable Anwendungserwartung.
   [Weiter: axiom_ng_runner](axiom-ng-runner.md)
3. **Transport-Regel (Contract v1)** — Dispatcher und Runner tauschen nur den
   HTTP-Vertrag aus (`PROCESSOR_CONTRACT`). Bulk-Flows (Ergebnis-JSON,
   Artifakt-Bodies) brauchen direkte LAN-Erreichbarkeit in beiden Richtungen.
   [Details im Deployment-Kapitel](../operations/deployment.md)

## Die Transport-Grenze (entscheidend)

Der Runner ist **pure Compute** — niemals toucht er Zotero, Postgres,
OpenSearch oder den Graph. Nur der Vertrag überquert die Leitung. Der Dispatcher
verhandelt beim Start die Capabilities und schlägt fehl, wenn der Runner
unerreichbar oder vertragsin kompatibel ist. Bullk-Daten laufen direkt übers LAN,
ein Tunnel taugt nur für die Kontrollebene (dritte Maskierungs-Ebene eines
historischen Transportproblems).

## Invarianten (aus der aufgelösten Work-Order)

- Nur der aktuelle Lease-Besitzer darf einen aktiven Job mutieren.
- Jede Job-Mutation nach dem Claim ist über die Lease-Token gefeucht.
- Ein stale Worker kann eine reclaimten Job weder komplettieren noch failen.
- Keine DB-Transaktion bleibt während CPU-/Modell-Ausführung offen (Go hält nie
  eine Transaktion, während es auf Python wartet).
- Ein Prozessor-Ergebnis ist unvertrauenswürdiger Input, bis Go es vollständig
  validiert hat.
- Ergebnis-Persistenz ist atomar; ein Fehler erhält den vorigen aktiven Snapshot.
- Ein Job wird erst `completed`, wenn Ergebnis + Artefakte durable committet sind.
- Prozessor-ACK erfolgt erst nach dem durable Commit und ist idempotent.
- Original-PDF/EPUB werden in place gelesen und nie durable kopiert.

## Wo weiter?

- Vertrag im Detail: [PROCESSOR_CONTRACT v1](processor-contract.md)
- Dispatcher/Leases/Persistenz: [axiom_ng (Go)](axiom-ng-go.md)
- Python-Runner + bekannte Grenzen: [axiom_ng_runner](axiom-ng-runner.md)
- Betrieb: [Operations → Deployment](../operations/deployment.md)
- Konfiguration + Testing: folgen in D5

Weiter: [axiom_ng (Go)](axiom-ng-go.md) · [axiom_ng_runner (Python)](axiom-ng-runner.md)
