# axiom-ng

Ein Zotero-gestütztes Wissenssystem, das deine Literatur in eine durchsuchbare
Wissensbasis verwandelt.

```text
Zotero-Bibliothek → Verarbeitungs-Pipeline → durchsuchbare Daten
```

**axiom-ng** organisiert die Verarbeitung mit einer Go-Anwendung (Dispatcher,
Persistenz, Fencing, Outbox), während die eigentliche Rechenarbeit von einem
Python-Compute-Runner erledigt wird. Das Ergebnis ist eine strukturierte,
durchsuchbare Wissensbasis deiner Dokumente, Entitäten und Beziehungen.

## Für wen ist das?

- **Nutzer** — verbinden Zotero, verarbeiten Dokumente, suchen in den Daten.
  → weiter zum [User Guide](user-guide/ingest.md)
- **Entwickler** — verstehen Architektur, Contract, Konfiguration, Testing.
  → weiter zum [Developer Guide](developer-guide/architecture.md)
- **Betreiber** — setzen einen Runner auf, überwachen Jobs, beheben Fehler.
  → weiter zu [Operations](operations/deployment.md)

## Wo geht es weiter?

| Ziel | Pfad |
| --- | --- |
| In 10 Minuten verstehen, was passiert | [Konzept-Tour](get-started/concept-tour.md) |
| Setup und erster Lauf | [Quickstart](get-started/quickstart.md) |
| Systemarchitektur | [Architektur-Übersicht](developer-guide/architecture.md) |
| Runner betreiben | [Deployment](operations/deployment.md) |
| Messungen und Analysen | [Benchmarks & Analysen](references/benchmarks.md) |
| Herkunft, Lizenz, Logo | [Über](about/index.md) |
